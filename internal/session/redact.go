package session

import (
	"regexp"
	"sort"
)

// redactPattern is one secret family the redactor scrubs before anything
// leaves the machine (OPEN_QUESTIONS.md §2: redact-and-continue + preview).
type redactPattern struct {
	name string
	re   *regexp.Regexp
}

var redactPatterns = []redactPattern{
	{"fmsgk_key", regexp.MustCompile(`fmsgk_[A-Za-z0-9]+_[A-Za-z0-9_\-]+`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github_token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{16,}\b`)},
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9]{20,}\b`)},
	{"gcp_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)},
	{"private_key_block", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"env_secret", regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY)[A-Z0-9_]*)\s*[=:]\s*['"]?[^\s'"]{6,}['"]?`)},
}

// scrub replaces secret matches in s with [REDACTED:<type>], recording each
// pattern family that hit in hits.
func scrub(s string, hits map[string]bool) string {
	for _, p := range redactPatterns {
		if p.name == "env_secret" {
			// Keep the variable name, redact only the value.
			s = p.re.ReplaceAllStringFunc(s, func(m string) string {
				sub := p.re.FindStringSubmatch(m)
				hits[p.name] = true
				return sub[1] + "=[REDACTED:env_secret]"
			})
			continue
		}
		if p.re.MatchString(s) {
			hits[p.name] = true
			s = p.re.ReplaceAllString(s, "[REDACTED:"+p.name+"]")
		}
	}
	return s
}

func sortedHits(hits map[string]bool) []string {
	var names []string
	for n := range hits {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Redact scrubs secrets from every text block of the turns, replacing matches
// with [REDACTED:<type>]. It returns the pattern names that hit, sorted, for
// the share preview.
func Redact(turns []Turn) []string {
	hits := map[string]bool{}
	for ti := range turns {
		for bi := range turns[ti].Blocks {
			b := &turns[ti].Blocks[bi]
			b.Text = scrub(b.Text, hits)
		}
	}
	return sortedHits(hits)
}

// RedactText scrubs one string and returns it with the pattern names that hit,
// sorted — for bodies that don't pass through a Transcript (summaries, direct
// sends).
func RedactText(s string) (string, []string) {
	hits := map[string]bool{}
	return scrub(s, hits), sortedHits(hits)
}
