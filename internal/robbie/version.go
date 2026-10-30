package robbie

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Semver is major.minor.patch. A pre-release or build suffix is dropped on
// bump, as semver says a bump of 1.2.3-rc.1 by patch gives 1.2.3.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func ParseSemver(s string) (Semver, error) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Semver{}, fmt.Errorf("%q is not a semantic version", s)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return Semver{a, b, c, m[4]}, nil
}

func (v Semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Tag is the git tag for the version: v1.2.3.
func (v Semver) Tag() string { return "v" + v.String() }

func (v Semver) Bump(kind string) (Semver, error) {
	pre := v.Pre != ""
	switch strings.ToLower(kind) {
	case "patch":
		if pre {
			return Semver{v.Major, v.Minor, v.Patch, ""}, nil
		}
		return Semver{v.Major, v.Minor, v.Patch + 1, ""}, nil
	case "minor":
		if pre && v.Patch == 0 {
			return Semver{v.Major, v.Minor, 0, ""}, nil
		}
		return Semver{v.Major, v.Minor + 1, 0, ""}, nil
	case "major":
		if pre && v.Minor == 0 && v.Patch == 0 {
			return Semver{v.Major, 0, 0, ""}, nil
		}
		return Semver{v.Major + 1, 0, 0, ""}, nil
	}
	return v, fmt.Errorf("bump must be patch, minor or major, got %q", kind)
}

// Less orders versions (pre-releases before their release).
func (v Semver) Less(o Semver) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	return v.Pre != "" && (o.Pre == "" || v.Pre < o.Pre)
}

// VersionJump renders "1.4.0 → 1.4.1 (patch)".
func VersionJump(old, new Semver, bump string) string {
	return fmt.Sprintf("%s → %s (%s)", old, new, bump)
}

// ReadVersion reads the top-level "version" of a version file (JSON such as
// package.json, or YAML).
func ReadVersion(content string) (string, error) {
	var j struct {
		Version any `json:"version"`
	}
	if err := json.Unmarshal([]byte(content), &j); err == nil {
		if s, ok := j.Version.(string); ok && s != "" {
			return s, nil
		}
		return "", fmt.Errorf("no string \"version\" field")
	}
	var y map[string]any
	if err := yaml.Unmarshal([]byte(content), &y); err != nil {
		return "", fmt.Errorf("neither JSON nor YAML: %v", err)
	}
	switch v := y["version"].(type) {
	case string:
		if v != "" {
			return v, nil
		}
	case float64, int:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("no top-level version field")
}

var (
	jsonVersionRE = regexp.MustCompile(`"version"(\s*):(\s*)"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?"`)
	lockfileRE    = regexp.MustCompile(`"lockfileVersion"\s*:\s*(\d+)`)
	yamlVersionRE = regexp.MustCompile(`(?m)^(\s*version:\s*)(["']?)\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(["']?)`)
)

// ReplaceVersion rewrites the version in a version file and leaves every other
// byte alone (formatting, key order, trailing newline).
//
// JSON: the first "version": "x.y.z". A package-lock.json at lockfileVersion 2
// or 3 also repeats it under packages[""], so the first TWO occurrences are
// replaced there. YAML: the first `version: x.y.z` line, quoted or not.
func ReplaceVersion(content, newVersion string) (string, error) {
	if locs := jsonVersionRE.FindAllStringSubmatchIndex(content, -1); len(locs) > 0 {
		n := 1
		if m := lockfileRE.FindStringSubmatch(content); m != nil && (m[1] == "2" || m[1] == "3") {
			n = 2
		}
		if n > len(locs) {
			n = len(locs)
		}
		var b strings.Builder
		last := 0
		for _, loc := range locs[:n] {
			b.WriteString(content[last:loc[0]])
			b.WriteString(`"version"` + content[loc[2]:loc[3]] + ":" + content[loc[4]:loc[5]] + `"` + newVersion + `"`)
			last = loc[1]
		}
		b.WriteString(content[last:])
		return b.String(), nil
	}
	if loc := yamlVersionRE.FindStringSubmatchIndex(content); loc != nil {
		prefix := content[loc[2]:loc[3]]
		q1, q2 := content[loc[4]:loc[5]], content[loc[6]:loc[7]]
		return content[:loc[0]] + prefix + q1 + newVersion + q2 + content[loc[1]:], nil
	}
	return "", fmt.Errorf("could not find a version in the file")
}
