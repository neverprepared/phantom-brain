export class MemoryError extends Error {
  constructor(
    message: string,
    public readonly code: string,
    public readonly details?: Record<string, unknown>
  ) {
    super(message);
    this.name = 'MemoryError';
    Error.captureStackTrace(this, this.constructor);
  }

  toJSON() {
    return {
      name: this.name,
      message: this.message,
      code: this.code,
      details: this.details,
    };
  }
}

export class NotFoundError extends MemoryError {
  constructor(identifier: string, details?: Record<string, unknown>) {
    super(
      `Memory not found: "${identifier}"`,
      'NOT_FOUND',
      { identifier, ...details }
    );
    this.name = 'NotFoundError';
  }
}

export class DuplicateError extends MemoryError {
  constructor(slug: string, details?: Record<string, unknown>) {
    super(
      `Memory with slug "${slug}" already exists`,
      'DUPLICATE',
      { slug, ...details }
    );
    this.name = 'DuplicateError';
  }
}

export class ValidationError extends MemoryError {
  constructor(message: string, details?: Record<string, unknown>) {
    super(message, 'VALIDATION_ERROR', details);
    this.name = 'ValidationError';
  }
}

export class VaultError extends MemoryError {
  constructor(message: string, details?: Record<string, unknown>) {
    super(message, 'VAULT_ERROR', details);
    this.name = 'VaultError';
  }
}

export function formatError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
