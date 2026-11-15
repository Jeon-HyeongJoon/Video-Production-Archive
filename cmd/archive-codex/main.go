package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"videoarchive/internal/adapter/archivistmcpclient"
	"videoarchive/internal/adapter/codexcli"
	"videoarchive/internal/application/archivistloop"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "usage: archive-codex run|mcp|version")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch arguments[0] {
	case "version", "--version", "-version":
		fmt.Println(version)
		return 0
	case "run":
		return runTurn(ctx, arguments[1:])
	case "mcp":
		return serveMCP(ctx)
	default:
		fmt.Fprintln(os.Stderr, "unknown archive-codex command")
		return 2
	}
}

// runTurn is the CLI an Archivist Host launches for one ArchivistTurn: the
// prompt arrives on stdin and runtime records leave on stdout.
func runTurn(ctx context.Context, arguments []string) int {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	codex := set.String("codex", "", "absolute path of the Codex CLI")
	stateDirectory := set.String("state-dir", "", "absolute directory for Codex native thread references")
	model := set.String("model", "", "Codex model; Codex default when empty")
	maximumCalls := set.Int("max-tool-calls", 16, "maximum Archive tool calls")
	logPath := set.String("log-file", "", "optional diagnostic log without prompt or model text")
	if set.Parse(arguments) != nil || set.NArg() != 0 || !filepath.IsAbs(strings.TrimSpace(*codex)) ||
		!filepath.IsAbs(strings.TrimSpace(*stateDirectory)) ||
		*maximumCalls < 1 || *maximumCalls > 64 {
		fmt.Fprintln(os.Stderr, "invalid archive-codex run options")
		return 2
	}
	runtimeContext, ok := runtimeFromEnvironment()
	if !ok {
		fmt.Fprintln(os.Stderr, "Archivist runtime context is incomplete")
		return 2
	}
	promptBytes, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil || len(promptBytes) == 0 || len(promptBytes) > 64<<10 {
		fmt.Fprintln(os.Stderr, "Archivist prompt is invalid")
		return 2
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "archive-codex executable path is unavailable")
		return 2
	}
	if !codexLoginAvailable(ctx, strings.TrimSpace(*codex)) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type": "failure", "code": "CODEX_LOGIN_REQUIRED", "summary": "Codex 로그인이 필요합니다",
		})
		return 0
	}
	mcpURL := strings.TrimSpace(os.Getenv("ARCHIVE_MCP_URL"))
	tools, err := archivistmcpclient.New(mcpURL, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Archivist MCP client is unavailable")
		return 2
	}
	var log io.Writer
	if path := strings.TrimSpace(*logPath); path != "" {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			defer file.Close()
			fmt.Fprintf(file, "--- %s turn %s\n", time.Now().UTC().Format(time.RFC3339), runtimeContext.TurnID)
			log = file
		}
	}
	workingDirectory, _ := os.Getwd()
	err = codexcli.Run(ctx, codexcli.Options{
		CodexExecutable: strings.TrimSpace(*codex), MCPExecutable: self, WorkingDirectory: workingDirectory,
		StateDirectory: strings.TrimSpace(*stateDirectory), Model: *model, MCPURL: mcpURL,
		Runtime: runtimeContext, MaximumCalls: *maximumCalls, Log: log,
	}, tools, string(promptBytes), func(kind, summary string, resultIDs []string) error {
		record := map[string]any{"type": kind, "summary": summary}
		if kind == "final" {
			record["result_ids"] = resultIDs
		}
		return json.NewEncoder(os.Stdout).Encode(record)
	})
	if err != nil {
		if log != nil {
			fmt.Fprintf(log, "error: %v\n", err)
		}
		fmt.Fprintln(os.Stderr, "Codex turn failed")
		return 1
	}
	return 0
}

func codexLoginAvailable(ctx context.Context, executable string) bool {
	command := exec.CommandContext(ctx, executable, "login", "status")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run() == nil
}

// serveMCP is the stdio MCP server Codex launches inside a turn.
func serveMCP(ctx context.Context) int {
	runtimeContext, ok := runtimeFromEnvironment()
	if !ok {
		fmt.Fprintln(os.Stderr, "Archivist runtime context is incomplete")
		return 2
	}
	tools, err := archivistmcpclient.New(os.Getenv("ARCHIVE_MCP_URL"), &http.Client{Timeout: 120 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Archivist MCP client is unavailable")
		return 2
	}
	maximumCalls, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("ARCHIVE_CODEX_MAX_CALLS")))
	server := &codexcli.MCPServer{
		Runtime: runtimeContext, Tools: tools, MaximumCalls: maximumCalls,
		ResultFile: strings.TrimSpace(os.Getenv("ARCHIVE_CODEX_RESULT_FILE")),
	}
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Archive MCP server stopped")
		return 1
	}
	return 0
}

func runtimeFromEnvironment() (archivistloop.RuntimeContext, bool) {
	fencingToken, err := strconv.ParseUint(strings.TrimSpace(os.Getenv("ARCHIVE_FENCING_TOKEN")), 10, 64)
	runtimeContext := archivistloop.RuntimeContext{
		SessionID: strings.TrimSpace(os.Getenv("ARCHIVE_SESSION_ID")), TurnID: strings.TrimSpace(os.Getenv("ARCHIVE_TURN_ID")),
		FencingToken: fencingToken,
	}
	return runtimeContext, err == nil && runtimeContext.SessionID != "" && runtimeContext.TurnID != "" && fencingToken != 0
}
