package ci

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/config"
)

// SonarResult is the SonarQube part of the report.
type SonarResult struct {
	Ran        bool              `json:"ran"`
	Note       string            `json:"note,omitempty"`
	ProjectKey string            `json:"project_key,omitempty"`
	URL        string            `json:"url,omitempty"`  // dashboard
	Gate       string            `json:"gate,omitempty"` // OK | WARN | ERROR | NONE
	Conditions []SonarCondition  `json:"conditions,omitempty"`
	Measures   map[string]string `json:"measures,omitempty"`
	Error      string            `json:"error,omitempty"`
	FailOnGate bool              `json:"fail_on_gate"`
}

type SonarCondition struct {
	Metric     string `json:"metric"`
	Status     string `json:"status"`
	Actual     string `json:"actual"`
	Comparator string `json:"comparator"`
	Threshold  string `json:"threshold"`
}

func (s SonarResult) Failed() bool {
	return s.Ran && (s.Error != "" || (s.FailOnGate && s.Gate == "ERROR"))
}

// SonarMetrics are shown in the report, in this order.
var SonarMetrics = []string{"bugs", "vulnerabilities", "security_hotspots", "code_smells", "coverage", "duplicated_lines_density"}

var invalidKeyChars = regexp.MustCompile(`[^A-Za-z0-9_.:-]+`)

// sonarProjectKey picks the configured key, else derives one from the repository name.
func sonarProjectKey(cfg config.Sonar, dir string, getenv func(string) string) string {
	if cfg.ProjectKey != "" {
		return cfg.ProjectKey
	}
	name := ""
	for _, v := range []string{"REPOGUARD_REPO", "CI_PROJECT_PATH", "GITHUB_REPOSITORY"} {
		if name = getenv(v); name != "" {
			break
		}
	}
	if name == "" {
		abs, _ := filepath.Abs(dir)
		name = filepath.Base(abs)
	}
	return strings.Trim(invalidKeyChars.ReplaceAllString(strings.ReplaceAll(name, "/", "_"), "_"), "_")
}

func runSonar(ctx context.Context, opts Options, cfg config.Sonar) SonarResult {
	host, token := strings.TrimRight(opts.Getenv("SONAR_HOST_URL"), "/"), opts.Getenv("SONAR_TOKEN")
	switch {
	case !cfg.Enabled || (host == "" && token == ""):
		return SonarResult{} // not configured: nothing to show
	case host == "" || token == "":
		return SonarResult{Note: "set both SONAR_HOST_URL and SONAR_TOKEN to analyse with SonarQube"}
	case !opts.LookCmd("sonar-scanner"):
		return SonarResult{Note: "sonar-scanner not installed: skipped"}
	}

	res := SonarResult{Ran: true, FailOnGate: cfg.FailOnGate, ProjectKey: sonarProjectKey(cfg, opts.Dir, opts.Getenv)}
	res.URL = host + "/dashboard?id=" + url.QueryEscape(res.ProjectKey)
	// Host and token reach the scanner through SONAR_HOST_URL / SONAR_TOKEN in
	// its environment, so the token never appears in the command line.
	args := []string{
		"sonar-scanner",
		"-Dsonar.projectKey=" + res.ProjectKey,
		"-Dsonar.sources=.",
		"-Dsonar.exclusions=**/node_modules/**,**/vendor/**,**/dist/**,**/build/**,**/.venv/**,**/testdata/**",
		"-Dsonar.qualitygate.wait=true",
		"-Dsonar.qualitygate.timeout=300",
	}
	args = append(args, cfg.Args...)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	fmt.Fprintf(opts.Out, "\n==> [sonar] %s\n", strings.Join(args, " "))
	scanErr := opts.Runner(ctx, opts.Dir, strings.Join(quoted, " "), opts.Out) // non-zero when the gate fails

	if dash := readReportTask(opts.Dir)["dashboardUrl"]; dash != "" {
		res.URL = dash
	}
	api := sonarAPI{host: host, token: token, client: &http.Client{Timeout: 30 * time.Second}}
	gate, conditions, err := api.qualityGate(ctx, res.ProjectKey)
	if err != nil {
		res.Error = "could not read the quality gate: " + err.Error()
		if scanErr != nil {
			res.Error = fmt.Sprintf("sonar-scanner failed (%v); %s", scanErr, res.Error)
		}
		return res
	}
	res.Gate, res.Conditions = gate, conditions
	res.Measures, _ = api.measures(ctx, res.ProjectKey) // best effort
	return res
}

// readReportTask parses .scannerwork/report-task.txt (key=value lines).
func readReportTask(dir string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(filepath.Join(dir, ".scannerwork", "report-task.txt"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = v
		}
	}
	return out
}

type sonarAPI struct {
	host, token string
	client      *http.Client
}

func (s sonarAPI) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

func (s sonarAPI) qualityGate(ctx context.Context, key string) (string, []SonarCondition, error) {
	var resp struct {
		ProjectStatus struct {
			Status     string `json:"status"`
			Conditions []struct {
				Status         string `json:"status"`
				MetricKey      string `json:"metricKey"`
				Comparator     string `json:"comparator"`
				ErrorThreshold string `json:"errorThreshold"`
				ActualValue    string `json:"actualValue"`
			} `json:"conditions"`
		} `json:"projectStatus"`
	}
	if err := s.get(ctx, "/api/qualitygates/project_status?projectKey="+url.QueryEscape(key), &resp); err != nil {
		return "", nil, err
	}
	var conds []SonarCondition
	for _, c := range resp.ProjectStatus.Conditions {
		conds = append(conds, SonarCondition{Metric: c.MetricKey, Status: c.Status, Actual: c.ActualValue, Comparator: c.Comparator, Threshold: c.ErrorThreshold})
	}
	return resp.ProjectStatus.Status, conds, nil
}

func (s sonarAPI) measures(ctx context.Context, key string) (map[string]string, error) {
	var resp struct {
		Component struct {
			Measures []struct {
				Metric string `json:"metric"`
				Value  string `json:"value"`
			} `json:"measures"`
		} `json:"component"`
	}
	path := "/api/measures/component?component=" + url.QueryEscape(key) + "&metricKeys=" + strings.Join(SonarMetrics, ",")
	if err := s.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range resp.Component.Measures {
		out[m.Metric] = m.Value
	}
	return out, nil
}
