package processlog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/execution"
)

const directory = "process"

type Policy struct {
	MaxFileBytes  int64
	TailBytes     int
	Retention     time.Duration
	MaxTotalBytes int64
}

func DefaultPolicy() Policy {
	return Policy{MaxFileBytes: 4 << 20, TailBytes: 12 << 10, Retention: 14 * 24 * time.Hour, MaxTotalBytes: 256 << 20}
}

func (policy Policy) valid() bool {
	return policy.MaxFileBytes >= 1024 && policy.MaxFileBytes <= 64<<20 &&
		policy.TailBytes >= 512 && policy.TailBytes <= 16<<10 && policy.Retention >= time.Hour &&
		policy.MaxTotalBytes >= policy.MaxFileBytes && policy.MaxTotalBytes <= 16<<30
}

type Runner struct {
	root   *archive.StorageRoot
	policy Policy
	now    func() time.Time
}

func NewRunner(root *archive.StorageRoot, policy Policy) (*Runner, error) {
	if root == nil || !policy.valid() {
		return nil, errors.New("process log runner configuration is invalid")
	}
	return &Runner{root: root, policy: policy, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (runner *Runner) Run(ctx context.Context, executable string, arguments []string) error {
	return runner.execute(ctx, executable, arguments, nil)
}

// Output implements the ffprobe Runner contract while retaining the same
// bounded failure evidence as processes that publish files.
func (runner *Runner) Output(ctx context.Context, executable string, arguments []string) ([]byte, error) {
	var output bytes.Buffer
	if err := runner.execute(ctx, executable, arguments, &output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (runner *Runner) execute(ctx context.Context, executable string, arguments []string, output io.Writer) error {
	boundary, ok := execution.ProcessContextFrom(ctx)
	if runner == nil || runner.root == nil || !ok || strings.TrimSpace(executable) == "" {
		return errors.New("process log execution boundary is invalid")
	}
	_ = runner.prune()
	logID := processLogID(boundary)
	relative := path.Join(directory, logID+".log")
	_ = runner.root.MkdirAll(directory, 0o700)
	file, createErr := runner.root.Create(relative, 0o600)
	writer := newBoundedWriter(file, runner.policy.MaxFileBytes, runner.policy.TailBytes)
	command := exec.CommandContext(ctx, executable, append([]string(nil), arguments...)...)
	stdout := io.Writer(streamWriter{target: writer, label: "stdout"})
	if output != nil {
		stdout = io.MultiWriter(output, stdout)
	}
	command.Stdout = stdout
	command.Stderr = streamWriter{target: writer, label: "stderr"}
	runErr := command.Run()
	if runErr != nil {
		_, _ = fmt.Fprintf(writer, "[process] error=%s\n", runErr)
	}
	closeErr := writer.close()
	if runErr == nil {
		if createErr == nil {
			_ = runner.root.Remove(relative)
		}
		return nil
	}
	artifact := execution.ProcessLogArtifact{
		ID: logID, Reference: "evidence://process-log/" + logID,
		State: execution.ProcessLogCaptureFailed, CapturedBytes: writer.captured(), TotalBytes: writer.total(),
		Truncated: writer.total() > writer.captured(), SanitizedTail: SanitizeTail(writer.tail()),
	}
	if createErr == nil && closeErr == nil {
		captured, digest, digestErr := runner.root.FileSHA256(relative)
		if digestErr == nil && captured == writer.captured() && captured > 0 {
			artifact.State = execution.ProcessLogAvailable
			artifact.Digest = digest
		}
	}
	if artifact.Validate() != nil {
		artifact = execution.ProcessLogArtifact{
			ID: logID, Reference: "evidence://process-log/" + logID,
			State: execution.ProcessLogCaptureFailed, CapturedBytes: 0, TotalBytes: max64(writer.total(), 0),
			Truncated: writer.total() > 0, SanitizedTail: SanitizeTail(writer.tail()),
		}
	}
	return &execution.ProcessFailure{Cause: runErr, Artifact: artifact}
}

func processLogID(boundary execution.ProcessContext) string {
	digest := sha256.Sum256([]byte(boundary.JobID + "\x00" + boundary.StepID + "\x00" + string(boundary.Operation) + "\x00" + formatUint(boundary.FencingToken)))
	return "PLOG-" + strings.ToUpper(hex.EncodeToString(digest[:12]))
}

func formatUint(value uint64) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}

type boundedWriter struct {
	mu       sync.Mutex
	file     *os.File
	max      int64
	tailMax  int
	tailData []byte
	written  int64
	totalN   int64
	writeErr error
}

func newBoundedWriter(file *os.File, max int64, tailMax int) *boundedWriter {
	return &boundedWriter{file: file, max: max, tailMax: tailMax, tailData: make([]byte, 0, tailMax)}
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.totalN += int64(len(data))
	writer.appendTail(data)
	if writer.file == nil || writer.writeErr != nil || writer.written >= writer.max {
		return len(data), nil
	}
	allowed := int64(len(data))
	if allowed > writer.max-writer.written {
		allowed = writer.max - writer.written
	}
	written, err := writer.file.Write(data[:allowed])
	writer.written += int64(written)
	if err != nil || int64(written) != allowed {
		writer.writeErr = errors.Join(err, io.ErrShortWrite)
		return len(data), nil
	}
	return len(data), nil
}

func (writer *boundedWriter) appendTail(data []byte) {
	if len(data) >= writer.tailMax {
		writer.tailData = append(writer.tailData[:0], data[len(data)-writer.tailMax:]...)
		return
	}
	needed := len(writer.tailData) + len(data) - writer.tailMax
	if needed > 0 {
		copy(writer.tailData, writer.tailData[needed:])
		writer.tailData = writer.tailData[:len(writer.tailData)-needed]
	}
	writer.tailData = append(writer.tailData, data...)
}

func (writer *boundedWriter) close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		return errors.New("process log file is unavailable")
	}
	err := errors.Join(writer.writeErr, writer.file.Sync(), writer.file.Close())
	writer.file = nil
	return err
}

func (writer *boundedWriter) captured() int64 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.written
}
func (writer *boundedWriter) total() int64 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.totalN
}
func (writer *boundedWriter) tail() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.tailData...)
}

