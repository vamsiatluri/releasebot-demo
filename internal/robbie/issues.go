package robbie

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultIssueIDFormat is used when the config sets none.
const DefaultIssueIDFormat = `\b[A-Z][A-Z0-9]+-\d+\b`

// IssueKeys finds Jira keys in texts with the configured pattern, normalised to
// KEY-123 (upper case, no spaces, one hyphen), de-duplicated, in first-seen
// order. Patterns in the wild match "ABC 12" and "abc-12" too, hence the
// normalisation.
func IssueKeys(format string, texts ...string) ([]string, error) {
	if format == "" {
		format = DefaultIssueIDFormat
	}
	re, err := regexp.Compile(format)
	if err != nil {
		return nil, fmt.Errorf("issueIDFormat %q: %w", format, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		for _, m := range re.FindAllString(t, -1) {
			k := normaliseKey(m)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

var keyShape = regexp.MustCompile(`^([A-Z][A-Z0-9]*?)[\s_-]*(\d+)$`)

func normaliseKey(m string) string {
	parts := keyShape.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(m)))
	if parts == nil {
		return ""
	}
	return parts[1] + "-" + parts[2]
}

// JiraFilterURL is the link to a set of issues, in the `key in (...)` form the
// cut message uses.
func JiraFilterURL(base string, keys []string) string {
	return strings.TrimRight(base, "/") + "/issues/?jql=" + url.QueryEscape("key in ("+strings.Join(keys, ",")+")")
}

// JiraIDListURL is the link appended to release notes (`id in (...)`).
func JiraIDListURL(base string, keys []string) string {
	return strings.TrimRight(base, "/") + "/issues/?jql=id%20in%20(" + strings.Join(keys, ",") + ")"
}
