package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"videoarchive/internal/adapter/archivistmcpclient"
	"videoarchive/internal/adapter/chatgptauth"
	"videoarchive/internal/adapter/chatgptresponses"
	"videoarchive/internal/application/archivistloop"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "usage: archive-archivist auth|models|run")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch arguments[0] {
	case "version", "--version", "-version":
		fmt.Println(version)
		return 0
	case "auth":
		return runAuth(ctx, arguments[1:])
	case "models":
		return runModels(ctx, arguments[1:])
	case "run":
		return runTurn(ctx, arguments[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown archive-archivist command")
		return 2
	}
}

type commonOptions struct {
	profileRef string
	issuer     string
	apiBase    string
}

func commonFlags(name string, arguments []string) (*flag.FlagSet, *commonOptions, error) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	options := &commonOptions{}
	set.StringVar(&options.profileRef, "profile-ref", strings.TrimSpace(os.Getenv("ARCHIVE_CHATGPT_PROFILE_REF")), "keychain:// ChatGPT profile reference")
	set.StringVar(&options.issuer, "issuer", environmentDefault("ARCHIVE_CHATGPT_ISSUER", "https://auth.openai.com"), "OpenID issuer")
	set.StringVar(&options.apiBase, "api-base", environmentDefault("ARCHIVE_CHATGPT_API_BASE", "https://api.openai.com/v1"), "Responses API base URL")
	if err := set.Parse(arguments); err != nil {
		return set, options, err
	}
	if strings.TrimSpace(options.profileRef) == "" || set.NArg() != 0 {
		return set, options, errors.New("profile reference is required")
	}
	return set, options, nil
}

func authManager(options *commonOptions) (*chatgptauth.Manager, error) {
	return chatgptauth.New(chatgptauth.Configuration{
		Store: chatgptauth.NewOSStore(), HTTPClient: &http.Client{Timeout: 30 * time.Second},
		IssuerURL: options.issuer, ResourceURL: options.apiBase, AgentName: "Archive Archivist",
		OpenBrowser: openBrowser,
	})
}

func runAuth(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "usage: archive-archivist auth login|status|logout")
		return 2
	}
	action := arguments[0]
	_, options, err := commonFlags("auth "+action, arguments[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid authentication options")
		return 2
	}
	manager, err := authManager(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "authentication configuration is invalid")
		return 2
	}
	switch action {
	case "login":
		loginContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		profile, err := manager.Login(loginContext, options.profileRef)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ChatGPT connection failed: %v\n", err)
			return 1
		}
		writeJSON(map[string]any{"status": "CONNECTED", "email": profile.Email, "profile_ref": options.profileRef})
		return 0
	case "status":
		profile, err := manager.Profile(options.profileRef)
		if err != nil {
			writeJSON(map[string]any{"status": "RECONNECT_REQUIRED", "profile_ref": options.profileRef})
			return 1
		}
		status := "CONNECTED"
		if time.Now().After(profile.SavedAt.Add(time.Duration(profile.ExpiresIn) * time.Second)) {
			status = "REFRESH_REQUIRED"
		}
		writeJSON(map[string]any{"status": status, "email": profile.Email, "profile_ref": options.profileRef, "saved_at": profile.SavedAt})
		return 0
	case "logout":
		if err := manager.Logout(ctx, options.profileRef); err != nil {
			fmt.Fprintln(os.Stderr, "ChatGPT disconnect failed")
			return 1
		}
		writeJSON(map[string]any{"status": "DISCONNECTED", "profile_ref": options.profileRef})
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown authentication command")
		return 2
	}
}

type managerTokenSource struct {
	manager    *chatgptauth.Manager
	profileRef string
}

func (source managerTokenSource) AccessToken(ctx context.Context) (string, error) {
	token, _, err := source.manager.AccessToken(ctx, source.profileRef)
	return token, err
}

func responsesClient(options *commonOptions) (*chatgptresponses.Client, error) {
	manager, err := authManager(options)
	if err != nil {
		return nil, err
	}
	return chatgptresponses.New(chatgptresponses.Configuration{
		BaseURL: options.apiBase, HTTPClient: &http.Client{Timeout: 5 * time.Minute},
		TokenSource: managerTokenSource{manager: manager, profileRef: options.profileRef},
	})
}

