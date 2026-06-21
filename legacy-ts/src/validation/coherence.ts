// Basic structural checks — no LLM, fast pass/fail
export function checkCoherence(content: string): { passed: boolean; reason?: string } {
  const trimmed = content.trim();
  if (trimmed.length < 10) return { passed: false, reason: 'Content too short to be a coherent claim' };
  if (trimmed.length > 50_000) return { passed: false, reason: 'Content exceeds maximum size' };
  // Reject if it's pure whitespace/punctuation
  if (!/[a-zA-Z]{3,}/.test(trimmed)) return { passed: false, reason: 'Content contains no meaningful words' };
  return { passed: true };
}
