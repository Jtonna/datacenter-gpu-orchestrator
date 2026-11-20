package devices

import (
	"context"
	"os"
	"os/exec"
)

// ExecRunner runs the program for real and returns its combined output.
func ExecRunner(ctx context.Context, path string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}
