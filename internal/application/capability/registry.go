package capability

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"videoarchive/internal/core/command"
)

var (
	ErrInvalidDefinition   = errors.New("invalid capability definition")
	ErrDuplicateDefinition = errors.New("capability definition already registered")
	ErrCapabilityNotFound  = errors.New("capability definition was not found")
	ErrInvalidReference    = errors.New("invalid capability result reference")
)

type Kind string

const (
	Immediate   Kind = "IMMEDIATE"
	LongRunning Kind = "LONG_RUNNING"
)

func (kind Kind) valid() bool {
	return kind == Immediate || kind == LongRunning
}

// CommandBus is the protocol-independent state change boundary used by every
// capability registered in a Registry.
type CommandBus interface {
	Execute(command.Envelope) (command.Result, error)
}

// Reference identifies the result produced by a capability. Immediate work
// returns ResultID. Long-running work returns the Job and Step handles that can
// be observed after the command has been accepted.
type Reference struct {
	ResultID               string
	JobID                  string
	StepID                 string
	TaskID                 string
	VerificationReferences []string
}

type ProjectReference func(command.Result) (Reference, error)

type Definition struct {
	Name             string
	Version          uint32
	Kind             Kind
	ProjectReference ProjectReference
}

type Input struct {
	Name        string
	Version     uint32
	Command     command.Envelope
	Correlation Correlation
}

type Correlation struct {
	RunID             string
	JobID             string
	StepID            string
	WorkerID          string
	WorkspaceDriveIDs []string
	InputIDs          []string
}

// Receipt is returned by the direct Application Capability Port. It contains
// the Command Bus acknowledgement and a protocol-independent result handle.
type Receipt struct {
	CapabilityName    string
	CapabilityVersion uint32
	Command           command.Result
	Reference         Reference
}

type definitionKey struct {
	name    string
	version uint32
}

// Port is the direct execution boundary shared by CLI, schedulers, tests and
// protocol adapters.
type Port interface {
	Execute(context.Context, Input) (Receipt, error)
}

type Registry struct {
	mu          sync.RWMutex
	bus         CommandBus
	definitions map[definitionKey]Definition
}

var _ Port = (*Registry)(nil)

func NewRegistry(bus CommandBus) (*Registry, error) {
	if bus == nil {
		return nil, ErrInvalidDefinition
	}
	return &Registry{bus: bus, definitions: make(map[definitionKey]Definition)}, nil
}

func (registry *Registry) Register(definition Definition) error {
	if definition.Name == "" || definition.Version == 0 || !definition.Kind.valid() || definition.ProjectReference == nil {
		return ErrInvalidDefinition
	}

	key := definitionKey{name: definition.Name, version: definition.Version}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.definitions[key]; exists {
		return ErrDuplicateDefinition
	}
	registry.definitions[key] = definition
	return nil
}

func (registry *Registry) Execute(ctx context.Context, input Input) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}

	key := definitionKey{name: input.Name, version: input.Version}
	registry.mu.RLock()
	definition, exists := registry.definitions[key]
	registry.mu.RUnlock()
	if !exists {
		return Receipt{}, ErrCapabilityNotFound
	}

	result, err := registry.bus.Execute(input.Command)
	if err != nil {
		return Receipt{}, err
	}
	reference, err := definition.ProjectReference(result)
	if err != nil {
		return Receipt{}, fmt.Errorf("project capability reference: %w", err)
	}
	if err := validateReference(definition.Kind, reference); err != nil {
		return Receipt{}, err
	}

	return Receipt{
		CapabilityName:    definition.Name,
		CapabilityVersion: definition.Version,
		Command:           result,
		Reference:         reference,
	}, nil
}

func validateReference(kind Kind, reference Reference) error {
	switch kind {
	case Immediate:
		if reference.ResultID == "" || reference.JobID != "" || reference.StepID != "" || reference.TaskID != "" {
			return ErrInvalidReference
		}
	case LongRunning:
		if reference.ResultID != "" || reference.JobID == "" || reference.StepID == "" {
			return ErrInvalidReference
		}
	default:
		return ErrInvalidReference
	}
	return nil
}
