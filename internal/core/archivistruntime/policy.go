package archivistruntime

import (
	"errors"

	"videoarchive/internal/core/vocabulary"
	"videoarchive/internal/core/workflow"
)

var ErrArchivistCommandDenied = errors.New("archivist command is denied by collaboration policy")

type CommandRoute string

const (
	RouteDirectCommand CommandRoute = "DIRECT_COMMAND"
	RouteActionCard    CommandRoute = "ACTION_CARD"
)

func RouteArchivistCommand(mode workflow.ControlMode, action vocabulary.ActionClass) (CommandRoute, error) {
	if mode == workflow.ManualMode || !action.Valid() || action == vocabulary.ActionManage || action == vocabulary.ActionRead {
		return "", ErrArchivistCommandDenied
	}
	if mode == workflow.AssistedMode || action == vocabulary.ActionGated || action == vocabulary.ActionDestructive {
		return RouteActionCard, nil
	}
	if mode == workflow.ArchivistMode && action == vocabulary.ActionProcess {
		return RouteDirectCommand, nil
	}
	return "", ErrArchivistCommandDenied
}
