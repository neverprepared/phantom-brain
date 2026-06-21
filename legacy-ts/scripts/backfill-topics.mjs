#!/usr/bin/env node
/**
 * One-time backfill: classify existing summary pages into topic buckets
 * by calling the claude CLI, then patch frontmatter in-place.
 *
 * Usage: node scripts/backfill-topics.mjs [--dry-run]
 */
import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { spawn } from 'node:child_process';

const VAULT = process.env['BRAIN_VAULT_PATH']
  ?? `${process.env['HOME']}/workspaces/profiles/personal/obsidian/vaults/personal-memory`;

const SUMMARIES_DIR = join(VAULT, 'Wiki', 'summaries');
const DRY_RUN = process.argv.includes('--dry-run');
const TIMEOUT_MS = 30_000;

const VALID_TOPICS = new Set([
  'agents', 'memory', 'governance', 'tools', 'training',
  'infrastructure', 'knowledge', 'multiagent', 'general',
]);

function buildPrompt(title, preview) {
  return (
    `Classify this document into exactly one topic category.\n\n` +
    `Title: ${title}\n` +
    `Content preview: ${preview.slice(0, 600)}\n\n` +
    `Topic categories:\n` +
    `- agents: AI agents, agentic systems, agent frameworks\n` +
    `- memory: memory systems, context management, RAG\n` +
    `- governance: HITL, autonomy, AI regulation, risk, security\n` +
    `- tools: tool use, function calling, MCP\n` +
    `- training: fine-tuning, synthetic data, model training\n` +
    `- infrastructure: vector DBs, deployment, embeddings, scaling\n` +
    `- knowledge: knowledge management, wikis, note-taking\n` +
    `- multiagent: multi-agent orchestration, swarms\n` +
    `- general: doesn't fit elsewhere\n\n` +
    `Respond with ONLY valid JSON: {"topic": "agents"}\n` +
    `Valid values: agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
  );
}

function callCLI(prompt) {
  return new Promise((resolve, reject) => {
    const child = spawn('claude', ['--print', '--model', 'claude-haiku-4-5-20251001', '--output-format', 'text'], {
      stdio: ['pipe', 'pipe', 'pipe'],
    });
    const timer = setTimeout(() => { child.kill('SIGTERM'); reject(new Error('timeout')); }, TIMEOUT_MS);
    let stdout = '';
    child.stdout.on('data', d => { stdout += d.toString(); });
    child.stdin.write(prompt, 'utf-8');
    child.stdin.end();
    child.on('close', code => {
      clearTimeout(timer);
      if (code === 0) resolve(stdout.trim());
      else reject(new Error(`exit ${code}`));
    });
    child.on('error', err => { clearTimeout(timer); reject(err); });
  });
}

function extractTopic(text) {
  try {
    const first = text.indexOf('{');
    const last = text.lastIndexOf('}');
    if (first < 0 || last <= first) throw new Error('no json');
    const obj = JSON.parse(text.slice(first, last + 1));
    const t = obj['topic'];
    if (typeof t === 'string' && VALID_TOPICS.has(t)) return t;
  } catch { /* fall through */ }
  return null;
}

function patchFrontmatter(content, topic) {
  // Insert `topic: <value>` after the `reliability:` line (or `category:` if present)
  const lines = content.split('\n');
  let insertAfter = -1;
  let inFrontmatter = false;
  let fmClose = -1;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? '';
    if (i === 0 && line === '---') { inFrontmatter = true; continue; }
    if (inFrontmatter && line === '---') { fmClose = i; break; }
    if (inFrontmatter && (line.startsWith('category:') || line.startsWith('reliability:'))) {
      insertAfter = i;
    }
  }

  if (insertAfter < 0 && fmClose > 0) {
    // Fallback: insert before closing ---
    insertAfter = fmClose - 1;
  }
  if (insertAfter < 0) return null; // can't parse

  lines.splice(insertAfter + 1, 0, `topic: ${topic}`);
  return lines.join('\n');
}

async function main() {
  const files = (await readdir(SUMMARIES_DIR)).filter(f => f.endsWith('.md'));
  console.log(`Found ${files.length} summaries in ${SUMMARIES_DIR}\n`);

  for (const filename of files) {
    const filepath = join(SUMMARIES_DIR, filename);
    const content = await readFile(filepath, 'utf-8');

    // Skip if already has topic
    if (/^topic:/m.test(content)) {
      const existing = content.match(/^topic:\s*(.+)$/m)?.[1] ?? '?';
      console.log(`SKIP  ${filename} (already: ${existing})`);
      continue;
    }

    // Extract title from frontmatter
    const titleMatch = content.match(/^title:\s*"?(.+?)"?\s*$/m);
    const title = titleMatch?.[1] ?? filename;

    // Body preview (skip frontmatter block)
    const bodyStart = content.indexOf('---', 3);
    const body = bodyStart > 0 ? content.slice(bodyStart + 3).trim() : content;

    const prompt = buildPrompt(title, body);

    let topic = null;
    try {
      const text = await callCLI(prompt);
      topic = extractTopic(text);
    } catch (err) {
      console.error(`ERROR ${filename}: ${err.message}`);
      continue;
    }

    if (!topic) {
      console.error(`PARSE ${filename}: could not extract valid topic`);
      continue;
    }

    if (DRY_RUN) {
      console.log(`DRY   ${filename} → ${topic}`);
      continue;
    }

    const patched = patchFrontmatter(content, topic);
    if (!patched) {
      console.error(`PATCH ${filename}: frontmatter parse failed`);
      continue;
    }

    await writeFile(filepath, patched, 'utf-8');
    console.log(`OK    ${filename} → ${topic}`);
  }

  console.log('\nDone.');
}

main().catch(err => { console.error(err); process.exit(1); });
