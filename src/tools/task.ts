/**
 * Working memory task tools — Phase 5.
 *
 * task_start  — create task, seed context from brain Wiki + atoms
 * task_update — append findings, steps, artifacts, questions
 * task_complete — promote medium/high findings to Raw/curated/ queue, then clear
 * task_get    — read current task state or list active tasks
 */
import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import {
  generateTaskId,
  createTask,
  getTask,
  getTaskState,
  updateTaskMeta,
  deleteTask,
  addStep,
  updateStep,
  addFinding,
  addArtifact,
  addQuestion,
  resolveQuestion,
  listActiveTasks,
  writeSnapshot,
  type StepStatus,
} from '../working/db.js';
import { seedTaskFromVault } from '../working/retrieval.js';
import { promoteTaskToVault } from '../working/promotion.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

// --- Zod schemas ---

const TaskStartSchema = z.object({
  goal: z.string().min(1).max(500),
  constraints: z.array(z.string().max(200)).max(20).optional(),
  plan: z.array(z.string().max(200)).max(50).optional(),
});

const TaskUpdateSchema = z.object({
  task_id: z.string().min(1),
  current_step: z.string().max(200).optional(),
  add_finding: z.object({
    content: z.string().min(1),
    importance: z.enum(['low', 'medium', 'high']).optional(),
    memory_type: z.enum(['semantic', 'episodic', 'procedural']).optional(),
  }).optional(),
  add_step: z.string().max(200).optional(),
  complete_step: z.number().int().positive().optional(),
  fail_step: z.number().int().positive().optional(),
  add_artifact: z.object({
    name: z.string().min(1).max(200),
    reference: z.string().min(1),
  }).optional(),
  add_question: z.string().max(500).optional(),
  resolve_question: z.object({
    id: z.number().int().positive(),
    resolution: z.string().min(1),
  }).optional(),
});

const TaskCompleteSchema = z.object({
  task_id: z.string().min(1),
  final_finding: z.string().max(2000).optional(),
});

const TaskGetSchema = z.object({
  task_id: z.string().optional(),
});

// --- Tool definitions ---

export const taskStartToolDefinition = {
  name: 'task_start',
  description:
    'Start a new working-memory task. Creates an in-session SQLite record, seeds context from brain Wiki pages and memory atoms, and returns a task_id. Use task_update to log findings and task_complete to promote them to the Wiki.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      goal: { type: 'string', description: 'What this task is trying to accomplish' },
      constraints: { type: 'array', items: { type: 'string' }, description: 'Optional constraints or guardrails' },
      plan: { type: 'array', items: { type: 'string' }, description: 'Optional initial plan steps' },
    },
    required: ['goal'],
  },
};

export const taskUpdateToolDefinition = {
  name: 'task_update',
  description: 'Update working-memory task state. Append findings, add/complete steps, record artifacts, or track open questions.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      task_id: { type: 'string', description: 'Task ID returned by task_start' },
      current_step: { type: 'string', description: 'Description of what is happening right now' },
      add_finding: {
        type: 'object',
        properties: {
          content: { type: 'string' },
          importance: { type: 'string', enum: ['low', 'medium', 'high'] },
          memory_type: { type: 'string', enum: ['semantic', 'episodic', 'procedural'] },
        },
        required: ['content'],
      },
      add_step: { type: 'string', description: 'Add a new pending step' },
      complete_step: { type: 'number', description: 'Mark a step ID as completed' },
      fail_step: { type: 'number', description: 'Mark a step ID as failed' },
      add_artifact: {
        type: 'object',
        properties: { name: { type: 'string' }, reference: { type: 'string' } },
        required: ['name', 'reference'],
      },
      add_question: { type: 'string', description: 'Record an open question' },
      resolve_question: {
        type: 'object',
        properties: { id: { type: 'number' }, resolution: { type: 'string' } },
        required: ['id', 'resolution'],
      },
    },
    required: ['task_id'],
  },
};

export const taskCompleteToolDefinition = {
  name: 'task_complete',
  description:
    'Mark a task as complete. Promotes medium/high findings to Raw/curated/ and queues them for brain_synthesize, then clears the task from working memory. Call brain_synthesize afterwards to write findings into the Wiki.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      task_id: { type: 'string', description: 'Task ID to complete' },
      final_finding: { type: 'string', description: 'Optional summary finding to add before promoting (importance: high, type: episodic)' },
    },
    required: ['task_id'],
  },
};

export const taskGetToolDefinition = {
  name: 'task_get',
  description: 'Get the current state of a working-memory task, or list all active tasks if no task_id is provided.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      task_id: { type: 'string', description: 'Task ID to retrieve (omit to list all active tasks)' },
    },
  },
};

// --- Handlers ---

export async function handleTaskStart(args: unknown): Promise<CallToolResult> {
  try {
    const input = TaskStartSchema.parse(args);
    const task_id = generateTaskId();
    createTask(task_id, input.goal, input.constraints, input.plan);

    if (input.plan) {
      for (const step of input.plan) addStep(task_id, step);
    }

    const { seeded, orphanedWarning } = await seedTaskFromVault(task_id, input.goal);
    logger.info('Task started', { task_id, goal: input.goal, seeded });
    writeSnapshot();

    return {
      content: [{
        type: 'text',
        text: [
          `Task started: ${task_id}`,
          `Goal: ${input.goal}`,
          input.constraints?.length ? `Constraints: ${input.constraints.join(', ')}` : null,
          input.plan?.length ? `Plan: ${input.plan.length} steps` : null,
          seeded > 0 ? `Seeded ${seeded} relevant results from brain (atoms + wiki)` : 'No matching brain context found',
          orphanedWarning ?? null,
        ].filter(Boolean).join('\n'),
      }],
    };
  } catch (err) {
    logger.error('task_start failed', { error: String(err) });
    return { content: [{ type: 'text', text: `Error: ${formatError(err)}` }], isError: true };
  }
}