func runModels(ctx context.Context, arguments []string) int {
	_, options, err := commonFlags("models", arguments)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid model options")
		return 2
	}
	client, err := responsesClient(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "model client configuration is invalid")
		return 2
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "model list failed")
		return 1
	}
	writeJSON(map[string]any{"models": models})
	return 0
}

func runTurn(ctx context.Context, arguments []string) int {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	profileRef := set.String("profile-ref", strings.TrimSpace(os.Getenv("ARCHIVE_CHATGPT_PROFILE_REF")), "keychain:// ChatGPT profile reference")
	issuer := set.String("issuer", environmentDefault("ARCHIVE_CHATGPT_ISSUER", "https://auth.openai.com"), "OpenID issuer")
	apiBase := set.String("api-base", environmentDefault("ARCHIVE_CHATGPT_API_BASE", "https://api.openai.com/v1"), "Responses API base URL")
	model := set.String("model", strings.TrimSpace(os.Getenv("ARCHIVE_CHATGPT_MODEL")), "account-available model slug")
	maximumRounds := set.Int("max-rounds", 8, "maximum model rounds")
	maximumCalls := set.Int("max-tool-calls", 16, "maximum Archive tool calls")
	if set.Parse(arguments) != nil || set.NArg() != 0 || strings.TrimSpace(*profileRef) == "" || strings.TrimSpace(*model) == "" {
		fmt.Fprintln(os.Stderr, "invalid Archivist run options")
		return 2
	}
	promptBytes, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil || len(promptBytes) == 0 || len(promptBytes) > 64<<10 {
		fmt.Fprintln(os.Stderr, "Archivist prompt is invalid")
		return 2
	}
	defer clear(promptBytes)
	fencingToken, err := strconv.ParseUint(strings.TrimSpace(os.Getenv("ARCHIVE_FENCING_TOKEN")), 10, 64)
	runtimeContext := archivistloop.RuntimeContext{
		SessionID: strings.TrimSpace(os.Getenv("ARCHIVE_SESSION_ID")), TurnID: strings.TrimSpace(os.Getenv("ARCHIVE_TURN_ID")), FencingToken: fencingToken,
	}
	if err != nil || runtimeContext.SessionID == "" || runtimeContext.TurnID == "" || fencingToken == 0 {
		fmt.Fprintln(os.Stderr, "Archivist runtime context is incomplete")
		return 2
	}
	options := &commonOptions{profileRef: *profileRef, issuer: *issuer, apiBase: *apiBase}
	modelClient, err := responsesClient(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Archivist model client is unavailable")
		return 2
	}
	toolClient, err := archivistmcpclient.New(os.Getenv("ARCHIVE_MCP_URL"), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Archivist MCP client is unavailable")
		return 2
	}
	loop, err := archivistloop.New(archivistloop.Configuration{
		Model: *model, MaximumRounds: *maximumRounds, MaximumCalls: *maximumCalls,
		ModelClient: modelClient, ToolClient: toolClient,
		Progress: func(summary string) error {
			return writeRuntimeRecord(map[string]any{"type": "progress", "summary": summary})
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Archivist loop configuration is invalid")
		return 2
	}
	result, err := loop.Run(ctx, runtimeContext, string(promptBytes))
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitizedFailure(err))
		return 1
	}
	if err := writeRuntimeRecord(map[string]any{"type": "final", "summary": result.Summary, "result_ids": result.ResultIDs}); err != nil {
		fmt.Fprintln(os.Stderr, "Archivist result output failed")
		return 1
	}
	return 0
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("/usr/bin/open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Start()
}

func writeRuntimeRecord(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }

func writeJSON(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }

func environmentDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func sanitizedFailure(err error) string {
	switch {
	case errors.Is(err, chatgptauth.ErrReconnectRequired):
		return "ChatGPT 재연결이 필요합니다"
	case errors.Is(err, archivistloop.ErrExecutionLimit):
		return "Archivist 실행 한도에 도달했습니다"
	default:
		return "Archivist 실행을 완료하지 못했습니다"
	}
}
