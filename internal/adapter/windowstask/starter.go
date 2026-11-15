package windowstask

import (
	"context"
	"errors"
	"strings"

	"videoarchive/internal/application/archivisthostactivation"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

type Starter struct {
	taskNames map[string]string
	runner    CommandRunner
}

func New(taskNames map[string]string, runner CommandRunner) (*Starter, error) {
	if runner == nil {
		return nil, errors.New("Windows task command runner is required")
	}
	copyNames := make(map[string]string, len(taskNames))
	for rawID, rawName := range taskNames {
		id := strings.TrimSpace(rawID)
		name := strings.TrimSpace(rawName)
		if id == "" || name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("Windows task mapping is invalid")
		}
		copyNames[id] = name
	}
	return &Starter{taskNames: copyNames, runner: runner}, nil
}

func (starter *Starter) Start(ctx context.Context, archivistHostID string) error {
	taskName := starter.taskNames[strings.TrimSpace(archivistHostID)]
	if taskName == "" {
		return archivisthostactivation.ErrHostStartUnmanaged
	}
	return starter.runner.Run(ctx, "schtasks.exe", "/Run", "/TN", taskName)
}
