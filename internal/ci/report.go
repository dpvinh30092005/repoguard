package ci

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

var stageWord = map[string]string{"passed": "passed", "failed": "**failed**", "skipped": "skipped"}

// WriteMarkdown renders the report for a job summary or a merge request comment.
func (r Report) WriteMarkdown(w io.Writer) {
	verdict := "passed"
	if r.Failed() {
		verdict = "failed"
	}
	fmt.Fprintf(w, "#### repoguard · Build & Tests %s\n\n", verdict)

	if len(r.Projects) == 0 {
		fmt.Fprintln(w, "No project was affected by this change.")
	} else {
		fmt.Fprintf(w, "| Project | Type | %s |\n| :-- | :-- |%s\n", strings.Join(Stages, " | "), strings.Repeat(" :-- |", len(Stages)))
		for _, p := range r.Projects {
			cells := make([]string, len(Stages))
			for i, name := range Stages {
				cells[i] = "–"
				for _, s := range p.Stages {
					if s.Stage == name {
						cells[i] = stageWord[s.Status]
						if s.Status != "skipped" {
							cells[i] += fmt.Sprintf(" · %.1fs", s.Duration.Seconds())
						}
					}
				}
			}
			fmt.Fprintf(w, "| `%s` | %s | %s |\n", p.Project.Path, p.Project.Type, strings.Join(cells, " | "))
		}
	}

	for _, p := range r.Projects {
		for _, s := range p.Stages {
			if s.Status == "failed" {
				fmt.Fprintf(w, "\n<details><summary>Failed: <code>%s</code> · %s · <code>%s</code></summary>\n\n```\n%s\n```\n</details>\n",
					p.Project.Path, s.Stage, s.Command, strings.TrimSpace(s.Output))
			}
		}
	}

	switch {
	case !r.SecretsRan:
	case len(r.Secrets) == 0:
		fmt.Fprintln(w, "\n**Secrets:** none found")
	default:
		fmt.Fprintf(w, "\n**Secrets:** %d possible secret(s). Remove them from the code and rotate them:\n", len(r.Secrets))
		for _, s := range r.Secrets {
			fmt.Fprintf(w, "- `%s:%d` %s (`%s`)\n", s.Path, s.Line, s.Rule, s.Masked)
		}
	}

	r.writeAPITests(w)
	r.writeSonar(w)

	switch {
	case !r.Vulns.Ran && r.Vulns.Note == "":
		fmt.Fprintln(w, "\n**Dependencies:** not scanned")
	case !r.Vulns.Ran:
		fmt.Fprintf(w, "\n**Dependencies:** not scanned: %s\n", r.Vulns.Note)
	case r.Vulns.Count == 0:
		fmt.Fprintf(w, "\n**Dependencies:** no known vulnerabilities %s\n", r.Vulns.Note)
	default:
		fmt.Fprintf(w, "\n**Dependencies:** %d known vulnerabilit(ies). Details are in the `osv-scanner` output of the build log.\n", r.Vulns.Count)
	}
}

func (r Report) writeAPITests(w io.Writer) {
	a := r.APITests
	switch {
	case !a.Ran && a.Note != "":
		fmt.Fprintf(w, "\n**API tests (Postman):** not run: %s\n", a.Note)
		return
	case !a.Ran:
		return
	}
	verdict := "passed"
	if a.Failed() {
		verdict = "**failed**"
	}
	fmt.Fprintf(w, "\n**API tests (Postman):** %s\n\n| Collection | Requests | Assertions | Failed |\n| :-- | --: | --: | --: |\n", verdict)
	for _, res := range a.Results {
		if res.Error != "" {
			fmt.Fprintf(w, "| %s | – | – | error: %s |\n", res.Name, res.Error)
			continue
		}
		fmt.Fprintf(w, "| %s | %d | %d | %d |\n", res.Name, res.Requests, res.Assertions, res.Failed)
	}
	for _, res := range a.Results {
		for _, f := range res.Failures {
			fmt.Fprintf(w, "- **%s** › %s: %s\n", f.Request, f.Test, f.Message)
		}
	}
}

var sonarLabels = map[string]string{
	"bugs": "Bugs", "vulnerabilities": "Vulnerabilities", "security_hotspots": "Security hotspots",
	"code_smells": "Code smells", "coverage": "Coverage %", "duplicated_lines_density": "Duplication %",
}

