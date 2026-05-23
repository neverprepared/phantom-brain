/**
 * The Gate — source-level reliability judgment for brain_synthesize.
 *
 * Phase 1 (no LLM):
 *   - Curated sources skip the LLM and return a fixed medium-trust verdict.
 *     Human curation itself is the quality signal.
 *
 * Phase 2 (LLM, gathered sources only):
 *   - Combine the domain tier from `validation/source-tiers.ts` with the
 *     document title, URL, and a content preview, and ask Claude Haiku for
 *     a JSON verdict: {reliability, category?, reason}.
 *   - The call is bounded to 15 seconds and 256 output tokens. Any failure
 *     (no API key, network, parse error, timeout) degrades to a safe medium
 *     fallback with a reason describing why.
 *
 * Contract: runGate NEVER throws. The caller always gets a usable verdict.
 *
 * The previous `src/prompts/evaluate-claim.ts` module targeted claim-level
 * evaluation for the deprecated brain_commit flow; it is intentionally not
 * used here.
 */
import { spawn } from 'node:child_process';
import { CONFIG } from '../config.js';
import { scoreDomain, type DomainTier } from '../validation/source-tiers.js';
import { logger } from '../shared/logger.js';

export type Reliability = 'high' | 'medium' | 'low' | 'contested';
export type GateCategory = 'source' | 'formal' | 'informal' | 'philosophical';

export interface GateVerdict {
  reliability: Reliability;
  category?: GateCategory;  // required when reliability is 'low' or 'contested'
  reason: string;
}

const CONTENT_PREVIEW_CHARS = 800;
const GATE_TIMEOUT_MS = 15_000;

const VALID_RELIABILITIES = new Set<Reliability>(['high', 'medium', 'low', 'contested']);
const VALID_CATEGORIES = new Set<GateCategory>(['source', 'formal', 'informal', 'philosophical']);

const CURATED_VERDICT: GateVerdict = {
  reliability: 'medium',
  reason: 'Curated source — human curation is a quality signal. Phase 2 skipped.',
};

function tierLabel(tier: DomainTier): string {
  switch (tier) {
    case 'authoritative': return 'authoritative';
    case 'credible': return 'credible';
    case 'unknown': return 'unknown';
    case 'low_quality': return 'low quality';
  }
}

function buildGatePrompt(opts: {
  title: string;
  sourceUrl?: string;
  content: string;
  tier: DomainTier;
  source: 'curated' | 'gathered';
}): string {
  const preview = opts.content.slice(0, CONTENT_PREVIEW_CHARS);
  const urlLine = opts.sourceUrl && opts.sourceUrl.length > 0 ? opts.sourceUrl : 'none';
  const curatedLine = opts.source === 'curated' ? 'yes' : 'no';
  return (
    `You are evaluating whether a source document is reliable enough to synthesize into a knowledge wiki.\n` +
    `Evaluate the SOURCE as a whole — not individual claims within it.\n\n` +
    `Source Information:\n` +
    `- Title: ${opts.title}\n` +
    `- URL: ${urlLine}\n` +
    `- Domain tier: ${tierLabel(opts.tier)}\n` +
    `- Curated by human: ${curatedLine}\n` +
    `- Content preview (first 800 chars): ${preview}\n\n` +
    `Evaluation criteria:\n` +
    `- source: domain reputation, primary vs secondary, commercial bias\n` +
    `- formal: invalid deductive structure, logical invalidity\n` +
    `- informal: ad hominem, strawman, false equivalence, appeal to authority, cherry picking\n` +
    `- philosophical: Hitchens razor (requires evidence), Sagan standard (extraordinary claims), Occam (simplest explanation ignored)\n\n` +
    `Reliability tiers:\n` +
    `- high: authoritative domain, primary source, independently corroborated, no detectable bias\n` +
    `- medium: credible domain, secondary source, minor bias, useful with caveats\n` +
    `- low: unknown domain, clear commercial interest, vendor documentation, weak evidence\n` +
    `- contested: contradicts established knowledge, logical fallacies undermine credibility, extraordinary claims without evidence\n\n` +
    `Respond with ONLY valid JSON. No explanation outside the JSON.\n\n` +
    `For high or medium: {"reliability": "high", "reason": "one sentence"}\n` +
    `For low or contested: {"reliability": "low", "category": "source", "reason": "one sentence"}\n` +
    `Valid categories: source | formal | informal | philosophical`
  );
}

/**
 * Extract the first JSON object from an LLM response. Tolerates leading
 * code fences or stray prose.
 */
function extractJson(text: string): unknown {
  const trimmed = text.trim();
  // Fast path — already pure JSON
  try {
    return JSON.parse(trimmed);
  } catch {
    // fall through to brace extraction
  }
  const first = trimmed.indexOf('{');
  const last = trimmed.lastIndexOf('}');
  if (first < 0 || last <= first) {
    throw new Error('No JSON object found in response');
  }
  return JSON.parse(trimmed.slice(first, last + 1));
}

