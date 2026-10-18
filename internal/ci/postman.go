package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/glob"
)

const maxAPIFailures = 20

// APITests is the Postman/Newman part of the report.
type APITests struct {
	Ran     bool            `json:"ran"`
	Note    string          `json:"note,omitempty"`
	Results []APITestResult `json:"results,omitempty"`
}

type APITestResult struct {
	Collection string        `json:"collection"` // path relative to the repo
	Name       string        `json:"name"`
	Requests   int           `json:"requests"`
	Assertions int           `json:"assertions"`
	Failed     int           `json:"failed"`
	Failures   []APIFailure  `json:"failures,omitempty"`
	Error      string        `json:"error,omitempty"` // newman could not run the collection
	Duration   time.Duration `json:"duration_ns"`
}

type APIFailure struct {
	Request string `json:"request"`
	Test    string `json:"test"`
	Message string `json:"message"`
}

func (a APITests) Failed() bool {
	for _, r := range a.Results {
		if r.Failed > 0 || r.Error != "" {
			return true
		}
	}
	return false
}

// findCollections returns repo-relative paths of Postman collections.
func findCollections(dir string, patterns, ignore []string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (skipDirs[d.Name()] && d.Name() != "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if glob.Any(ignore, rel) {
			return nil
		}
		if len(patterns) > 0 {
			if glob.Any(patterns, rel) {
				out = append(out, rel)
			}
		} else if strings.HasSuffix(rel, ".postman_collection.json") {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// runPostman starts the API if configured, then runs each collection with Newman.
func runPostman(ctx context.Context, opts Options, cfg config.Postman, ignore []string) APITests {
	if !cfg.Enabled {
		return APITests{}
	}
	collections := findCollections(opts.Dir, cfg.Collections, ignore)
	if len(collections) == 0 {
		return APITests{}
	}
	if !opts.LookCmd("newman") {
		return APITests{Note: "newman not installed (npm install -g newman): skipped"}
	}

	res := APITests{Ran: true}
	if cfg.Start != "" {
		fmt.Fprintf(opts.Out, "\n==> [postman] starting API: %s\n", cfg.Start)
		stop, err := opts.Start(ctx, opts.Dir, cfg.Start, opts.Out)
		if err != nil {
			return failAll(collections, "could not start the API: "+err.Error())
		}
		defer stop()
		if cfg.WaitURL != "" {
			if err := waitHTTP(ctx, cfg.WaitURL, time.Duration(cfg.WaitSeconds)*time.Second); err != nil {
				return failAll(collections, err.Error())
			}
		}
	}

	tmp, err := os.MkdirTemp("", "repoguard-newman-")
	if err != nil {
		return failAll(collections, err.Error())
	}
	defer os.RemoveAll(tmp)

	for i, coll := range collections {
		out := filepath.Join(tmp, fmt.Sprintf("run-%d.json", i))
		cmd := "newman run " + shellQuote(coll) + " --color off --reporters cli,json --reporter-json-export " + shellQuote(out)
		if cfg.Environment != "" {
			cmd += " --environment " + shellQuote(cfg.Environment)
		}
		fmt.Fprintf(opts.Out, "\n==> [postman] %s\n", cmd)
		start := time.Now()
		runErr := opts.Runner(ctx, opts.Dir, cmd, opts.Out) // exits 1 on failed assertions: the report says why
		r := parseNewman(out, coll)
		r.Duration = time.Since(start)
		if r.Error != "" && runErr != nil {
			r.Error = fmt.Sprintf("%s (%v)", r.Error, runErr)
		}
		res.Results = append(res.Results, r)
	}
	return res
}

func failAll(collections []string, msg string) APITests {
	res := APITests{Ran: true}
	for _, c := range collections {
		res.Results = append(res.Results, APITestResult{Collection: c, Name: c, Error: msg})
	}
	return res
}

// parseNewman reads Newman's JSON reporter output.
func parseNewman(path, collection string) APITestResult {
	r := APITestResult{Collection: collection, Name: collection}
	data, err := os.ReadFile(path)
	if err != nil {
		r.Error = "newman produced no report"
		return r
	}
	var rep struct {
		Collection struct {
			Info struct {
				Name string `json:"name"`
			} `json:"info"`
		} `json:"collection"`
		Run struct {
			Stats struct {
				Requests   struct{ Total int } `json:"requests"`
				Assertions struct{ Total int } `json:"assertions"`
			} `json:"stats"`
			Failures []struct {
				Error struct {
					Name    string `json:"name"`
					Test    string `json:"test"`
					Message string `json:"message"`
				} `json:"error"`
				Source struct {
					Name string `json:"name"`
				} `json:"source"`
			} `json:"failures"`
		} `json:"run"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		r.Error = "unreadable newman report: " + err.Error()
		return r
	}
	if n := rep.Collection.Info.Name; n != "" {
		r.Name = n
	}
	r.Requests = rep.Run.Stats.Requests.Total
	r.Assertions = rep.Run.Stats.Assertions.Total
	r.Failed = len(rep.Run.Failures)
	for i, f := range rep.Run.Failures {
		if i >= maxAPIFailures {
			break
		}
		test := f.Error.Test
		if test == "" {
			test = f.Error.Name
		}
		r.Failures = append(r.Failures, APIFailure{Request: f.Source.Name, Test: test, Message: f.Error.Message})
	}
	return r
}

// waitHTTP polls url until it answers with a non-5xx status.
func waitHTTP(ctx context.Context, url string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("wait_url: %w", err)
		}
		if resp, err := client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("API did not answer at %s within %s", url, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