type streamWriter struct {
	target io.Writer
	label  string
}

func (writer streamWriter) Write(data []byte) (int, error) {
	prefixed := make([]byte, 0, len(writer.label)+3+len(data))
	prefixed = append(prefixed, '[')
	prefixed = append(prefixed, writer.label...)
	prefixed = append(prefixed, ']', ' ')
	prefixed = append(prefixed, data...)
	_, err := writer.target.Write(prefixed)
	return len(data), err
}

var (
	secretPattern      = regexp.MustCompile(`(?i)(authorization|bearer|token|password|passwd|secret|api[_-]?key|cookie)(\s*[:=]\s*|\s+)[^\s,;]+`)
	windowsPathPattern = regexp.MustCompile(`(?i)\b[A-Z]:\\[^\r\n\t]*`)
	unixPathPattern    = regexp.MustCompile(`(^|[\s=:])/(?:Users|home|var|tmp|private|mnt|media|Volumes|opt|srv|etc)/[^\r\n\t ]*`)
)

func SanitizeTail(raw []byte) string {
	value := strings.ToValidUTF8(string(raw), "�")
	value = strings.ReplaceAll(value, "\x00", "")
	value = secretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	value = windowsPathPattern.ReplaceAllString(value, "[PATH]")
	value = unixPathPattern.ReplaceAllString(value, "$1[PATH]")
	lines := strings.Split(value, "\n")
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	value = strings.TrimSpace(strings.Join(lines, "\n"))
	if len(value) > 16<<10 {
		value = value[len(value)-(16<<10):]
		for !utf8.ValidString(value) && len(value) > 0 {
			value = value[1:]
		}
	}
	return value
}

func (runner *Runner) prune() error {
	entries, err := runner.root.ReadDir(directory)
	if err != nil {
		return nil
	}
	type candidate struct {
		name string
		size int64
		mod  time.Time
	}
	items := make([]candidate, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "PLOG-") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		item := candidate{name: entry.Name(), size: info.Size(), mod: info.ModTime()}
		if runner.now().Sub(item.mod) > runner.policy.Retention {
			_ = runner.root.Remove(path.Join(directory, item.name))
			continue
		}
		total += item.size
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	limit := runner.policy.MaxTotalBytes - runner.policy.MaxFileBytes
	for _, item := range items {
		if total <= limit {
			break
		}
		if runner.root.Remove(path.Join(directory, item.name)) == nil {
			total -= item.size
		}
	}
	return nil
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
