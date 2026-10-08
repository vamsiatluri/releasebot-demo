package contract

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

type Summary struct {
	Target        string        `json:"target"`
	Environment   string        `json:"environment"`
	StartedAt     time.Time     `json:"started_at"`
	Duration      time.Duration `json:"-"`
	DurationMS    int64         `json:"duration_ms"`
	Total         int           `json:"total"`
	Passed        int           `json:"passed"`
	Failed        int           `json:"failed"`
	CriticalFails int           `json:"critical_failures"`
	Results       []Result      `json:"-"`
}

func Summarise(target, env string, started time.Time, results []Result) Summary {
	s := Summary{Target: target, Environment: env, StartedAt: started,
		Total: len(results), Results: results}
	for _, r := range results {
		if r.Pass {
			s.Passed++
			continue
		}
		s.Failed++
		if r.Case.Severity == Critical {
			s.CriticalFails++
		}
	}
	s.Duration = time.Since(started)
	s.DurationMS = s.Duration.Milliseconds()
	return s
}

// --- terminal ---------------------------------------------------------------

func (s Summary) WriteText(w io.Writer) {
	fmt.Fprintf(w, "\nReleaseBot contract suite\n  target: %s\n  env:    %s\n\n", s.Target, s.Environment)
	for _, r := range s.Results {
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %-46s %4dms\n", mark, r.Case.Name, r.Duration.Milliseconds())
		for _, f := range r.Failures {
			fmt.Fprintf(w, "         %s\n", f)
		}
	}
	fmt.Fprintf(w, "\n  %d passed, %d failed (%d critical) in %s\n\n",
		s.Passed, s.Failed, s.CriticalFails, s.Duration.Round(time.Millisecond))
}

// --- GitHub step summary ----------------------------------------------------

// WriteMarkdown is what a non-engineer actually reads. Every row carries the
// WHY, not just the test name: "23 passed" tells a manager nothing, "forged
// Slack signature rejected" tells them the endpoint is safe to leave public.
func (s Summary) WriteMarkdown(w io.Writer) {
	status := "✅ **All checks passed**"
	if s.CriticalFails > 0 {
		status = "🔴 **CRITICAL FAILURE — do not cut over**"
	} else if s.Failed > 0 {
		status = "⚠️ **Non-critical failures**"
	}
	fmt.Fprintf(w, "## ReleaseBot contract suite — %s\n\n", s.Environment)
	fmt.Fprintf(w, "%s &nbsp;&nbsp; `%d/%d` passed in %s\n\n", status, s.Passed, s.Total,
		s.Duration.Round(time.Millisecond))
	fmt.Fprintf(w, "Target: `%s`\n\n", s.Target)
	fmt.Fprintln(w, "| | Check | What it proves | |")
	fmt.Fprintln(w, "|---|---|---|---|")
	for _, r := range s.Results {
		mark := "✅"
		if !r.Pass {
			mark = "❌"
			if r.Case.Severity == Critical {
				mark = "🔴"
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | `%dms` |\n", mark, r.Case.Name, r.Case.Why,
			r.Duration.Milliseconds())
	}
	if s.Failed > 0 {
		fmt.Fprint(w, "\n### Failures\n\n")
		for _, r := range s.Results {
			if r.Pass {
				continue
			}
			fmt.Fprintf(w, "**%s** (%s)\n", r.Case.Name, r.Case.Severity)
			for _, f := range r.Failures {
				fmt.Fprintf(w, "- %s\n", f)
			}
			fmt.Fprintf(w, "- response: `HTTP %d` `%s`\n\n", r.Status, truncate(r.Body, 300))
		}
	}
}

// --- JUnit ------------------------------------------------------------------

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Time     float64     `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit feeds GitHub's native per-test reporting, so a failure shows up
// annotated on the run rather than buried in a log.
func (s Summary) WriteJUnit(w io.Writer) error {
	suite := junitSuite{
		Name:  "releasebot-contract-" + s.Environment,
		Tests: s.Total, Failures: s.Failed, Time: s.Duration.Seconds(),
	}
	for _, r := range s.Results {
		jc := junitCase{
			Name:      r.Case.Name,
			ClassName: "releasebot." + s.Environment + "." + string(r.Case.Severity),
			Time:      r.Duration.Seconds(),
		}
		if !r.Pass {
			jc.Failure = &junitFailure{
				Message: strings.Join(r.Failures, "; "),
				Type:    string(r.Case.Severity),
				Text: fmt.Sprintf("%s\n\nWhy this matters: %s\n\nHTTP %d\n%s",
					strings.Join(r.Failures, "\n"), r.Case.Why, r.Status, truncate(r.Body, 1000)),
			}
		}
		suite.Cases = append(suite.Cases, jc)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(junitSuites{Suites: []junitSuite{suite}})
}

// --- CloudWatch -------------------------------------------------------------

type metric struct {
	MetricName string            `json:"MetricName"`
	Value      float64           `json:"Value"`
	Unit       string            `json:"Unit"`
	Dimensions []metricDimension `json:"Dimensions"`
}

type metricDimension struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// WriteMetrics emits a put-metric-data payload. The workflow pipes it to the
// AWS CLI rather than the binary calling AWS itself -- that keeps the test
// runner dependency-free and credential-free, so it is safe to run anywhere,
// including against localhost in a pull request from a fork.
func (s Summary) WriteMetrics(w io.Writer) error {
	dims := []metricDimension{{Name: "Environment", Value: s.Environment}}
	pct := 100.0
	if s.Total > 0 {
		pct = float64(s.Passed) / float64(s.Total) * 100
	}
	ms := []metric{
		{MetricName: "ContractSuccessPercent", Value: pct, Unit: "Percent", Dimensions: dims},
		{MetricName: "ContractChecksFailed", Value: float64(s.Failed), Unit: "Count", Dimensions: dims},
		{MetricName: "ContractCriticalFailed", Value: float64(s.CriticalFails), Unit: "Count", Dimensions: dims},
		{MetricName: "ContractDuration", Value: float64(s.DurationMS), Unit: "Milliseconds", Dimensions: dims},
	}

	// ⚠️ Emit a DIMENSIONLESS copy as well, for the estate-wide alarms.
	//
	// A CloudWatch alarm that names no dimensions does not aggregate across
	// dimensions -- it matches only the metric published with no dimensions at
	// all. So an alarm written without dimensions against a metric that is only
	// ever published WITH them never sees a datapoint: it cannot fire when
	// something breaks, and with TreatMissingData=breaching it cannot clear
	// either. It just sits red, and everyone learns to ignore it.
	//
	// Caught on 2026-10-07 when four green suite runs left the staleness alarm
	// stuck in ALARM. The per-environment series drives the dashboard; this
	// dimensionless rollup drives the alarms. Both, deliberately, rather than
	// one pretending to be the other.
	for _, m := range []metric{
		{MetricName: "ContractSuccessPercent", Value: pct, Unit: "Percent"},
		{MetricName: "ContractChecksFailed", Value: float64(s.Failed), Unit: "Count"},
		{MetricName: "ContractCriticalFailed", Value: float64(s.CriticalFails), Unit: "Count"},
	} {
		m.Dimensions = []metricDimension{}
		ms = append(ms, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ms)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
