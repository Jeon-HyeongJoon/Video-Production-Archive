package processruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"videoarchive/internal/core/archivistruntime"
)

type Runner struct{}

func New() *Runner { return &Runner{} }

func (*Runner) Run(ctx context.Context, plan archivistruntime.LaunchPlan, prompt, mcpURL string, environment map[string]string, started func() error, progress func(archivistruntime.ProgressUpdate) error) (archivistruntime.FinalResponse, error) {
	command := exec.CommandContext(ctx, plan.Executable, plan.Arguments...)
	command.Dir = plan.WorkingDirectory
	command.Env = append(os.Environ(), "ARCHIVE_MCP_URL="+mcpURL)
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Stdin = strings.NewReader(prompt)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return archivistruntime.FinalResponse{}, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return archivistruntime.FinalResponse{}, err
	}
	if err := started(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return archivistruntime.FinalResponse{}, err
	}
	scanner := bufio.NewScanner(io.LimitReader(stdout, (64<<10)+1))
	scanner.Buffer(make([]byte, 4096), 16<<10)
	var response archivistruntime.FinalResponse
	finalSeen := false
	var reportedFailure *archivistruntime.RuntimeExecutionFailure
	lineCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineCount++
		if lineCount > 128 || finalSeen || reportedFailure != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return archivistruntime.FinalResponse{}, errors.New("runtime event stream is invalid")
		}
		record, err := decodeOutputRecord([]byte(line))
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return archivistruntime.FinalResponse{}, err
		}
		switch record.Type {
		case "progress":
			update, err := archivistruntime.NewProgressUpdate(record.Summary)
			if err != nil || progress == nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return archivistruntime.FinalResponse{}, errors.New("runtime progress event is invalid")
			}
			if err := progress(update); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return archivistruntime.FinalResponse{}, err
			}
		case "", "final":
			response, err = archivistruntime.NewFinalResponse(record.Summary, record.ResultIDs)
			if err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return archivistruntime.FinalResponse{}, errors.New("runtime final response is invalid")
			}
			finalSeen = true
		case "failure":
			reportedFailure, err = archivistruntime.NewRuntimeExecutionFailure(record.Code, record.Summary)
			if err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return archivistruntime.FinalResponse{}, errors.New("runtime failure event is invalid")
			}
		default:
			_ = command.Process.Kill()
			_ = command.Wait()
			return archivistruntime.FinalResponse{}, errors.New("runtime event type is invalid")
		}
	}
	if err := scanner.Err(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return archivistruntime.FinalResponse{}, errors.New("runtime event stream exceeds limit")
	}
	if err := command.Wait(); err != nil {
		return archivistruntime.FinalResponse{}, err
	}
	if reportedFailure != nil {
		return archivistruntime.FinalResponse{}, reportedFailure
	}
	if !finalSeen {
		return archivistruntime.FinalResponse{}, errors.New("runtime final response is missing")
	}
	return response, nil
}

type outputRecord struct {
	Type      string   `json:"type,omitempty"`
	Code      string   `json:"code,omitempty"`
	Summary   string   `json:"summary"`
	ResultIDs []string `json:"result_ids,omitempty"`
}

func decodeOutputRecord(payload []byte) (outputRecord, error) {
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var record outputRecord
	if err := decoder.Decode(&record); err != nil {
		return outputRecord{}, errors.New("runtime event is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return outputRecord{}, errors.New("runtime event contains trailing data")
	}
	return record, nil
}
