// Package rarc reads the release-automation config document ("rarc"): the
// YAML file that lists every app the release bot can cut, and where its Slack
// channels, Jira project and version files are.
//
// The DATA FORMAT is deliberately the one the production release bot reads
// (top-level github / slack / jira / projects, with the same field names), so
// one config file can drive either bot during a rehearsal. The code is this
// repository's own.
//
// Keys are matched CASE-INSENSITIVELY. The format grew up being read by a
// case-insensitive loader, so real files mix `doneStateID` and `doneStateId`,
// `masterBranch` and `masterbranch`. A strict decoder would silently drop half
// of them and the bot would run with zero values -- e.g. a done-state of 0.
package rarc

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole document.
type Config struct {
	GitHub   GitHub    `yaml:"github"`
	Slack    Slack     `yaml:"slack"`
	Jira     Jira      `yaml:"jira"`
	Projects []Project `yaml:"projects"`
}

type GitHub struct {
	// MergeLabel marks a PR as ready for `cut`. The literal "none" means no
	// label is required.
	MergeLabel string `yaml:"mergelabel"`
	RecutLabel string `yaml:"recutlabel"`
}

type Slack struct {
	ReleaseChannel string `yaml:"releasechannel"`
	SlackbotID     string `yaml:"slackbotid"`
	UserID         string `yaml:"userid"`
	NosTeamMention string `yaml:"nosteammention"`
}

type Jira struct {
	BaseURL       string           `yaml:"baseurl"`
	QAStateID     Int              `yaml:"qastateid"`
	DoneStateID   Int              `yaml:"donestateid"`
	IssueIDFormat string           `yaml:"issueidformat"`
	PerBoard      map[string]Board `yaml:"perboard"`
	// Fields names the Jira custom fields the release ticket fills. They are
	// per Jira SITE, so they are configuration with NO defaults: a field whose
	// key is not configured is left off the ticket (and logged), never sent
	// to a guessed id.
	Fields JiraFields `yaml:"fields"`
}

// JiraFields are custom field keys, e.g. customfield_10044.
type JiraFields struct {
	Platforms    string `yaml:"platforms"`    // multi-select: [{"id": project.jira.platform}]
	Risk         string `yaml:"risk"`         // select: {"id": project.jira.releaseIssueDefaultRisk}
	Notes        string `yaml:"notes"`        // multi-line text: the release notes
	EnvVarChange string `yaml:"envvarchange"` // select: {"value": "Yes"|"No"}
}

// Board overrides the QA / done transition ids for one Jira board (the key
// prefix, e.g. ABC for ABC-123), optionally per issue type.
type Board struct {
	QAStateID   Int              `yaml:"qastateid"`
	DoneStateID Int              `yaml:"donestateid"`
	IssueTypes  map[string]Board `yaml:"issuetypes"`
}

type Project struct {
	Name                        string       `yaml:"name"`
	Repo                        string       `yaml:"repo"`
	Organization                string       `yaml:"organization"`
	ApprovalsNeeded             Int          `yaml:"approvalsneeded"`
	DefaultBranch               string       `yaml:"defaultbranch"`
	ReleaseBranch               string       `yaml:"releasebranch"`
	MasterBranch                string       `yaml:"masterbranch"`
	AutoMergeback               bool         `yaml:"automergeback"`
	ExcludeLabels               []string     `yaml:"excludelabels"`
	MasterMergebackLabels       []string     `yaml:"mastermergebacklabels"`
	DevelopMergebackLabels      []string     `yaml:"developmergebacklabels"`
	FilterCommitMessagePrefixes []string     `yaml:"filtercommitmessageprefixes"`
	FilterIssuesQuery           string       `yaml:"filterissuesquery"`
	FilesWithVersion            []string     `yaml:"fileswithversion"`
	Jira                        ProjectJira  `yaml:"jira"`
	Slack                       ProjectSlack `yaml:"slack"`
}

type ProjectJira struct {
	Platform                string `yaml:"platform"`
	ProjectID               string `yaml:"projectid"`
	ReleaseIssueTitleFormat string `yaml:"releaseissuetitleformat"`
	ReleaseIssueTypeID      string `yaml:"releaseissuetypeid"`
	ReleaseIssueProjectID   string `yaml:"releaseissueprojectid"`
	ReleaseIssueProjectKey  string `yaml:"releaseissueprojectkey"`
	ReleaseIssueDefaultRisk string `yaml:"releaseissuedefaultrisk"`
	IssueIDFormat           string `yaml:"issueidformat"`
	AutoDetectEnvVarChange  bool   `yaml:"autodetectenvvarchange"`
	// Per-project override of jira.fields.envVarChange.
	EnvVarChangeFieldKey string `yaml:"envvarchangefieldkey"`
}

type ProjectSlack struct {
	TeamChannel    string   `yaml:"teamchannel"`
	PrivateChannel string   `yaml:"privatechannel"`
	ChannelTopic   string   `yaml:"channeltopic"`
	DevTeamMention string   `yaml:"devteammention"`
	QAMentions     []string `yaml:"qamentions"`
	QATeamMention  []string `yaml:"qateammention"`
}

// Int accepts 61 and "61" alike: the documents in the wild use both.
type Int int

func (i *Int) UnmarshalYAML(n *yaml.Node) error {
	s := strings.TrimSpace(n.Value)
	if n.Tag == "!!null" || s == "" {
		*i = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not an integer", n.Line, n.Value)
	}
	*i = Int(v)
	return nil
}

func (i Int) String() string { return strconv.Itoa(int(i)) }

