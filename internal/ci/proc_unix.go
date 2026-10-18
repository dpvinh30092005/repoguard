//go:build !windows

package ci

import (
	"context"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// startBackground runs command in its own process group so stop() also
// terminates anything it spawned (npm start -> node, etc.).
func startBackground(_ context.Context, dir, command string, out io.Writer) (func(), error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}, nil
}
