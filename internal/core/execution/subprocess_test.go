package execution

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type runnerFunc func(context.Context, string, []string) error

func (f runnerFunc) Run(ctx context.Context, executable string, arguments []string) error {
	return f(ctx, executable, arguments)
}

type verifierFunc func(string) error

func (f verifierFunc) Verify(path string) error { return f(path) }

type publisherFunc func(string, string) error

func (f publisherFunc) Publish(temporaryPath, publishedPath string) error {
	return f(temporaryPath, publishedPath)
}

// Requirement: EXE-001
func TestSubprocessReceivesExecutableAndArgumentArrayWithoutShellExpansion(t *testing.T) {
	plan := Plan{
		Executable:      "ffmpeg",
		Arguments:       []string{"-i", `D:\source\clip & delete.mp4`, "-metadata", `title=$(unsafe)`},
		TemporaryOutput: "scratch/output.partial", PublishedOutput: "archive/output.mp4",
	}
	runner := runnerFunc(func(_ context.Context, executable string, arguments []string) error {
		if executable != "ffmpeg" || !reflect.DeepEqual(arguments, plan.Arguments) {
			t.Fatalf("executable=%q arguments=%q", executable, arguments)
		}
		return nil
	})
	verifier := verifierFunc(func(string) error { return nil })
	publisher := publisherFunc(func(string, string) error { return nil })
	if err := RunVerified(context.Background(), plan, runner, verifier, publisher); err != nil {
		t.Fatal(err)
	}
}

// Requirement: EXE-002
func TestTemporaryOutputIsNotPublishedUntilVerificationPasses(t *testing.T) {
	verificationFailure := errors.New("checksum mismatch")
	plan := Plan{Executable: "ffmpeg", TemporaryOutput: "scratch/output.partial", PublishedOutput: "archive/output.mp4"}
	runner := runnerFunc(func(context.Context, string, []string) error { return nil })
	verifier := verifierFunc(func(path string) error {
		if path != plan.TemporaryOutput {
			t.Fatalf("verified %q", path)
		}
		return verificationFailure
	})
	published := false
	publisher := publisherFunc(func(string, string) error {
		published = true
		return nil
	})
	if err := RunVerified(context.Background(), plan, runner, verifier, publisher); !errors.Is(err, verificationFailure) {
		t.Fatalf("error=%v", err)
	}
	if published {
		t.Fatal("unverified temporary output was published")
	}
}