// Parse decodes a document. JSON is valid YAML, so a .json config parses too.
func Parse(raw []byte) (*Config, error) {
	var generic any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("rarc: not valid YAML: %w", err)
	}
	if generic == nil {
		return nil, fmt.Errorf("rarc: document is empty")
	}
	norm, err := yaml.Marshal(lowerKeys(generic))
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(norm))
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("rarc: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// lowerKeys lower-cases every mapping key, recursively, EXCEPT inside the two
// maps whose keys are data rather than field names (Jira board keys and issue
// type names). Those are kept as written and matched case-insensitively later.
func lowerKeys(v any) any { return lower(v, "") }

func lower(v any, parent string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			key := strings.ToLower(k)
			if parent == "perboard" || parent == "issuetypes" {
				key = k
			}
			childParent := key
			if parent == "perboard" || parent == "issuetypes" {
				childParent = ""
			}
			out[key] = lower(val, childParent)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = lower(val, "")
		}
		return out
	default:
		return v
	}
}

func (c *Config) validate() error {
	if len(c.Projects) == 0 {
		return fmt.Errorf("rarc: no projects configured")
	}
	seen := map[string]bool{}
	for i, p := range c.Projects {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("rarc: projects[%d] has no name", i)
		}
		k := strings.ToLower(p.Name)
		if seen[k] {
			return fmt.Errorf("rarc: project %q is listed twice", p.Name)
		}
		seen[k] = true
		if p.Repo == "" || p.Organization == "" {
			return fmt.Errorf("rarc: project %q needs repo and organization", p.Name)
		}
		if p.DefaultBranch == "" || p.ReleaseBranch == "" {
			return fmt.Errorf("rarc: project %q needs defaultbranch and releasebranch", p.Name)
		}
		for _, f := range []string{p.Jira.IssueIDFormat, c.Jira.IssueIDFormat} {
			if f == "" {
				continue
			}
			if _, err := regexp.Compile(f); err != nil {
				return fmt.Errorf("rarc: project %q: issueIDFormat %q does not compile: %w", p.Name, f, err)
			}
		}
	}
	return nil
}

// Project returns the project with this name, ignoring case, or nil.
func (c *Config) Project(name string) *Project {
	name = strings.TrimSpace(name)
	for i := range c.Projects {
		if strings.EqualFold(c.Projects[i].Name, name) {
			return &c.Projects[i]
		}
	}
	return nil
}

// Names returns the configured project names, sorted.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		if strings.TrimSpace(p.Name) != "" {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// FullRepo is organization/repo.
func (p *Project) FullRepo() string { return p.Organization + "/" + p.Repo }

// IssueIDFormat is the project's pattern, else the global one.
func (c *Config) IssueIDFormat(p *Project) string {
	if p != nil && p.Jira.IssueIDFormat != "" {
		return p.Jira.IssueIDFormat
	}
	return c.Jira.IssueIDFormat
}

// TicketFields resolves the custom field keys for a project's release ticket:
// the global jira.fields, with the project's envVarChangeFieldKey winning.
func (c *Config) TicketFields(p *Project) JiraFields {
	f := c.Jira.Fields
	if p != nil && p.Jira.EnvVarChangeFieldKey != "" {
		f.EnvVarChange = p.Jira.EnvVarChangeFieldKey
	}
	return f
}

// TitleFor renders the release ticket title. Only the FIRST %s is replaced, and
// nothing else in the format is interpreted, so a stray % in a title cannot
// turn into fmt garbage.
func (p *Project) TitleFor(tag string) string {
	f := p.Jira.ReleaseIssueTitleFormat
	if !strings.Contains(f, "%s") {
		return strings.TrimSpace(f + " " + tag)
	}
	return strings.Replace(f, "%s", tag, 1)
}

// TitlePrefix is the fixed text of the title format: the format with its %s
// removed. Empty means the format cannot identify anything.
func (p *Project) TitlePrefix() string {
	return strings.Replace(p.Jira.ReleaseIssueTitleFormat, "%s", "", 1)
}

// QAState / DoneState resolve the transition id for an issue: issue type on the
// board, then the board, then the global value.
func (c *Config) QAState(issueKey, issueType string) string {
	return c.state(issueKey, issueType, func(b Board) Int { return b.QAStateID }, c.Jira.QAStateID)
}

func (c *Config) DoneState(issueKey, issueType string) string {
	return c.state(issueKey, issueType, func(b Board) Int { return b.DoneStateID }, c.Jira.DoneStateID)
}

func (c *Config) state(issueKey, issueType string, pick func(Board) Int, global Int) string {
	board := issueKey
	if i := strings.Index(issueKey, "-"); i > 0 {
		board = issueKey[:i]
	}
	for name, b := range c.Jira.PerBoard {
		if !strings.EqualFold(name, board) {
			continue
		}
		if issueType != "" {
			for tn, tb := range b.IssueTypes {
				if strings.EqualFold(tn, issueType) && pick(tb) != 0 {
					return pick(tb).String()
				}
			}
		}
		if pick(b) != 0 {
			return pick(b).String()
		}
	}
	return global.String()
}

// BoardHasIssueTypes reports whether resolving a transition for this key needs
// the issue's type (so the caller can skip a Jira lookup when it does not).
func (c *Config) BoardHasIssueTypes(issueKey string) bool {
	board := issueKey
	if i := strings.Index(issueKey, "-"); i > 0 {
		board = issueKey[:i]
	}
	for name, b := range c.Jira.PerBoard {
		if strings.EqualFold(name, board) && len(b.IssueTypes) > 0 {
			return true
		}
	}
	return false
}
