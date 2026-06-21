package server

import (
	"net/url"
	"strings"
)

// DomainTier is the curated reliability classification for the
// domain a gathered source came from. The gate combines this with
// the LLM's reading of the content to produce the final reliability
// verdict. Ported verbatim from src/validation/source-tiers.ts.
type DomainTier string

const (
	TierAuthoritative DomainTier = "authoritative"
	TierCredible      DomainTier = "credible"
	TierUnknown       DomainTier = "unknown"
	TierLowQuality    DomainTier = "low_quality"
)

// authoritativeDomains is the primary-source set: peer-reviewed,
// upstream documentation, code repositories. Editors of this list
// should treat additions as policy decisions — every entry shifts
// gate verdicts on every page from that origin.
var authoritativeDomains = map[string]bool{
	"arxiv.org":                  true,
	"pubmed.ncbi.nlm.nih.gov":    true,
	"nature.com":                 true,
	"science.org":                true,
	"github.com":                 true,
	"npmjs.com":                  true,
	"docs.python.org":            true,
	"developer.mozilla.org":      true,
	"developer.apple.com":        true,
	"docs.microsoft.com":         true,
	"cloud.google.com":           true,
	"aws.amazon.com":             true,
	"docs.aws.amazon.com":        true,
	"kubernetes.io":              true,
	"go.dev":                     true,
}

// credibleDomains is well-known secondary sources: encyclopedias,
// reputable journalism, large community Q&A sites.
var credibleDomains = map[string]bool{
	"wikipedia.org":     true,
	"britannica.com":    true,
	"stackoverflow.com": true,
	"ycombinator.com":   true,
	"techcrunch.com":    true,
	"wired.com":         true,
	"arstechnica.com":   true,
	"theverge.com":      true,
	"reuters.com":       true,
	"apnews.com":        true,
	"bbc.com":           true,
	"theguardian.com":   true,
}

// lowQualityDomains is the explicit deny-list. Kept tiny — it's
// easier to defend an additions list than to enumerate "everything
// untrustworthy". The fallback is TierUnknown, which the gate then
// scrutinises on content.
var lowQualityDomains = map[string]bool{
	"content-farm.com": true,
}

// ScoreDomain returns the tier for the given URL. Strips "www." and
// matches against the curated maps; unknown/unparseable input yields
// TierUnknown so the gate has a sane fallback.
func ScoreDomain(rawURL string) DomainTier {
	if rawURL == "" {
		return TierUnknown
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return TierUnknown
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if authoritativeDomains[host] {
		return TierAuthoritative
	}
	if credibleDomains[host] {
		return TierCredible
	}
	if lowQualityDomains[host] {
		return TierLowQuality
	}
	// Try walking up the domain (e.g. blog.arxiv.org → arxiv.org)
	// so subdomains of authoritative sources don't get demoted.
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts)-1; i++ {
		parent := strings.Join(parts[i:], ".")
		if authoritativeDomains[parent] {
			return TierAuthoritative
		}
		if credibleDomains[parent] {
			return TierCredible
		}
	}
	return TierUnknown
}
