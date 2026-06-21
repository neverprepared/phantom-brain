import slugifyLib from 'slugify';
import { CONFIG } from '../config.js';

export function generateSlug(title: string): string {
  return slugifyLib(title, {
    lower: true,
    strict: true,
    trim: true,
  }).slice(0, CONFIG.MAX_SLUG_LENGTH);
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

