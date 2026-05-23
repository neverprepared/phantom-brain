import slugifyLib from 'slugify';
import { CONFIG, DEFAULT_TTL_DAYS } from '../config.js';

export function generateSlug(title: string): string {
  return slugifyLib(title, {
    lower: true,
    strict: true,
    trim: true,
  }).slice(0, CONFIG.MAX_SLUG_LENGTH);
}

export function generateMemoryId(slug: string): string {
  const timestamp = Math.floor(Date.now() / 1000);
  return `mem_${timestamp}_${slug}`;
}

export function nowISO(): string {
  return new Date().toISOString();
}

export function todayDateString(): string {
  return new Date().toISOString().split('T')[0]!;
}

export function escapeRegex(str: string): string {
  return str.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Returns true if the memory is past its TTL.
 * Uses lifecycle_status for TTL lookup; falls back to legacy para for backward compat.
 */
export function isStale(
  updated: string,
  ttlDays: number | undefined,
  lifecycleStatus?: string,
): boolean {
  const ttl = ttlDays ?? DEFAULT_TTL_DAYS[lifecycleStatus ?? 'reference'] ?? 180;
  const updatedMs = new Date(updated).getTime();
  const expiresMs = updatedMs + ttl * 86_400_000;
  return Date.now() > expiresMs;
}
