package execution

import (
	"context"
	"io"
)

// StreamRunner also copies process output to local protocol parsers. A nil
// observer leaves that stream under the runner's usual retention policy.
type StreamRunner interface {
	Runner
	RunStreams(context.Context, string, []string, io.Writer, io.Writer) error
}
