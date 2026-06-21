import type { DomainTier } from '../validation/source-tiers.js';
import { domainFromUrl } from '../validation/source-tiers.js';

export interface RelevantAtom {
  id: string;
  title: string;
  excerpt: string;
  confidence: string;
  tags: string[];
}

export interface EvaluationPackage {
  status: 'needs_evaluation';
  layer1: {
    passed: boolean;
    domain_tier: DomainTier;
    is_duplicate: boolean;
    duplicate_id?: string;
    coherence_passed: boolean;
    coherence_reason?: string;
  };
  incoming: {
    content: string;
    source_url?: string;
    domain: string;
    content_hash: string;
  };
  relevant_atoms: RelevantAtom[];
  evaluation_prompt: string;
}

const TOP_12_FALLACIES = `
1. Ad Hominem — attacks the arguer not the argument
2. Appeal to Authority — true only because an authority said so, no supporting evidence
3. False Dichotomy — only two options presented when more exist
4. Hasty Generalization — broad conclusion from small/unrepresentative sample
5. Post Hoc — correlation presented as causation (X then Y, therefore X caused Y)
6. Straw Man — misrepresents an opposing position to attack it
7. Appeal to Emotion — substitutes emotional manipulation for logical reasoning
8. Cherry Picking — uses only confirming data, ignores contradictions
9. Circular Reasoning — conclusion restates a premise (begging the question)
10. Slippery Slope — small step asserted to necessarily cause extreme outcome without justification
11. Appeal to Popularity — true only because many people believe it
12. False Equivalence — treats two dissimilar things as equivalent
`.trim();

const RAZORS = `
Apply in order:
1. Alder/Popper — Is this claim empirically testable? If not, flag as non-empirical.
2. Hitchens — Does it provide evidence? If not, confidence = low.
3. Sagan — Is this extraordinary (contradicts established knowledge)? Raise evidence bar accordingly.
4. Occam — Is there a simpler explanation being ignored?
5. Hanlon — Does this require malicious intent? Require stronger evidence.
6. Hume — Does it jump from facts to normative conclusions without stating the value premise?
`.trim();

export function buildEvaluationPrompt(
  content: string,
  sourceUrl: string | undefined,
  domainTier: DomainTier,
  relevantAtoms: RelevantAtom[],
): string {
  const atomsText = relevantAtoms.length > 0
    ? relevantAtoms.map(a =>
        `[${a.confidence.toUpperCase()}] ${a.title}\n  ${a.excerpt}\n  Tags: ${a.tags.join(', ')}`
      ).join('\n\n')
    : 'No related atoms found in memory.';

  return `You are evaluating an incoming claim before it is stored in long-term memory.

## Incoming Claim
${content}

Source: ${sourceUrl ?? 'none provided'}
Source tier: ${domainTier}

## Existing Knowledge (relevant atoms)
${atomsText}

## Evaluation Steps

**Step 1 — Fallacy scan** (check all 12):
${TOP_12_FALLACIES}

**Step 2 — Razor application**:
${RAZORS}

**Step 3 — Contradiction check**:
Does this conflict with any existing atom above? If so, which ones and how severely?

**Step 4 — Verdict**:
Choose one:
- store: claim is credible, no significant conflicts, assign confidence (low/medium/high)
- ask: uncertain — state your specific question for the user
- reject: claim conflicts with high-confidence knowledge OR exhibits fallacies that undermine it

**Step 5 — Required output** (call brain_commit with these values):
- decision: store | ask | reject
- reasoning: one paragraph explaining your verdict
- confidence: low | medium | high (only matters if decision=store)
- fallacies_detected: [] (list any found, empty if none)
- conflicting_atom_ids: [] (atom IDs that conflict, empty if none)

Do not store anything yet. Call brain_commit with your verdict.`;
}

export function buildEvaluationPackage(params: {
  content: string;
  sourceUrl?: string;
  domainTier: DomainTier;
  isDuplicate: boolean;
  duplicateId?: string;
  coherencePassed: boolean;
  coherenceReason?: string;
  contentHash: string;
  relevantAtoms: RelevantAtom[];
}): EvaluationPackage {
  return {
    status: 'needs_evaluation',
    layer1: {
      passed: params.coherencePassed && !params.isDuplicate,
      domain_tier: params.domainTier,
      is_duplicate: params.isDuplicate,
      ...(params.duplicateId !== undefined && { duplicate_id: params.duplicateId }),
      coherence_passed: params.coherencePassed,
      ...(params.coherenceReason !== undefined && { coherence_reason: params.coherenceReason }),
    },
    incoming: {
      content: params.content,
      ...(params.sourceUrl !== undefined && { source_url: params.sourceUrl }),
      domain: domainFromUrl(params.sourceUrl),
      content_hash: params.contentHash,
    },
    relevant_atoms: params.relevantAtoms,
    evaluation_prompt: buildEvaluationPrompt(
      params.content,
      params.sourceUrl,
      params.domainTier,
      params.relevantAtoms,
    ),
  };
}
