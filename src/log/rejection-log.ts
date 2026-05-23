import fs from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { getVaultPath, CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';

export interface RejectionEntry {
  timestamp: string;
  content_hash: string;
  source_url?: string;
  domain: string;
  domain_tier: string;
  verdict: 'rejected' | 'disputed';
  reasoning: string;
  conflicting_atom_ids: string[];
  fallacies_detected: string[];
}

function rejectionLogPath(): string {
  return path.join(getVaultPath(), CONFIG.LOG_FOLDER, 'rejections.jsonl');
}

export async function appendRejection(entry: RejectionEntry): Promise<void> {
  const logPath = rejectionLogPath();
  try {
    await fs.mkdir(path.dirname(logPath), { recursive: true });
    await fs.appendFile(logPath, JSON.stringify(entry) + '\n', 'utf-8');
  } catch (err) {
    logger.warn('Failed to append rejection', { error: String(err) });
  }
}

export async function queryRejections(query: string, limit = 10): Promise<RejectionEntry[]> {
  const logPath = rejectionLogPath();
  try {
    const raw = await fs.readFile(logPath, 'utf-8');
    const lines = raw.trim().split('\n').filter(Boolean);
    const entries: RejectionEntry[] = [];
    for (const l of lines) {
      try {
        entries.push(JSON.parse(l) as RejectionEntry);
      } catch {
        // Malformed line — skip
      }
    }
    const q = query.toLowerCase();
    const matched = entries.filter(e =>
      e.reasoning.toLowerCase().includes(q) ||
      e.domain.toLowerCase().includes(q) ||
      e.fallacies_detected.some(f => f.toLowerCase().includes(q)) ||
      e.conflicting_atom_ids.some(id => id.toLowerCase().includes(q))
    );
    return matched.slice(-limit).reverse(); // most recent first
  } catch {
    return [];
  }
}

export function hashContent(content: string): string {
  return createHash('sha256').update(content).digest('hex').slice(0, 16);
}
