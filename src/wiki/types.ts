export type WikiSubfolder = 'HowTos' | 'Runbooks' | 'References' | 'Scratch';

export interface WikiEntry {
  relPath: string;       // relative to Wiki/ folder, e.g. 'HowTos/oauth-setup.md'
  subfolder: WikiSubfolder | string;
  slug: string;          // filename without .md
  title: string;
  kind: 'howto' | 'runbook' | 'reference' | 'scratch';
  tags: string[];
  created: string;
  updated: string;
  sources: string[];     // atom slugs and Input/ paths cited
}