export async function handleTaskUpdate(args: unknown): Promise<CallToolResult> {
  try {
    const input = TaskUpdateSchema.parse(args);
    const task = getTask(input.task_id);
    if (!task) return { content: [{ type: 'text', text: `Task not found: ${input.task_id}` }], isError: true };

    const changes: string[] = [];

    if (input.current_step !== undefined) {
      updateTaskMeta(input.task_id, { current_step: input.current_step });
      changes.push(`Current step: ${input.current_step}`);
    }
    if (input.add_finding) {
      addFinding(input.task_id, input.add_finding.content, input.add_finding.importance, input.add_finding.memory_type);
      changes.push(`Finding recorded (${input.add_finding.importance ?? 'medium'}, ${input.add_finding.memory_type ?? 'episodic'})`);
    }
    if (input.add_step) {
      const id = addStep(input.task_id, input.add_step);
      changes.push(`Step #${id} added`);
    }
    if (input.complete_step !== undefined) {
      updateStep(input.complete_step, 'completed' as StepStatus);
      changes.push(`Step #${input.complete_step} completed`);
    }
    if (input.fail_step !== undefined) {
      updateStep(input.fail_step, 'failed' as StepStatus);
      changes.push(`Step #${input.fail_step} marked failed`);
    }
    if (input.add_artifact) {
      const id = addArtifact(input.task_id, input.add_artifact.name, input.add_artifact.reference);
      changes.push(`Artifact #${id}: ${input.add_artifact.name}`);
    }
    if (input.add_question) {
      const id = addQuestion(input.task_id, input.add_question);
      changes.push(`Question #${id} added`);
    }
    if (input.resolve_question) {
      resolveQuestion(input.resolve_question.id, input.resolve_question.resolution);
      changes.push(`Question #${input.resolve_question.id} resolved`);
    }

    if (changes.length > 0) writeSnapshot();

    return {
      content: [{
        type: 'text',
        text: changes.length > 0
          ? `Task ${input.task_id} updated:\n${changes.join('\n')}`
          : `Task ${input.task_id}: no changes applied`,
      }],
    };
  } catch (err) {
    logger.error('task_update failed', { error: String(err) });
    return { content: [{ type: 'text', text: `Error: ${formatError(err)}` }], isError: true };
  }
}

export async function handleTaskComplete(args: unknown): Promise<CallToolResult> {
  try {
    const input = TaskCompleteSchema.parse(args);
    const state = getTaskState(input.task_id);
    if (!state) return { content: [{ type: 'text', text: `Task not found: ${input.task_id}` }], isError: true };

    if (input.final_finding) {
      addFinding(input.task_id, input.final_finding, 'high', 'episodic');
      const updated = getTaskState(input.task_id);
      if (updated) Object.assign(state, updated);
    }

    updateTaskMeta(input.task_id, { status: 'completed' });
    const counts = await promoteTaskToVault(state);
    deleteTask(input.task_id);
    writeSnapshot();

    logger.info('Task completed', { task_id: input.task_id, ...counts });

    return {
      content: [{
        type: 'text',
        text: [
          `Task ${input.task_id} completed.`,
          `Goal: ${state.task.goal}`,
          counts.created > 0
            ? `Promoted ${counts.created} finding(s) to Raw/curated/ — call brain_synthesize to write to Wiki.`
            : counts.skipped > 0 ? 'Findings already in vault (SHA256 match).' : 'No promotable findings.',
          'Working memory cleared.',
        ].join('\n'),
      }],
    };
  } catch (err) {
    logger.error('task_complete failed', { error: String(err) });
    return { content: [{ type: 'text', text: `Error: ${formatError(err)}` }], isError: true };
  }
}

export async function handleTaskGet(args: unknown): Promise<CallToolResult> {
  try {
    const input = TaskGetSchema.parse(args);

    if (!input.task_id) {
      const tasks = listActiveTasks();
      if (tasks.length === 0) return { content: [{ type: 'text', text: 'No active tasks.' }] };
      const summary = tasks.map(t => `- ${t.task_id}: ${t.goal} (started ${t.created_at})`).join('\n');
      return { content: [{ type: 'text', text: `Active tasks:\n${summary}` }] };
    }

    const state = getTaskState(input.task_id);
    if (!state) return { content: [{ type: 'text', text: `Task not found: ${input.task_id}` }], isError: true };

    const pending = state.steps.filter(s => s.status === 'pending').length;
    const done = state.steps.filter(s => s.status === 'completed').length;
    const openQs = state.questions.filter(q => !q.resolved).length;

    const lines = [
      `Task: ${state.task.task_id}`,
      `Goal: ${state.task.goal}`,
      `Status: ${state.task.status}`,
      state.task.current_step ? `Current step: ${state.task.current_step}` : null,
      `Steps: ${done} completed, ${pending} pending`,
      `Findings: ${state.findings.length}`,
      `Artifacts: ${state.artifacts.length}`,
      `Open questions: ${openQs}`,
    ];

    if (state.findings.length > 0) {
      lines.push('\nFindings:');
      for (const f of state.findings) {
        lines.push(`  [${f.importance}/${f.memory_type ?? 'episodic'}] ${f.content.slice(0, 120)}`);
      }
    }

    return { content: [{ type: 'text', text: lines.filter(Boolean).join('\n') }] };
  } catch (err) {
    logger.error('task_get failed', { error: String(err) });
    return { content: [{ type: 'text', text: `Error: ${formatError(err)}` }], isError: true };
  }
}
