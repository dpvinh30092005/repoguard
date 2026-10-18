package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Jenkins builds change requests by triggering a parameterized pipeline job
// (see ci-templates/jenkins/Jenkinsfile) and reading its report artifact.
type Jenkins struct {
	URL     string // e.g. https://jenkins.example.com
	User    string
	Token   string // API token (or password)
	Job     string // job name; folders as "team/repoguard-ci"
	Timeout time.Duration
	Poll    time.Duration // default 5s

	client *http.Client
}

func (j *Jenkins) http() *http.Client {
	if j.client == nil {
		jar, _ := cookiejar.New(nil) // CSRF crumbs are bound to the session cookie
		j.client = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	}
	return j.client
}

// jobPath turns "team/repoguard-ci" into "/job/team/job/repoguard-ci".
func (j *Jenkins) jobPath() string {
	var b strings.Builder
	for _, part := range strings.Split(strings.Trim(j.Job, "/"), "/") {
		b.WriteString("/job/" + url.PathEscape(part))
	}
	return b.String()
}

func (j *Jenkins) Build(ctx context.Context, req RemoteBuild) (Result, error) {
	poll := j.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	params := url.Values{
		"REPO_URL":         {req.CloneURL},
		"GIT_REF":          {req.Ref},
		"GIT_SHA":          {req.SHA},
		"REPO_NAME":        {req.Repo.FullName},
		"MR_NUMBER":        {strconv.Itoa(req.CR.Number)},
		"REPOGUARD_POLICY": {base64.StdEncoding.EncodeToString(req.Policy)},
	}
	queueURL, err := j.trigger(ctx, params)
	if err != nil {
		return Result{}, err
	}

	buildURL, number, err := j.waitForStart(ctx, queueURL, poll)
	if err != nil {
		return Result{}, err
	}
	res := Result{Backend: fmt.Sprintf("Jenkins #%d", number), URL: j.URL + buildURL}

	result, err := j.waitForEnd(ctx, buildURL, poll)
	if ctx.Err() == context.DeadlineExceeded {
		// Don't leave the build running; use a fresh context, ours has expired.
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = j.post(stopCtx, buildURL+"stop", nil)
		stop()
		res.TimedOut = true
		res.Logs = j.consoleTail(context.Background(), buildURL)
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if result != "SUCCESS" {
		res.ExitCode = 1
	}
	res.Report, _ = j.get(ctx, buildURL+"artifact/"+ReportFile)
	res.Logs = j.consoleTail(ctx, buildURL)
	return res, nil
}

// trigger starts the job and returns the queue item path.
func (j *Jenkins) trigger(ctx context.Context, params url.Values) (string, error) {
	resp, err := j.post(ctx, j.jobPath()+"/buildWithParameters", params)
	if err != nil {
		return "", fmt.Errorf("jenkins: trigger %s: %w", j.Job, err)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Path == "" {
		return "", fmt.Errorf("jenkins: trigger %s: no queue location in response", j.Job)
	}
	// Use only the path: Jenkins' configured root URL may differ from how we reach it.
	return strings.TrimSuffix(loc.Path, "/") + "/", nil
}

func (j *Jenkins) waitForStart(ctx context.Context, queuePath string, poll time.Duration) (string, int, error) {
	for {
		var item struct {
			Cancelled  bool   `json:"cancelled"`
			Why        string `json:"why"`
			Executable *struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			} `json:"executable"`
		}
		if err := j.getJSON(ctx, queuePath+"api/json", &item); err != nil {
			return "", 0, fmt.Errorf("jenkins: queue: %w", err)
		}
		switch {
		case item.Cancelled:
			return "", 0, fmt.Errorf("jenkins: build was cancelled while queued")
		case item.Executable != nil && item.Executable.URL != "":
			u, err := url.Parse(item.Executable.URL)
			if err != nil {
				return "", 0, err
			}
			return strings.TrimSuffix(u.Path, "/") + "/", item.Executable.Number, nil
		}
		if err := sleep(ctx, poll); err != nil {
			return "", 0, fmt.Errorf("jenkins: still queued (%s): %w", item.Why, err)
		}
	}
}

func (j *Jenkins) waitForEnd(ctx context.Context, buildPath string, poll time.Duration) (string, error) {
	for {
		var b struct {
			Building bool   `json:"building"`
			Result   string `json:"result"`
		}
		if err := j.getJSON(ctx, buildPath+"api/json?tree=building,result", &b); err != nil {
			return "", fmt.Errorf("jenkins: build status: %w", err)
		}
		if !b.Building && b.Result != "" {
			return b.Result, nil // SUCCESS | FAILURE | UNSTABLE | ABORTED
		}
		if err := sleep(ctx, poll); err != nil {
			return "", err
		}
	}
}

func (j *Jenkins) consoleTail(ctx context.Context, buildPath string) string {
	data, err := j.get(ctx, buildPath+"consoleText")
	if err != nil {
		return ""
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	return string(data)
}

// crumb fetches a CSRF token; Jenkins without CSRF protection answers 404.
func (j *Jenkins) crumb(ctx context.Context) (field, value string) {
	var c struct {
		Field string `json:"crumbRequestField"`
		Crumb string `json:"crumb"`
	}
	if err := j.getJSON(ctx, "/crumbIssuer/api/json", &c); err != nil {
		return "", ""
	}
	return c.Field, c.Crumb
}

func (j *Jenkins) post(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	field, value := j.crumb(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if field != "" {
		req.Header.Set(field, value)
	}
	req.SetBasicAuth(j.User, j.Token)
	resp, err := j.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusFound {
		return nil, fmt.Errorf("POST %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	return resp, nil
}

func (j *Jenkins) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(j.User, j.Token)
	resp, err := j.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

func (j *Jenkins) getJSON(ctx context.Context, path string, out any) error {
	data, err := j.get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
