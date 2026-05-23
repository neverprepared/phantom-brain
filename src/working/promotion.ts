/**
 * Phase 5 promotion: write medium/high task findings as a curated raw document
 * and enqueue for brain_synthesize. The gate treats curated sources as trusted
 * (skips LLM evaluation) so findings land in the Wiki without an extra API call.
 *
 * Low-importance findings are skipped — they're ephemeral and don't warrant
 * long-term storage.
 */
import fs from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';
import { appendToLog, writeAtomicFile } from '../vault/filesystem.js';
import { slugFromTitle } from '../vault/naming.js';
import { readProvenance, isHashKnown } from '../vault/provenance.js';
import { enqueueItem, type QueueItem } from '../vault/queue.js';
import { CONFIG } from '../config.js';
import { todayDateString, nowISO } from '../shared/utils.js';
import type { Finding, TaskState } from './db.js';
import { logger } from '../shared/logger.js';

function sha256(content: string): string {
  return createHash('sha256').update(content).digest('hex');
}

function buildFindingsDocument(state: TaskState, findings: Finding[]): string {
  const goal = state.task.goal;
  const completedAt = nowISO();

  let doc = `# Task findings: ${goal}\n\n`;
  doc += `**Task ID:** ${state.task.task_id}  \n`;
  doc += `**Completed:** ${completedAt}  \n\n`;

  const byType: Record<string, Finding[]> = {};
  for (const f of findings) {
    const t = f.memory_type ?? 'episodic';
    (byType[t] ??= []).push(f);
  }

  for (const [type, group] of Object.entries(byType)) {
    doc += `## ${type.charAt(0).toUpperCase() + type.slice(1)} findings\n\n`;
    for (const f of group) {
      const importance = f.importance !== 'medium' ? ` *(${f.importance})*` : '';
      doc += `- ${f.content}${importance}\n`;
    }
    doc += '\n';
  }

  return doc;
}

export async function promoteTaskToVault(state: TaskState): Promise<{ created: number; appended: number; skipped: number }> {
  const counts = { created: 0, appended: 0, skipped: 0 };

  const promotable = (state.findings as Finding[]).filter(f => f.importance !== 'low');
  if (promotable.length === 0) {
    logger.info('No promotable findings (all low importance)', { task_id: state.task.task_id });
    await appendToLog(`task-complete: no promotable findings for ${state.task.task_id}`);
    return counts;
  }

  const content = buildFindingsDocument(state, promotable);
  const contentHash = sha256(content);

  const provenance = await readProvenance();
  if (await isHashKnown(contentHash, provenance)) {
    logger.info('Task findings already in vault (SHA256 match)', { task_id: state.task.task_id });
    counts.skipped = promotable.length;
    return counts;
  }

  const title = `Task findings: ${state.task.goal.slice(0, 60)}`;
  const slug = slugFromTitle(title);
  const date = todayDateString();
  const curatedDir = path.join(CONFIG.VAULT_PATH, CONFIG.RAW_CURATED);
  await fs.mkdir(curatedDir, { recursive: true });

  let candidate = `${date}-${slug}`;
  let counter = 2;
  let targetPath = path.join(curatedDir, `${candidate}.md`);
  while (true) {
    try {
      await fs.stat(targetPath);
      candidate = `${date}-${slug}-${counter}`;
      targetPath = path.join(curatedDir, `${candidate}.md`);
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') break;
      throw err;
    }
  }

  const rawPathRel = path.posix.join(CONFIG.RAW_CURATED, `${candidate}.md`);
  await writeAtomicFile(targetPath, content);

  const item: QueueItem = {
    raw_path: rawPathRel,
    source: 'curated',
    captured_at: nowISO(),
    title,
    format: 'markdown',
    content_hash: contentHash,
  };
  await enqueueItem(item);

  counts.created = promotable.length;

  logger.info('Task findings promoted to vault queue', {
    task_id: state.task.task_id,
    raw_path: rawPathRel,
    findings: counts.created,
  });

  await appendToLog(`task-promoted: ${state.task.task_id} → ${rawPathRel} (${counts.created} findings)`);

  return counts;
}
