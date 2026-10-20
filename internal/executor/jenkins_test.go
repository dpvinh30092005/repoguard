package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// fakeJenkins mimics the REST endpoints the backend uses, including CSRF.
type fakeJenkins struct {
	mu        sync.Mutex
	params    map[string]string
	polls     int
	result    string
	report    string
	neverEnds bool
	stopped   bool
}

func (f *fakeJenkins) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "api-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /crumbIssuer/api/json", auth(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "s1"})
		_, _ = io.WriteString(w, `{"crumbRequestField":"Jenkins-Crumb","crumb":"c123"}`)
	}))
	mux.HandleFunc("POST /job/team/job/repoguard-ci/buildWithParameters", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Jenkins-Crumb") != "c123" {
			http.Error(w, "No valid crumb", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		f.params = map[string]string{}
		for k := range r.PostForm {
			f.params[k] = r.PostForm.Get(k)
		}
		f.mu.Unlock()
		// Jenkins reports its own (possibly different) root URL; only the path must be used.
		w.Header().Set("Location", "http://jenkins.internal:8080/queue/item/7/")
		w.WriteHeader(http.StatusCreated)
	}))
	mux.HandleFunc("GET /queue/item/7/api/json", auth(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		if f.polls < 2 {
			_, _ = io.WriteString(w, `{"why":"Waiting for next available executor"}`)
			return
		}
		_, _ = io.WriteString(w, `{"executable":{"number":12,"url":"http://jenkins.internal:8080/job/team/job/repoguard-ci/12/"}}`)
	}))
	mux.HandleFunc("GET /job/team/job/repoguard-ci/12/api/json", auth(func(w http.ResponseWriter, _ *http.Request) {
		if f.neverEnds {
			_, _ = io.WriteString(w, `{"building":true,"result":null}`)
			return
		}
		fmt.Fprintf(w, `{"building":false,"result":%q}`, f.result)
	}))
	mux.HandleFunc("POST /job/team/job/repoguard-ci/12/stop", auth(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.stopped = true
		f.mu.Unlock()
	}))
	mux.HandleFunc("GET /job/team/job/repoguard-ci/12/artifact/"+ReportFile, auth(func(w http.ResponseWriter, r *http.Request) {
		if f.report == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, f.report)
	}))
	mux.HandleFunc("GET /job/team/job/repoguard-ci/12/consoleText", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "+ repoguard ci --json repoguard-report.json\nFinished: "+f.result)
	}))
	return mux
}

func jenkinsFor(t *testing.T, f *fakeJenkins) *Jenkins {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return &Jenkins{URL: srv.URL, User: "bot", Token: "api-token", Job: "team/repoguard-ci", Poll: 10 * time.Millisecond, Timeout: 5 * time.Second}
}

var remoteReq = RemoteBuild{
	Repo:     platform.Repo{FullName: "grp/app"},
	CR:       &platform.ChangeRequest{Number: 5},
	CloneURL: "https://gitlab.example/grp/app.git",
	Ref:      "refs/merge-requests/5/head",
	SHA:      "abc123",
	Policy:   []byte("ci:\n  fail_on_vulns: true\n"),
}

func TestJenkinsBuildSuccess(t *testing.T) {
	f := &fakeJenkins{result: "SUCCESS", report: string(reportJSON(t, false))}
	j := jenkinsFor(t, f)
	res, err := j.Build(context.Background(), remoteReq)
	if err != nil {
		t.Fatal(err)
	}
	if f.params["GIT_SHA"] != "abc123" || f.params["GIT_REF"] != "refs/merge-requests/5/head" || f.params["REPO_NAME"] != "grp/app" {
		t.Errorf("params = %v", f.params)
	}
	if pol, _ := base64.StdEncoding.DecodeString(f.params["REPOGUARD_POLICY"]); string(pol) != string(remoteReq.Policy) {
		t.Errorf("policy param = %q", pol)
	}
	if res.ExitCode != 0 || res.Report == nil || res.Backend != "Jenkins #12" || res.URL != j.URL+"/job/team/job/repoguard-ci/12/" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Logs, "Finished: SUCCESS") {
		t.Errorf("logs = %q", res.Logs)
	}
}

func TestJenkinsBuildFailureWithoutReport(t *testing.T) {
	f := &fakeJenkins{result: "FAILURE"}
	res, err := jenkinsFor(t, f).Build(context.Background(), remoteReq)
	if err != nil || res.ExitCode != 1 || res.Report != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	state, summary, body, _ := outcome(remoteReq.CR, res)
	if state != platform.StateFailure || !strings.Contains(summary, "crashed") || !strings.Contains(body, "Built on [Jenkins #12]") {
		t.Fatalf("outcome: %s %s\n%s", state, summary, body)
	}
}

func TestJenkinsTimeoutStopsTheBuild(t *testing.T) {
	f := &fakeJenkins{neverEnds: true}
	j := jenkinsFor(t, f)
	j.Timeout = 200 * time.Millisecond
	res, err := j.Build(context.Background(), remoteReq)
	if err != nil || !res.TimedOut {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !f.stopped {
		t.Error("a timed-out build must be stopped on Jenkins")
	}
}

func TestJenkinsBadCredentials(t *testing.T) {
	j := jenkinsFor(t, &fakeJenkins{})
	j.Token = "wrong"
	if _, err := j.Build(context.Background(), remoteReq); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecutorUsesRemoteBackend(t *testing.T) {
	f := &fakeJenkins{result: "SUCCESS", report: string(reportJSON(t, false))}
	c := &fakeClient{canPush: true, policy: "ci: {}\n"}
	x := newExec(c, &fakeSandbox{}, nil)
	x.fetch = func(context.Context, platform.GitSource, string) (string, error) {
		t.Fatal("remote builds must not fetch sources locally")
		return "", nil
	}
	x.Remote = jenkinsFor(t, f)
	if err := x.Run(context.Background(), repo, cr()); err != nil {
		t.Fatal(err)
	}
	if f.params["REPO_URL"] != "https://gitlab.example/grp/app.git" || strings.Contains(f.params["REPO_URL"], "glpat") {
		t.Errorf("clone URL sent to Jenkins = %q (must carry no credentials)", f.params["REPO_URL"])
	}
	if !strings.Contains(c.comment, "Built on [Jenkins #12]") || !strings.Contains(c.comment, "Build & Tests passed") {
		t.Errorf("comment = %s", c.comment)
	}
}
