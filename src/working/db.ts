import Database from 'better-sqlite3';
import path from 'node:path';
import fsSync from 'node:fs';
import { nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import { CONFIG } from '../config.js';
import { isProcessAlive } from '../vault/filesystem.js';

export type MemoryType = 'semantic' | 'episodic' | 'procedural';
export type TaskStatus = 'active' | 'completed' | 'failed';
export type StepStatus = 'pending' | 'active' | 'completed' | 'failed';
export type Importance = 'low' | 'medium' | 'high';

export interface OrphanedTask {
  pid: number;
  task_id: string;
  goal: string;
  status: string;
}

export interface Task {
  task_id: string;
  goal: string;
  constraints: string | null;
  plan: string | null;
  current_step: string | null;
  status: TaskStatus;
  created_at: string;
  updated_at: string;
}

export interface Step {
  id: number;
  task_id: string;
  description: string;
  status: StepStatus;
  completed_at: string | null;
}

export interface Finding {
  id: number;
  task_id: string;
  content: string;
  importance: Importance;
  memory_type: MemoryType | null;
  created_at: string;
}

export interface Artifact {
  id: number;
  task_id: string;
  name: string;
  reference: string;
  created_at: string;
}

export interface Question {
  id: number;
  task_id: string;
  question: string;
  resolved: number;
  resolution: string | null;
}

export interface TaskState {
  task: Task;
  steps: Step[];
  findings: Finding[];
  artifacts: Artifact[];
  questions: Question[];
}

let db: Database.Database | null = null;

function getWmDbPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER, `wm-${process.pid}.sqlite`);
}

/**
 * Scan the _index/ directory for dead-PID shard files, collect orphaned active
 * tasks from them, delete the shards, and return the list of orphaned tasks.
 */
function collectOrphanedTasks(): OrphanedTask[] {
  const indexDir = path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER);
  fsSync.mkdirSync(indexDir, { recursive: true });

  const orphaned: OrphanedTask[] = [];
  try {
    const allFiles = fsSync.readdirSync(indexDir);
    const shards = allFiles.filter((f) => /^wm-\d+\.sqlite$/.test(f));
    for (const shard of shards) {
      const match = shard.match(/^wm-(\d+)\.sqlite$/);
      const pid = match ? parseInt(match[1]!, 10) : 0;
      if (!pid || pid === process.pid) continue;
      if (!isProcessAlive(pid)) {
        try {
          const shardDb = new Database(path.join(indexDir, shard));
          const tasks = shardDb
            .prepare("SELECT task_id, goal, status FROM tasks WHERE status = 'active'")
            .all() as Array<{ task_id: string; goal: string; status: string }>;
          for (const t of tasks) {
            orphaned.push({ pid, task_id: t.task_id, goal: t.goal, status: t.status });
          }
          shardDb.close();
          fsSync.unlinkSync(path.join(indexDir, shard));
        } catch {
          // Corrupt shard — skip
        }
      }
    }
  } catch {
    // indexDir doesn't exist yet or unreadable
  }
  return orphaned;
}

export function initWorkingDb(): OrphanedTask[] {
  const wmDbPath = getWmDbPath();
  db = new Database(wmDbPath);

  db.exec(`
    DROP TABLE IF EXISTS questions;
    DROP TABLE IF EXISTS artifacts;
    DROP TABLE IF EXISTS findings;
    DROP TABLE IF EXISTS steps;
    DROP TABLE IF EXISTS tasks;

    CREATE TABLE tasks (
      task_id    TEXT PRIMARY KEY,
      goal       TEXT NOT NULL,
      constraints TEXT,
      plan       TEXT,
      current_step TEXT,
      status     TEXT NOT NULL DEFAULT 'active',
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    );

    CREATE TABLE steps (
      id           INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id      TEXT NOT NULL,
      description  TEXT NOT NULL,
      status       TEXT NOT NULL DEFAULT 'pending',
      completed_at TEXT,
      FOREIGN KEY (task_id) REFERENCES tasks(task_id)
    );

    CREATE TABLE findings (
      id          INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id     TEXT NOT NULL,
      content     TEXT NOT NULL,
      importance  TEXT NOT NULL DEFAULT 'medium',
      memory_type TEXT,
      created_at  TEXT NOT NULL,
      FOREIGN KEY (task_id) REFERENCES tasks(task_id)
    );

    CREATE TABLE artifacts (
      id         INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id    TEXT NOT NULL,
      name       TEXT NOT NULL,
      reference  TEXT NOT NULL,
      created_at TEXT NOT NULL,
      FOREIGN KEY (task_id) REFERENCES tasks(task_id)
    );

    CREATE TABLE questions (
      id         INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id    TEXT NOT NULL,
      question   TEXT NOT NULL,
      resolved   INTEGER NOT NULL DEFAULT 0,
      resolution TEXT,
      FOREIGN KEY (task_id) REFERENCES tasks(task_id)
    );
  `);

  const orphaned = collectOrphanedTasks();
  logger.info('Working memory SQLite initialized', { path: wmDbPath, orphaned: orphaned.length });
  return orphaned;
}

