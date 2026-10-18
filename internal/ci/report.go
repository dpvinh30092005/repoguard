package ci

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

var stageIcon = map[string]string{"passed": "✅", "failed": "❌", "skipped": "⏭️"}

// WriteMarkdown renders the report for a job summary / MR artifact.
func (r Report) WriteMarkdown(w io.Writer) {
	verdict := "✅ passed"
	if r.Failed() {
		verdict = "❌ failed"
	}
	fmt.Fprintf(w, "## 🛡️ repoguard ci — %s\n\n", verdict)

	if len(r.Projects) == 0 {
		fmt.Fprintln(w, "_No affected projects._")
	} else {
		fmt.Fprintf(w, "| Project | Type | %s |\n|---|---|%s\n", strings.Join(Stages, " | "), strings.Repeat("---|", len(Stages)))
		for _, p := range r.Projects {
			cells := make([]string, len(Stages))
			for i, name := range Stages {
				cells[i] = "–"
				for _, s := range p.Stages {
					if s.Stage == name {
						cells[i] = stageIcon[s.Status]
						if s.Status != "skipped" {
							cells[i] += fmt.Sprintf(" %.1fs", s.Duration.Seconds())
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
				fmt.Fprintf(w, "\n<details><summary>❌ <code>%s</code> · %s — <code>%s</code></summary>\n\n```\n%s\n```\n</details>\n",
					p.Project.Path, s.Stage, s.Command, strings.TrimSpace(s.Output))
			}
		}
	}

	switch {
	case !r.SecretsRan:
	case len(r.Secrets) == 0:
		fmt.Fprintln(w, "\n**Secrets:** ✅ none found")
	default:
		fmt.Fprintf(w, "\n**Secrets:** ❌ %d possible secret(s) — remove and rotate them:\n", len(r.Secrets))
		for _, s := range r.Secrets {
			fmt.Fprintf(w, "- `%s:%d` %s (`%s`)\n", s.Path, s.Line, s.Rule, s.Masked)
		}
	}

	switch {
	case !r.Vulns.Ran:
		fmt.Fprintf(w, "\n**Dependencies:** ⏭️ %s\n", r.Vulns.Note)
	case r.Vulns.Count == 0:
		fmt.Fprintf(w, "\n**Dependencies:** ✅ no known vulnerabilities %s\n", r.Vulns.Note)
	default:
		icon := "⚠️"
		if r.FailOnVulns {
			icon = "❌"
		}
		fmt.Fprintf(w, "\n**Dependencies:** %s %d known vulnerabilit(ies) — see `osv-scanner` output in the job log\n", icon, r.Vulns.Count)
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
