package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/chenhg5/cc-connect/core"
)

type cliBridgeClaudeOptions struct {
	project    string
	sessionKey string
	dataDir    string
	readOnly   bool
}

var errCLIBridgeUsage = errors.New("show cli-bridge usage")

func runCLIBridge(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCLIBridgeWithIO(ctx, args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errCLIBridgeUsage) {
			printCLIBridgeUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printCLIBridgeUsage(os.Stderr)
		os.Exit(1)
	}
}

func runCLIBridgeWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errCLIBridgeUsage
	}
	subcommand := args[0]
	if subcommand == "--help" || subcommand == "-h" || subcommand == "help" {
		return errCLIBridgeUsage
	}
	if subcommand != "claude" {
		return fmt.Errorf("unknown cli-bridge subcommand: %s", subcommand)
	}
	opts, err := parseCLIBridgeClaudeArgs(args[1:])
	if err != nil {
		return err
	}
	return runCLIBridgeClaude(ctx, opts, stdin, stdout, stderr)
}

func parseCLIBridgeClaudeArgs(args []string) (cliBridgeClaudeOptions, error) {
	opts := cliBridgeClaudeOptions{
		project:    strings.TrimSpace(os.Getenv("CC_PROJECT")),
		sessionKey: strings.TrimSpace(os.Getenv("CC_SESSION_KEY")),
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", args[i])
			}
			i++
			opts.project = args[i]
		case "--session", "-s":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", args[i])
			}
			i++
			opts.sessionKey = args[i]
		case "--data-dir":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--data-dir requires a value")
			}
			i++
			opts.dataDir = args[i]
		case "--read-only":
			opts.readOnly = true
		case "--help", "-h":
			return opts, errCLIBridgeUsage
		default:
			return opts, fmt.Errorf("unknown cli-bridge claude option: %s", args[i])
		}
	}
	return opts, nil
}

func runCLIBridgeClaude(ctx context.Context, opts cliBridgeClaudeOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	sockPath := resolveSocketPath(opts.dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		return fmt.Errorf("cc-connect is not running (socket not found: %s)", sockPath)
	}

	client := cliBridgeHTTPClient(sockPath)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	attachBody, err := json.Marshal(core.CLIBridgeAttachRequest{Project: opts.project, SessionKey: opts.sessionKey})
	if err != nil {
		return fmt.Errorf("encode attach request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/cli-bridge/attach", bytes.NewReader(attachBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect attach stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return errors.New(strings.TrimSpace(string(body)))
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- readCLIBridgeFrames(resp.Body, stdout, stderr)
		cancel()
	}()

	if !opts.readOnly {
		go func() {
			errCh <- sendCLIBridgeInputLines(ctx, client, opts, stdin)
			cancel()
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

func cliBridgeHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

func readCLIBridgeFrames(r io.Reader, stdout, stderr io.Writer) error {
	decoder := json.NewDecoder(r)
	for {
		var frame core.CLIBridgeFrame
		if err := decoder.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read attach stream: %w", err)
		}
		printCLIBridgeFrame(frame, stdout, stderr)
	}
}

func printCLIBridgeFrame(frame core.CLIBridgeFrame, stdout, stderr io.Writer) {
	switch frame.Type {
	case "ready":
		fmt.Fprintln(stdout, "Attached to cc-connect")
		fmt.Fprintf(stdout, "Project: %s\n", frame.Project)
		fmt.Fprintf(stdout, "Session: %s\n", frame.SessionKey)
		fmt.Fprintf(stdout, "Claude session: %s\n", frame.AgentSessionID)
	case "assistant":
		fmt.Fprintf(stdout, "[assistant] %s\n", frame.Content)
	case "assistant_delta":
		fmt.Fprint(stdout, frame.Content)
	case "status":
		fmt.Fprintf(stdout, "[status] %s\n", frame.Content)
	case "permission":
		fmt.Fprintf(stdout, "[permission] %s\n", frame.Content)
	case "input":
		fmt.Fprintf(stdout, "[input] %s\n", frame.Content)
	case "error":
		fmt.Fprintf(stderr, "[error] %s\n", frame.Error)
	default:
		fmt.Fprintf(stdout, "[%s] %s\n", frame.Type, frame.Content)
	}
}

func sendCLIBridgeInputLines(ctx context.Context, client *http.Client, opts cliBridgeClaudeOptions, stdin io.Reader) error {
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		if err := postCLIBridgeInput(ctx, client, opts, scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

func postCLIBridgeInput(ctx context.Context, client *http.Client, opts cliBridgeClaudeOptions, line string) error {
	body, err := json.Marshal(core.CLIBridgeInputRequest{
		Project:    opts.project,
		SessionKey: opts.sessionKey,
		Message:    line,
	})
	if err != nil {
		return fmt.Errorf("encode input request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/cli-bridge/input", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send input: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return errors.New(strings.TrimSpace(string(respBody)))
	}
	return nil
}

func printCLIBridgeUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: cc-connect cli-bridge claude [options]

Attach a local terminal to an existing cc-connect-owned Claude session.

Options:
  -p, --project <name>   Target project (optional if only one project)
  -s, --session <key>    Target session key (optional if only one live session)
      --data-dir <path>  Data directory (default: ~/.cc-connect)
      --read-only        Mirror output only; do not read local stdin
  -h, --help             Show this help`)
}