/**
 * Idempotent variant: only initializes if no DB exists yet. Use from app
 * lifecycle (server / CLI) to avoid wiping task state when init runs more
 * than once in a process. Tests that need a clean slate should call
 * initWorkingDb() directly.
 */
export function ensureWorkingDb(): OrphanedTask[] {
  if (db) return [];
  return initWorkingDb();
}

function requireDb(): Database.Database {
  if (!db) throw new Error('Working memory DB not initialized — call initWorkingDb() first');
  return db;
}

export function generateTaskId(): string {
  const timestamp = Math.floor(Date.now() / 1000);
  const random = Math.random().toString(36).substring(2, 8);
  return `task_${timestamp}_${random}`;
}

// --- Tasks ---

export function createTask(
  task_id: string,
  goal: string,
  constraints?: string[],
  plan?: string[],
): void {
  const now = nowISO();
  requireDb().prepare(`
    INSERT INTO tasks (task_id, goal, constraints, plan, status, created_at, updated_at)
    VALUES (?, ?, ?, ?, 'active', ?, ?)
  `).run(
    task_id,
    goal,
    constraints ? JSON.stringify(constraints) : null,
    plan ? JSON.stringify(plan) : null,
    now,
    now,
  );
}

export function getTask(task_id: string): Task | undefined {
  return requireDb()
    .prepare('SELECT * FROM tasks WHERE task_id = ?')
    .get(task_id) as Task | undefined;
}

export function listActiveTasks(): Task[] {
  return requireDb()
    .prepare("SELECT * FROM tasks WHERE status = 'active' ORDER BY created_at DESC")
    .all() as Task[];
}

export function updateTaskMeta(
  task_id: string,
  fields: Partial<Pick<Task, 'current_step' | 'plan' | 'status'>>,
): void {
  const sets: string[] = ['updated_at = ?'];
  const values: unknown[] = [nowISO()];

  if (fields.current_step !== undefined) { sets.push('current_step = ?'); values.push(fields.current_step); }
  if (fields.plan !== undefined) { sets.push('plan = ?'); values.push(JSON.stringify(fields.plan)); }
  if (fields.status !== undefined) { sets.push('status = ?'); values.push(fields.status); }

  values.push(task_id);
  requireDb().prepare(`UPDATE tasks SET ${sets.join(', ')} WHERE task_id = ?`).run(...values);
}

export function deleteTask(task_id: string): void {
  const d = requireDb();
  d.prepare('DELETE FROM steps WHERE task_id = ?').run(task_id);
  d.prepare('DELETE FROM findings WHERE task_id = ?').run(task_id);
  d.prepare('DELETE FROM artifacts WHERE task_id = ?').run(task_id);
  d.prepare('DELETE FROM questions WHERE task_id = ?').run(task_id);
  d.prepare('DELETE FROM tasks WHERE task_id = ?').run(task_id);
}

// --- Steps ---

export function addStep(task_id: string, description: string): number {
  const result = requireDb()
    .prepare('INSERT INTO steps (task_id, description, status) VALUES (?, ?, ?)')
    .run(task_id, description, 'pending');
  touchTask(task_id);
  return Number(result.lastInsertRowid);
}

export function updateStep(id: number, status: StepStatus): void {
  const completed_at = (status === 'completed' || status === 'failed') ? nowISO() : null;
  requireDb()
    .prepare('UPDATE steps SET status = ?, completed_at = ? WHERE id = ?')
    .run(status, completed_at, id);
}

// --- Findings ---

export function addFinding(
  task_id: string,
  content: string,
  importance: Importance = 'medium',
  memory_type?: MemoryType,
): number {
  const result = requireDb()
    .prepare('INSERT INTO findings (task_id, content, importance, memory_type, created_at) VALUES (?, ?, ?, ?, ?)')
    .run(task_id, content, importance, memory_type ?? null, nowISO());
  touchTask(task_id);
  return Number(result.lastInsertRowid);
}

export function getPromotableFindings(task_id: string): Finding[] {
  return requireDb()
    .prepare("SELECT * FROM findings WHERE task_id = ? AND importance IN ('medium', 'high') ORDER BY id")
    .all(task_id) as Finding[];
}

// --- Artifacts ---

export function addArtifact(task_id: string, name: string, reference: string): number {
  const result = requireDb()
    .prepare('INSERT INTO artifacts (task_id, name, reference, created_at) VALUES (?, ?, ?, ?)')
    .run(task_id, name, reference, nowISO());
  touchTask(task_id);
  return Number(result.lastInsertRowid);
}

// --- Questions ---

export function addQuestion(task_id: string, question: string): number {
  const result = requireDb()
    .prepare('INSERT INTO questions (task_id, question, resolved) VALUES (?, ?, 0)')
    .run(task_id, question);
  touchTask(task_id);
  return Number(result.lastInsertRowid);
}

export function resolveQuestion(id: number, resolution: string): void {
  requireDb()
    .prepare('UPDATE questions SET resolved = 1, resolution = ? WHERE id = ?')
    .run(resolution, id);
}

// --- Full state ---

