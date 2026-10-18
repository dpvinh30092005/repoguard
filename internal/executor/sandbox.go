package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"
)

// ReportFile is written by `repoguard ci --json` inside the sandbox.
const ReportFile = "repoguard-report.json"

// Result of one sandboxed run.
type Result struct {
	Report   []byte // contents of ReportFile; nil if the run never produced one
	Logs     string // tail of combined stdout/stderr
	ExitCode int
	TimedOut bool
	Backend  string // set by remote backends, e.g. "Jenkins #12"
	URL      string // link to the remote build
}

// Sandbox runs `repoguard ci` against a source directory in isolation.
// env holds the only variables the build may see (e.g. SONAR_TOKEN).
type Sandbox interface {
	Run(ctx context.Context, srcDir string, env map[string]string) (Result, error)
}

// Docker runs each job in a fresh, resource-limited container that is
// removed afterwards. It never bind-mounts host paths: sources are copied in
// with `docker cp`, so it also works when repoguard itself runs in a
// container talking to the host's Docker socket.
type Docker struct {
	Bin       string // docker CLI, default "docker"
	Image     string
	CPUs      string // e.g. "2"
	Memory    string // e.g. "4g"
	PidsLimit int
	Network   string        // "bridge" (deps need the registry) or "none"
	Runtime   string        // optional, e.g. "runsc" (gVisor) for stronger isolation
	Timeout   time.Duration // wall clock for the whole build
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// CreateArgs builds `docker create` arguments. No mounts and no host env: the
// container sees the copied sources plus the named variables, which are
// passed as `--env NAME` (value taken from the docker CLI's environment) so
// secret values never appear in argv.
func (d Docker) CreateArgs(name string, envNames []string) []string {
	args := []string{
		"create", "--name", name,
		"--label", "repoguard.executor=1",
		"--cpus", d.CPUs,
		"--memory", d.Memory, "--memory-swap", d.Memory,
		"--pids-limit", strconv.Itoa(d.PidsLimit),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", d.Network,
		"--workdir", "/work",
		"--env", "CI=true",
		"--env", "HOME=/tmp",
	}
	for _, n := range envNames {
		args = append(args, "--env", n)
	}
	if d.Runtime != "" {
		args = append(args, "--runtime", d.Runtime)
	}
	return append(args, d.Image,
		"repoguard", "ci", "--dir", "/work", "--json", ReportFile, "--report", "", "--junit", "")
}

func (d Docker) Run(ctx context.Context, srcDir string, env map[string]string) (Result, error) {
	name := "repoguard-" + randomID()
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	create := d.cmd(ctx, d.CreateArgs(name, names)...)
	create.Env = os.Environ()
	for _, k := range names {
		create.Env = append(create.Env, k+"="+env[k])
	}
	if out, err := create.CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("docker create: %v: %s", err, bytes.TrimSpace(out))
	}
	defer func() {
		// Background context: clean up even if the job was cancelled.
		_ = d.cmd(context.Background(), "rm", "-f", name).Run()
	}()

	if out, err := d.cmd(ctx, "cp", srcDir+"/.", name+":/work").CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("docker cp (in): %v: %s", err, bytes.TrimSpace(out))
	}

	runCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	logs := &tailBuffer{max: 64 << 10}
	start := d.cmd(runCtx, "start", "--attach", name)
	start.Stdout, start.Stderr = logs, logs
	err := start.Run()

	res := Result{Logs: logs.String()}
	if runCtx.Err() == context.DeadlineExceeded {
		_ = d.cmd(context.Background(), "kill", name).Run()
		res.TimedOut = true
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode() // `start --attach` exits with the container's code
	case err != nil:
		return res, fmt.Errorf("docker start: %w", err)
	}

	// `docker cp <ctr>:<file> -` streams a tar archive.
	archive, err := d.cmd(ctx, "cp", name+":/work/"+ReportFile, "-").Output()
	if err == nil {
		res.Report, _ = firstFileFromTar(archive)
	}
	return res, nil
}

func (d Docker) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, d.bin(), args...)
}

func firstFileFromTar(data []byte) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 10<<20))
		}
	}
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
