import fs from 'node:fs/promises';
import path from 'node:path';
import { spawn } from 'node:child_process';

const MAX_BYTES = 500 * 1024; // 500 KB cap on extracted text

function spawnCommand(cmd: string, args: string[], timeoutMs: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] });

    let stdout = '';
    let stderr = '';

    const timer = setTimeout(() => {
      child.kill('SIGTERM');
      reject(new Error(`Command timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    child.stdout.on('data', (chunk: Buffer) => {
      if (stdout.length < MAX_BYTES) stdout += chunk.toString();
    });
    child.stderr.on('data', (chunk: Buffer) => { stderr += chunk.toString(); });

    child.on('close', (code) => {
      clearTimeout(timer);
      if (code === 0) resolve(stdout.slice(0, MAX_BYTES));
      else reject(new Error(`${path.basename(cmd)} exited with code ${code}: ${stderr.slice(0, 200)}`));
    });

    child.on('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}

async function isCommandAvailable(cmd: string): Promise<boolean> {
  try {
    await spawnCommand('which', [cmd], 5_000);
    return true;
  } catch {
    return false;
  }
}

export async function extractTextFromFile(
  absPath: string,
  ext: string,
): Promise<{ text: string; method: string }> {
  switch (ext) {
    case '.pdf': {
      const text = await spawnCommand('pdftotext', ['-layout', absPath, '-'], 60_000);
      return { text, method: 'pdftotext' };
    }

    case '.docx':
    case '.doc':
    case '.rtf': {
      const text = await spawnCommand('textutil', ['-convert', 'txt', '-stdout', absPath], 60_000);
      return { text, method: 'textutil' };
    }

    case '.jpg':
    case '.jpeg':
    case '.png':
    case '.heic':
    case '.webp':
    case '.gif': {
      if (!(await isCommandAvailable('tesseract'))) {
        return { text: '', method: 'ocr_unavailable' };
      }
      const text = await spawnCommand('tesseract', [absPath, 'stdout'], 120_000);
      return { text, method: 'tesseract' };
    }

    case '.txt':
    case '.md':
    case '.csv':
    case '.json':
    case '.html':
    case '.htm': {
      const raw = await fs.readFile(absPath, 'utf-8');
      return { text: raw.slice(0, MAX_BYTES), method: 'plaintext' };
    }

    default:
      return { text: '', method: 'unsupported' };
  }
}
