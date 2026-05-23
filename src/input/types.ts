export interface InputEntry {
  relPath: string;       // relative to Input/ folder, e.g. 'articles/oauth-rfc.md'
  slug: string;          // filename without .md
  kind: 'article' | 'doc' | 'transcript' | 'note';
  sourceUrl?: string;
  capturedAt: string;   // ISO
  description?: string;  // first non-empty line of content body, if available
  supersededBy?: string; // relPath of the newer version if superseded
}
