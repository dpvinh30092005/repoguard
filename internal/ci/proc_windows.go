//go:build windows

package ci

import (
	"context"
	"io"
	"os/exec"
)

func startBackground(_ context.Context, dir, command string, out io.Writer) (func(), error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}
