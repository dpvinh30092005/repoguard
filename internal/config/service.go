package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Service is the configuration of `repoguard serve`, read from the environment.
type Service struct {
	Addr          string
	Workers       int
	QueueSize     int
	SweepInterval time.Duration // 0 disables the stale/SLA sweeper
	PublicURL     string        // optional link shown on commit statuses

	GitHub   GitHub
	GitLab   GitLab
	AI       AI
	Executor Executor
}

// Executor runs `repoguard ci` for repos whose own CI is unavailable. Off by default.
type Executor struct {
	Mode        string   // "" (auto: docker if found) | "docker" (required) | "jenkins" | "off"
	Repos       []string // repo globs allowed via env, e.g. "team/*" (the UI can allow more)
	Image       string
	CPUs        string
	Memory      string
	PidsLimit   int
	Network     string
	Runtime     string // optional container runtime, e.g. runsc (gVisor)
	Timeout     time.Duration
	Concurrency int
	Jenkins     Jenkins
	Sonar       SonarServer // handed to sandboxed builds so `repoguard ci` can run sonar-scanner
}

// Jenkins is the remote build backend (REPOGUARD_EXECUTOR=jenkins).
type Jenkins struct {
	URL   string
	User  string
	Token string
	Job   string
}

// SonarServer is where builds send analyses (SONAR_HOST_URL / SONAR_TOKEN).
type SonarServer struct {
	HostURL string
	Token   string
}

// Disabled reports an explicit opt-out; Required means startup fails without Docker.
func (e Executor) Disabled() bool   { return e.Mode == "off" }
func (e Executor) Required() bool   { return e.Mode == "docker" }
func (e Executor) UseJenkins() bool { return e.Mode == "jenkins" }

type GitHub struct {
	APIURL        string // https://api.github.com or https://ghe.example.com/api/v3
	AppID         int64
	PrivateKeyPEM []byte
	Token         string // alternative to an App: a bot PAT
	WebhookSecret string
}

func (g GitHub) Enabled() bool { return g.WebhookSecret != "" && (g.AppID != 0 || g.Token != "") }

type GitLab struct {
	URL           string // https://gitlab.com or self-managed
	Token         string // bot user / group access token with `api` scope
	WebhookSecret string
}

func (g GitLab) Enabled() bool { return g.Token != "" && g.WebhookSecret != "" }

type AI struct {
	APIKey string
	Model  string
	Effort string
}

func (a AI) Enabled() bool { return a.APIKey != "" }

// LoadService reads settings from the environment (12-factor).
func LoadService() (Service, error) {
	s := Service{
		Addr:      env("REPOGUARD_ADDR", ":47474"),
		PublicURL: os.Getenv("REPOGUARD_PUBLIC_URL"),
		GitHub: GitHub{
			APIURL:        strings.TrimRight(env("GITHUB_API_URL", "https://api.github.com"), "/"),
			Token:         os.Getenv("GITHUB_TOKEN"),
			WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		},
		GitLab: GitLab{
			URL:           strings.TrimRight(env("GITLAB_URL", "https://gitlab.com"), "/"),
			Token:         os.Getenv("GITLAB_TOKEN"),
			WebhookSecret: os.Getenv("GITLAB_WEBHOOK_SECRET"),
		},
		AI: AI{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  env("REPOGUARD_AI_MODEL", "claude-opus-5-5"),
			Effort: env("REPOGUARD_AI_EFFORT", "high"),
		},
	}
	var err error
	if s.Workers, err = envInt("REPOGUARD_WORKERS", 8); err != nil {
		return s, err
	}
	if s.QueueSize, err = envInt("REPOGUARD_QUEUE_SIZE", 1000); err != nil {
		return s, err
	}
	if s.SweepInterval, err = time.ParseDuration(env("REPOGUARD_SWEEP_INTERVAL", "6h")); err != nil {
		return s, fmt.Errorf("REPOGUARD_SWEEP_INTERVAL: %w", err)
	}
	if v := os.Getenv("GITHUB_APP_ID"); v != "" {
		if s.GitHub.AppID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return s, fmt.Errorf("GITHUB_APP_ID: %w", err)
		}
	}
	if pem := os.Getenv("GITHUB_APP_PRIVATE_KEY"); pem != "" {
		s.GitHub.PrivateKeyPEM = []byte(strings.ReplaceAll(pem, `\n`, "\n"))
	} else if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		if s.GitHub.PrivateKeyPEM, err = os.ReadFile(path); err != nil {
			return s, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
	}
	if s.GitHub.AppID != 0 && len(s.GitHub.PrivateKeyPEM) == 0 {
		return s, fmt.Errorf("GITHUB_APP_ID is set but no GITHUB_APP_PRIVATE_KEY(_FILE)")
	}
	if err := loadExecutor(&s.Executor); err != nil {
		return s, err
	}
	// No platform configured is fine: it can be connected from the web UI.
	return s, nil
}

func loadExecutor(x *Executor) error {
	*x = Executor{
		Mode:    os.Getenv("REPOGUARD_EXECUTOR"),
		Image:   os.Getenv("REPOGUARD_EXECUTOR_IMAGE"),
		CPUs:    env("REPOGUARD_EXECUTOR_CPUS", "2"),
		Memory:  env("REPOGUARD_EXECUTOR_MEMORY", "4g"),
		Network: env("REPOGUARD_EXECUTOR_NETWORK", "bridge"),
		Runtime: os.Getenv("REPOGUARD_EXECUTOR_RUNTIME"),
		Jenkins: Jenkins{
			URL:   strings.TrimRight(os.Getenv("JENKINS_URL"), "/"),
			User:  os.Getenv("JENKINS_USER"),
			Token: os.Getenv("JENKINS_TOKEN"),
			Job:   env("JENKINS_JOB", "repoguard-ci"),
		},
		Sonar: SonarServer{HostURL: os.Getenv("SONAR_HOST_URL"), Token: os.Getenv("SONAR_TOKEN")},
	}
	for _, r := range strings.Split(os.Getenv("REPOGUARD_EXECUTOR_REPOS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			x.Repos = append(x.Repos, r)
		}
	}
	var err error
	if x.Timeout, err = time.ParseDuration(env("REPOGUARD_EXECUTOR_TIMEOUT", "20m")); err != nil {
		return fmt.Errorf("REPOGUARD_EXECUTOR_TIMEOUT: %w", err)
	}
	if x.Concurrency, err = envInt("REPOGUARD_EXECUTOR_CONCURRENCY", 2); err != nil {
		return err
	}
	if x.PidsLimit, err = envInt("REPOGUARD_EXECUTOR_PIDS_LIMIT", 1024); err != nil {
		return err
	}
	switch x.Mode {
	case "", "docker", "off":
	case "jenkins":
		if x.Jenkins.URL == "" || x.Jenkins.User == "" || x.Jenkins.Token == "" {
			return fmt.Errorf("REPOGUARD_EXECUTOR=jenkins needs JENKINS_URL, JENKINS_USER and JENKINS_TOKEN")
		}
	default:
		return fmt.Errorf("REPOGUARD_EXECUTOR must be empty, \"docker\", \"jenkins\" or \"off\", got %q", x.Mode)
	}
	// Fail closed regardless of mode: nothing is built until a repository is
	// allowed via REPOGUARD_EXECUTOR_REPOS or switched on in the UI.
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}
