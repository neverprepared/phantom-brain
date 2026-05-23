import { z } from 'zod';
import { LifecycleStatusSchema, ConfidenceSchema, SourceSchema, StatusSchema } from './frontmatter.js';

export const FreshnessFilterSchema = z.enum(['all', 'fresh', 'stale']);
export type FreshnessFilter = z.infer<typeof FreshnessFilterSchema>;

export const TagModeSchema = z.enum(['and', 'or']).default('and');
export type TagMode = z.infer<typeof TagModeSchema>;

export const StoreInputSchema = z.object({
  title: z.string().min(1).max(200),
  content: z.string().min(1),
  lifecycle_status: LifecycleStatusSchema,
  tags: z.array(z.string().max(50)).max(50).default([]),
  related: z.array(z.string().max(60)).max(50).default([]),
  confidence: ConfidenceSchema.default('medium'),
  source: SourceSchema.default('conversation'),
  source_urls: z.array(z.string().url()).max(20).default([]),
  ttl_days: z.number().nonnegative().optional(),
  deadline: z.string().optional(),
});
export type StoreInput = z.infer<typeof StoreInputSchema>;

export const RecallInputSchema = z.object({
  id: z.string().optional(),
  title: z.string().optional(),
}).refine(
  (data) => data.id || data.title,
  { message: 'Either id or title must be provided' }
);
export type RecallInput = z.infer<typeof RecallInputSchema>;

export const SearchModeSchema = z.enum(['auto', 'keyword', 'vector']).default('auto');
export type SearchMode = z.infer<typeof SearchModeSchema>;

export const SearchInputSchema = z.object({
  query: z.string().optional(),
  tags: z.array(z.string()).optional(),
  tag_mode: TagModeSchema,
  exclude_tags: z.array(z.string()).optional(),
  lifecycle_status: LifecycleStatusSchema.optional(),
  status: StatusSchema.optional(),
  freshness: FreshnessFilterSchema.default('all'),
  sort_by: z.enum(['relevance', 'created', 'updated', 'title']).default('relevance'),
  limit: z.number().min(1).max(100).default(10),
  include_archived: z.boolean().default(false),
  search_mode: SearchModeSchema,
  created_after: z.string().optional(),
  created_before: z.string().optional(),
  updated_after: z.string().optional(),
  updated_before: z.string().optional(),
});
export type SearchInput = z.infer<typeof SearchInputSchema>;


export const UpdateInputSchema = z.object({
  id: z.string(),
  title: z.string().min(1).max(200).optional(),
  content: z.string().optional(),
  tags: z.array(z.string().max(50)).max(50).optional(),
  add_tags: z.array(z.string().max(50)).max(50).optional(),
  related: z.array(z.string().max(60)).max(50).optional(),
  confidence: ConfidenceSchema.optional(),
  status: StatusSchema.optional(),
  source_urls: z.array(z.string().url()).max(20).optional(),
  ttl_days: z.number().nonnegative().optional(),
});
export type UpdateInput = z.infer<typeof UpdateInputSchema>;


export const DeleteInputSchema = z.object({
  id: z.string(),
  confirm: z.boolean(),
}).refine(
  (data) => data.confirm === true,
  { message: 'confirm must be true to delete a memory' }
);
export type DeleteInput = z.infer<typeof DeleteInputSchema>;

export const LinkInputSchema = z.object({
  source_id: z.string(),
  target_id: z.string().optional(),
  discover: z.boolean().default(false),
  depth: z.number().int().min(1).max(5).optional(),
}).refine(
  (data) => data.discover || data.target_id,
  { message: 'Either target_id or discover: true must be provided' }
);
export type LinkInput = z.infer<typeof LinkInputSchema>;

export const TaskStartInputSchema = z.object({
  goal: z.string().min(1).max(500),
  constraints: z.array(z.string().max(200)).max(20).optional(),
  plan: z.array(z.string().max(200)).max(50).optional(),
});
export type TaskStartInput = z.infer<typeof TaskStartInputSchema>;

export const TaskUpdateInputSchema = z.object({
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
export type TaskUpdateInput = z.infer<typeof TaskUpdateInputSchema>;

export const TaskCompleteInputSchema = z.object({
  task_id: z.string().min(1),
  final_finding: z.string().max(2000).optional(),
});
export type TaskCompleteInput = z.infer<typeof TaskCompleteInputSchema>;

export const TaskGetInputSchema = z.object({
  task_id: z.string().optional(),
});
export type TaskGetInput = z.infer<typeof TaskGetInputSchema>;

export const TimelineInputSchema = z.object({
  after: z.string().optional().describe('ISO date — only show activity after this date'),
  before: z.string().optional().describe('ISO date — only show activity before this date'),
  activity: z.enum(['created', 'updated', 'accessed']).default('updated').describe('Which timestamp to use for ordering'),
  lifecycle_status: LifecycleStatusSchema.optional(),
  tags: z.array(z.string()).optional(),
  group_by: z.enum(['day', 'week', 'none']).default('day'),
  limit: z.number().min(1).max(100).default(30),
});
export type TimelineInput = z.infer<typeof TimelineInputSchema>;

export const CleanupInputSchema = z.object({
  action: z.enum(['list', 'archive', 'delete']).default('list'),
  target: z.enum(['stale', 'archived', 'orphan']).default('stale'),
  dry_run: z.boolean().default(true),
  limit: z.number().min(1).max(50).default(20),
  confirm: z.boolean().default(false),
}).refine(
  (data) => data.action !== 'delete' || data.confirm === true,
  { message: 'confirm must be true when action is delete' }
);
export type CleanupInput = z.infer<typeof CleanupInputSchema>;