function coerceVerdict(parsed: unknown): GateVerdict {
  if (!parsed || typeof parsed !== 'object') {
    throw new Error('Verdict is not an object');
  }
  const obj = parsed as Record<string, unknown>;
  const reliability = obj['reliability'];
  const reason = obj['reason'];
  const category = obj['category'];

  if (typeof reliability !== 'string' || !VALID_RELIABILITIES.has(reliability as Reliability)) {
    throw new Error(`Invalid reliability: ${String(reliability)}`);
  }
  if (typeof reason !== 'string' || reason.length === 0) {
    throw new Error('Verdict missing reason');
  }

  const rel = reliability as Reliability;
  const verdict: GateVerdict = { reliability: rel, reason };

  if (rel === 'low' || rel === 'contested') {
    // Category is required for low/contested. If missing or invalid, default
    // to "source" — it's the most likely cause and lets the entry stand.
    if (typeof category === 'string' && VALID_CATEGORIES.has(category as GateCategory)) {
      verdict.category = category as GateCategory;
    } else {
      verdict.category = 'source';
    }
  } else if (typeof category === 'string' && VALID_CATEGORIES.has(category as GateCategory)) {
    // Optional for high/medium, but honor it if the model supplied a valid one.
    verdict.category = category as GateCategory;
  }

  return verdict;
}

/**
 * Invoke the `claude` CLI in non-interactive mode, piping the prompt via stdin.
 * Uses the subscription credentials already active in the running Claude Code session —
 * no separate API key required.
 */
function callClaudeCLI(prompt: string, model: string, timeoutMs: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn('claude', ['--print', '--model', model, '--output-format', 'text'], {
      stdio: ['pipe', 'pipe', 'pipe'],
    });

    const timer = setTimeout(() => {
      child.kill('SIGTERM');
      reject(new Error(`Gate CLI call timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    let stdout = '';
    let stderr = '';

    child.stdout.on('data', (chunk: Buffer) => { stdout += chunk.toString(); });
    child.stderr.on('data', (chunk: Buffer) => { stderr += chunk.toString(); });

    child.stdin.write(prompt, 'utf-8');
    child.stdin.end();

    child.on('close', (code) => {
      clearTimeout(timer);
      if (code === 0) {
        resolve(stdout.trim());
      } else {
        reject(new Error(`claude CLI exited with code ${code}: ${stderr.slice(0, 200)}`));
      }
    });

    child.on('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}

/**
 * Run the Gate against a source. Returns a verdict on every code path —
 * any failure degrades to a medium fallback rather than throwing.
 */
export async function runGate(opts: {
  title: string;
  sourceUrl?: string;
  content: string;
  format: 'markdown' | 'html' | 'text' | 'pdf';
  source: 'curated' | 'gathered';
}): Promise<GateVerdict> {
  // Phase 1 — curated sources skip Phase 2.
  if (opts.source === 'curated') {
    return { ...CURATED_VERDICT };
  }

  // Phase 2 toggle — when the gate is disabled, fall back to medium.
  if (!CONFIG.GATE_ENABLED) {
    return {
      reliability: 'medium',
      reason: 'Gate disabled via GATE_ENABLED=false. Defaulting to medium.',
    };
  }

  const tier = scoreDomain(opts.sourceUrl);
  const prompt = buildGatePrompt({
    title: opts.title,
    ...(opts.sourceUrl !== undefined && { sourceUrl: opts.sourceUrl }),
    content: opts.content,
    tier,
    source: opts.source,
  });

  try {
    const text = await callClaudeCLI(prompt, CONFIG.GATE_MODEL, GATE_TIMEOUT_MS);

    if (!text) {
      logger.warn('Gate CLI returned no text content', { format: opts.format });
      return {
        reliability: 'medium',
        reason: 'Gate CLI returned no text content; defaulting to medium.',
      };
    }

    try {
      const parsed = extractJson(text);
      return coerceVerdict(parsed);
    } catch (parseErr) {
      logger.warn('Gate CLI returned unparseable response', {
        error: String(parseErr),
        preview: text.slice(0, 200),
      });
      return {
        reliability: 'medium',
        reason: 'Gate CLI returned unparseable response; defaulting to medium.',
      };
    }
  } catch (err) {
    const msg = String(err);
    const isTimeout = msg.includes('timed out');
    logger.warn('Gate CLI call failed', { error: msg, timeout: isTimeout });
    return {
      reliability: 'medium',
      reason: isTimeout
        ? `Gate CLI call timed out after ${GATE_TIMEOUT_MS / 1000}s; defaulting to medium.`
        : `Gate CLI call failed (${msg}); defaulting to medium.`,
    };
  }
}
