package execution

import (
	"context"
	"errors"
)

var ErrInvalidPlan = errors.New("invalid subprocess plan")

type Plan struct {
	Executable      string
	Arguments       []string
	TemporaryOutput string
	PublishedOutput string
}

func (p Plan) Validate() error {
	if p.Executable == "" || p.TemporaryOutput == "" || p.PublishedOutput == "" || p.TemporaryOutput == p.PublishedOutput {
		return ErrInvalidPlan
	}
	return nil
}

type Runner interface {
	Run(context.Context, string, []string) error
}

type Verifier interface {
	Verify(string) error
}

type Publisher interface {
	Publish(temporaryPath, publishedPath string) error
}

// RunVerified executes without a shell and keeps output private until it passes
// verification. Adapters publish with an atomic rename when the filesystem allows it.
func RunVerified(ctx context.Context, plan Plan, runner Runner, verifier Verifier, publisher Publisher) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if err := runner.Run(ctx, plan.Executable, append([]string(nil), plan.Arguments...)); err != nil {
		return err
	}
	if err := verifier.Verify(plan.TemporaryOutput); err != nil {
		return err
	}
	return publisher.Publish(plan.TemporaryOutput, plan.PublishedOutput)
}