// SonarLabel names a SonarQube metric for people.
func SonarLabel(metric string) string {
	if l, ok := sonarLabels[metric]; ok {
		return l
	}
	return metric
}

func (r Report) writeSonar(w io.Writer) {
	s := r.Sonar
	switch {
	case !s.Ran && s.Note != "":
		fmt.Fprintf(w, "\n**SonarQube:** not run: %s\n", s.Note)
		return
	case !s.Ran:
		return
	case s.Error != "":
		fmt.Fprintf(w, "\n**SonarQube:** error: %s\n", s.Error)
		return
	}
	verdict := map[string]string{"OK": "Quality Gate passed", "ERROR": "**Quality Gate failed**", "WARN": "Quality Gate warning"}[s.Gate]
	if verdict == "" {
		verdict = "no Quality Gate (" + s.Gate + ")"
	}
	fmt.Fprintf(w, "\n**SonarQube:** %s · [%s](%s)\n", verdict, s.ProjectKey, s.URL)
	var parts []string
	for _, m := range SonarMetrics {
		if v, ok := s.Measures[m]; ok {
			parts = append(parts, fmt.Sprintf("%s: **%s**", sonarLabels[m], v))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "\n%s\n", strings.Join(parts, " · "))
	}
	for _, c := range s.Conditions {
		if c.Status == "ERROR" {
			fmt.Fprintf(w, "- `%s` = %s (limit %s %s)\n", c.Metric, c.Actual, c.Comparator, c.Threshold)
		}
	}
}

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *struct{}     `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit renders one test case per project stage and scan; GitLab shows
// these in the merge request widget, GitHub keeps them as an artifact.
func (r Report) WriteJUnit(w io.Writer) error {
	s := junitSuite{Name: "repoguard"}
	add := func(c junitCase) {
		s.Tests++
		if c.Failure != nil {
			s.Failures++
		}
		if c.Skipped != nil {
			s.Skipped++
		}
		s.Cases = append(s.Cases, c)
	}
	for _, p := range r.Projects {
		for _, st := range p.Stages {
			c := junitCase{Classname: p.Project.Path, Name: st.Stage, Time: st.Duration.Seconds()}
			switch st.Status {
			case "failed":
				c.Failure = &junitFailure{Message: st.Command, Text: st.Output}
			case "skipped":
				c.Skipped = &struct{}{}
			}
			add(c)
		}
	}
	if r.SecretsRan {
		c := junitCase{Classname: "repoguard", Name: "secrets"}
		if len(r.Secrets) > 0 {
			var b strings.Builder
			for _, f := range r.Secrets {
				fmt.Fprintf(&b, "%s:%d %s (%s)\n", f.Path, f.Line, f.Rule, f.Masked)
			}
			c.Failure = &junitFailure{Message: fmt.Sprintf("%d possible secret(s)", len(r.Secrets)), Text: b.String()}
		}
		add(c)
	}
	for _, res := range r.APITests.Results {
		c := junitCase{Classname: "postman", Name: res.Name, Time: res.Duration.Seconds()}
		switch {
		case res.Error != "":
			c.Failure = &junitFailure{Message: res.Error}
		case res.Failed > 0:
			var b strings.Builder
			for _, f := range res.Failures {
				fmt.Fprintf(&b, "%s > %s: %s\n", f.Request, f.Test, f.Message)
			}
			c.Failure = &junitFailure{Message: fmt.Sprintf("%d failed assertion(s)", res.Failed), Text: b.String()}
		}
		add(c)
	}
	if r.Sonar.Ran {
		c := junitCase{Classname: "sonar", Name: "quality-gate"}
		switch {
		case r.Sonar.Error != "":
			c.Failure = &junitFailure{Message: r.Sonar.Error}
		case r.Sonar.FailOnGate && r.Sonar.Gate == "ERROR":
			c.Failure = &junitFailure{Message: "quality gate failed: " + r.Sonar.URL}
		}
		add(c)
	}
	if r.Vulns.Ran && r.FailOnVulns {
		c := junitCase{Classname: "repoguard", Name: "dependencies"}
		if r.Vulns.Count > 0 {
			c.Failure = &junitFailure{Message: fmt.Sprintf("%d known vulnerabilities", r.Vulns.Count)}
		}
		add(c)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(s)
}
