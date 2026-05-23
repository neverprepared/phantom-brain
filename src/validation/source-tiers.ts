export type DomainTier = 'authoritative' | 'credible' | 'unknown' | 'low_quality';

const AUTHORITATIVE = new Set([
  'arxiv.org', 'pubmed.ncbi.nlm.nih.gov', 'nature.com', 'science.org',
  'github.com', 'npmjs.com', 'docs.python.org', 'developer.mozilla.org',
  'developer.apple.com', 'docs.microsoft.com', 'cloud.google.com',
  'aws.amazon.com', 'docs.aws.amazon.com', 'kubernetes.io',
]);

const CREDIBLE = new Set([
  'wikipedia.org', 'britannica.com', 'stackoverflow.com', 'news.ycombinator.com',
  'techcrunch.com', 'wired.com', 'arstechnica.com', 'theverge.com',
  'reuters.com', 'apnews.com', 'bbc.com', 'theguardian.com',
]);

const LOW_QUALITY = new Set<string>([
  'content-farm.com', // extend as needed
]);

export function scoreDomain(url: string | undefined): DomainTier {
  if (!url) return 'unknown';
  try {
    const hostname = new URL(url).hostname.replace(/^www\./, '');
    if (AUTHORITATIVE.has(hostname)) return 'authoritative';
    if (CREDIBLE.has(hostname)) return 'credible';
    if (LOW_QUALITY.has(hostname)) return 'low_quality';
    return 'unknown';
  } catch {
    return 'unknown';
  }
}

export function tierToConfidence(tier: DomainTier): 'high' | 'medium' | 'low' {
  switch (tier) {
    case 'authoritative': return 'high';
    case 'credible': return 'medium';
    case 'unknown': return 'low';
    case 'low_quality': return 'low';
  }
}

export function domainFromUrl(url: string | undefined): string {
  if (!url) return 'unknown';
  try {
    return new URL(url).hostname;
  } catch {
    return 'unknown';
  }
}
