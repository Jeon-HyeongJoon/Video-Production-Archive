package archivistruntime

import (
	"errors"

	"videoarchive/internal/core/vocabulary"
)

var ErrArchivistCommandDenied = errors.New("archivist command is denied by collaboration policy")

type CommandRoute string

const (
	RouteDirectCommand CommandRoute = "DIRECT_COMMAND"
	RouteActionCard    CommandRoute = "ACTION_CARD"
)

func RouteArchivistCommand(action vocabulary.ActionClass) (CommandRoute, error) {
	if !action.Valid() || action == vocabulary.ActionManage || action == vocabulary.ActionRead {
		return "", ErrArchivistCommandDenied
	}
	if action == vocabulary.ActionGated || action == vocabulary.ActionDestructive {
		return RouteActionCard, nil
	}
	if action == vocabulary.ActionProcess {
		return RouteDirectCommand, nil
	}
	return "", ErrArchivistCommandDenied
}
