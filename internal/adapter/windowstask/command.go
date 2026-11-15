package windowstask

import (
	"context"
	"os/exec"
)

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, executable string, arguments ...string) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	return command.Run()
}
