package capability

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"regexp"

	"videoarchive/internal/core/command"
)

// CommandObserver retains one invocation per attempt. Replayed commands share
// the CommandID while the underlying store remains the authority for effects.
type CommandObserver struct {
	port *ObservedPort
	next CommandBus
	job  bool
}
type commandPort struct{ next CommandBus }

func (port commandPort) Execute(_ context.Context, input Input) (Receipt, error) {
	r, err := port.next.Execute(input.Command)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Command: r, Reference: Reference{ResultID: r.TargetID}}, nil
}
func NewCommandObserver(next CommandBus, sink EvidenceSink, job bool) (*CommandObserver, error) {
	if next == nil {
		return nil, ErrInvalidEvidence
	}
	port, err := NewObservedPort(commandPort{next}, sink)
	if err != nil {
		return nil, err
	}
	return &CommandObserver{port: port, next: next, job: job}, nil
}
func (observer *CommandObserver) Execute(envelope command.Envelope) (command.Result, error) {
	if envelope.Validate() != nil {
		return observer.next.Execute(envelope)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return command.Result{}, err
	}
	c := Correlation{RunID: fmt.Sprintf("EVD-CAP-%x", key)}
	if observer.job {
		c.JobID = envelope.TargetID
	}
	c.StepID, _ = envelope.Parameters["step_id"].(string)
	c.WorkerID, _ = envelope.Parameters["worker_id"].(string)
	if drive, _ := envelope.Parameters["workspace_drive_id"].(string); drive != "" {
		c.WorkspaceDriveIDs = []string{drive}
	}
	result, err := observer.port.Execute(context.Background(), Input{Name: envelope.CommandType, Version: 1, Command: envelope, Correlation: c})
	return result.Command, err
}

var referencePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,192}$`)

func safeReference(id string) string {
	if id == "" || referencePattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("REF-%x", sum[:12])
}
