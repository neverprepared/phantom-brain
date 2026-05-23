import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';

let _available: boolean | null = null;

/** Returns true if Ollama is reachable and the embedding model is loaded. Cached after first check. */
export async function isEmbeddingAvailable(): Promise<boolean> {
  if (_available !== null) return _available;
  try {
    const res = await fetch(`${CONFIG.OLLAMA_BASE_URL}/api/tags`, { signal: AbortSignal.timeout(2000) });
    if (!res.ok) { _available = false; return false; }
    const data = await res.json() as { models?: Array<{ name: string }> };
    _available = (data.models ?? []).some((m) => m.name.startsWith(CONFIG.EMBEDDING_MODEL));
    if (!_available) {
      logger.warn('Embedding model not found in Ollama', { model: CONFIG.EMBEDDING_MODEL });
    }
    return _available;
  } catch {
    _available = false;
    logger.info('Ollama not reachable — vector search disabled, keyword search active');
    return false;
  }
}

/** Reset the availability cache (used in tests). */
export function resetEmbeddingAvailabilityCache(): void {
  _available = null;
}

/**
 * Embed a single text string. Returns null if Ollama is unavailable.
 */
export async function embedText(text: string): Promise<number[] | null> {
  if (!(await isEmbeddingAvailable())) return null;
  try {
    const res = await fetch(`${CONFIG.OLLAMA_BASE_URL}/api/embed`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: CONFIG.EMBEDDING_MODEL, input: text }),
      signal: AbortSignal.timeout(10_000),
    });
    if (!res.ok) throw new Error(`Ollama embed HTTP ${res.status}`);
    const data = await res.json() as { embeddings: number[][] };
    return data.embeddings[0] ?? null;
  } catch (err) {
    logger.warn('Embed failed', { error: String(err) });
    return null;
  }
}

/**
 * Embed a batch of texts in a single Ollama API call.
 * Returns an array of embeddings (null entries where the call failed).
 * Processes in chunks of EMBEDDING_BATCH_SIZE to avoid overwhelming Ollama.
 */
export async function embedBatch(texts: string[]): Promise<Array<number[] | null>> {
  if (!(await isEmbeddingAvailable())) return texts.map(() => null);

  const results: Array<number[] | null> = new Array(texts.length).fill(null);
  const batchSize = CONFIG.EMBEDDING_BATCH_SIZE;

  for (let i = 0; i < texts.length; i += batchSize) {
    const chunk = texts.slice(i, i + batchSize);
    try {
      const res = await fetch(`${CONFIG.OLLAMA_BASE_URL}/api/embed`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: CONFIG.EMBEDDING_MODEL, input: chunk }),
        signal: AbortSignal.timeout(30_000),
      });
      if (!res.ok) throw new Error(`Ollama embed HTTP ${res.status}`);
      const data = await res.json() as { embeddings: number[][] };
      for (let j = 0; j < chunk.length; j++) {
        results[i + j] = data.embeddings[j] ?? null;
      }
    } catch (err) {
      logger.warn('Batch embed chunk failed', { offset: i, size: chunk.length, error: String(err) });
      // Leave nulls for this chunk — caller handles gracefully
    }
  }

  return results;
}

/**
 * Build the text to embed for a memory note.
 * Combines title + tags + body for richer semantic signal.
 */
export function buildEmbedText(title: string, tags: string[], body: string): string {
  const tagStr = tags.length > 0 ? `Tags: ${tags.join(', ')}` : '';

  let bodyPart: string;
  if (body.length <= 3000) {
    bodyPart = body;
  } else if (body.length <= 6000) {
    bodyPart = body.slice(0, 4500);
  } else {
    const headings = (body.match(/^#{1,6}\s.+$/gm) ?? []).slice(0, 20).join('\n');
    bodyPart = [
      body.slice(0, 3000),
      headings ? `Headings:\n${headings}` : '',
      body.slice(-1500),
    ].filter(Boolean).join('\n\n');
  }

  return [title, tagStr, bodyPart].filter(Boolean).join('\n\n');
}