export function getTaskState(task_id: string): TaskState | undefined {
  const task = getTask(task_id);
  if (!task) return undefined;

  const d = requireDb();
  return {
    task,
    steps: d.prepare('SELECT * FROM steps WHERE task_id = ? ORDER BY id').all(task_id) as Step[],
    findings: d.prepare('SELECT * FROM findings WHERE task_id = ? ORDER BY id').all(task_id) as Finding[],
    artifacts: d.prepare('SELECT * FROM artifacts WHERE task_id = ? ORDER BY id').all(task_id) as Artifact[],
    questions: d.prepare('SELECT * FROM questions WHERE task_id = ? ORDER BY id').all(task_id) as Question[],
  };
}

// --- Snapshot ---

function snapshotDir(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER);
}

function snapshotPath(): string {
  return path.join(snapshotDir(), `working-memory-${process.pid}.json`);
}

export interface WorkingMemorySnapshot {
  pid: number;
  server_start: string;
  updated_at: string;
  tasks: Array<{
    task_id: string;
    goal: string;
    status: string;
    current_step: string | null;
    created_at: string;
    updated_at: string;
    steps: { total: number; pending: number; completed: number; failed: number };
    findings: { total: number; low: number; medium: number; high: number };
    artifacts: number;
    questions: { total: number; open: number; resolved: number };
  }>;
}

const serverStart = nowISO();

/** Write a snapshot of all active working memory to a JSON file. */
export function writeSnapshot(): void {
  if (!db) return;

  try {
    const tasks = db.prepare("SELECT * FROM tasks WHERE status = 'active' ORDER BY created_at DESC").all() as Task[];

    const snapshot: WorkingMemorySnapshot = {
      pid: process.pid,
      server_start: serverStart,
      updated_at: nowISO(),
      tasks: tasks.map((t) => {
        const steps = db!.prepare('SELECT status FROM steps WHERE task_id = ?').all(t.task_id) as Array<{ status: string }>;
        const findings = db!.prepare('SELECT importance FROM findings WHERE task_id = ?').all(t.task_id) as Array<{ importance: string }>;
        const artifacts = (db!.prepare('SELECT COUNT(*) as n FROM artifacts WHERE task_id = ?').get(t.task_id) as { n: number }).n;
        const questions = db!.prepare('SELECT resolved FROM questions WHERE task_id = ?').all(t.task_id) as Array<{ resolved: number }>;

        return {
          task_id: t.task_id,
          goal: t.goal,
          status: t.status,
          current_step: t.current_step,
          created_at: t.created_at,
          updated_at: t.updated_at,
          steps: {
            total: steps.length,
            pending: steps.filter((s) => s.status === 'pending').length,
            completed: steps.filter((s) => s.status === 'completed').length,
            failed: steps.filter((s) => s.status === 'failed').length,
          },
          findings: {
            total: findings.length,
            low: findings.filter((f) => f.importance === 'low').length,
            medium: findings.filter((f) => f.importance === 'medium').length,
            high: findings.filter((f) => f.importance === 'high').length,
          },
          artifacts,
          questions: {
            total: questions.length,
            open: questions.filter((q) => !q.resolved).length,
            resolved: questions.filter((q) => q.resolved).length,
          },
        };
      }),
    };

    const dir = snapshotDir();
    fsSync.mkdirSync(dir, { recursive: true });
    const tmpPath = snapshotPath() + '.tmp';
    fsSync.writeFileSync(tmpPath, JSON.stringify(snapshot, null, 2));
    fsSync.renameSync(tmpPath, snapshotPath());
  } catch (err) {
    logger.warn('Failed to write working memory snapshot', { error: String(err) });
  }
}

/** Remove this process's snapshot file. Called on graceful shutdown. */
export function cleanupSnapshot(): void {
  try {
    const fp = snapshotPath();
    if (fsSync.existsSync(fp)) {
      fsSync.unlinkSync(fp);
    }
  } catch {
    // Best effort — process is exiting
  }
}

/**
 * Read all working memory snapshots from disk. Filters out stale
 * snapshots from dead processes.
 */
export function readAllSnapshots(): WorkingMemorySnapshot[] {
  const dir = snapshotDir();
  const snapshots: WorkingMemorySnapshot[] = [];

  try {
    const files = fsSync.readdirSync(dir).filter((f) => f.startsWith('working-memory-') && f.endsWith('.json'));

    for (const file of files) {
      try {
        const content = fsSync.readFileSync(path.join(dir, file), 'utf-8');
        const snapshot = JSON.parse(content) as WorkingMemorySnapshot;

        // Check if owning process is still alive
        try {
          process.kill(snapshot.pid, 0);
          snapshots.push(snapshot);
        } catch {
          // Process is dead — clean up stale snapshot
          try { fsSync.unlinkSync(path.join(dir, file)); } catch { /* */ }
        }
      } catch {
        // Corrupt or unreadable — skip
      }
    }
  } catch {
    // Dir doesn't exist yet — no snapshots
  }

  return snapshots;
}

// --- Internal ---

function touchTask(task_id: string): void {
  requireDb()
    .prepare('UPDATE tasks SET updated_at = ? WHERE task_id = ?')
    .run(nowISO(), task_id);
}
