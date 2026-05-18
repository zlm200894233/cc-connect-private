package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"golang.org/x/term"
)

type terminalClaudeOptions struct {
	project          string
	workDir          string
	resumeID         string
	dataDir          string
	reportLocalInput bool
}

type terminalPTY interface {
	Start(ctx context.Context, opts terminalClaudeOptions) (terminalProcess, error)
}

type terminalConsole interface {
	Prepare(stdin io.Reader, stdout io.Writer, proc terminalProcess) (func(), error)
}

type terminalProcess interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	Wait() error
}

var errTerminalUsage = errors.New("show terminal usage")

var defaultTerminalPTY terminalPTY = realTerminalPTY{}
var defaultTerminalConsole terminalConsole = realTerminalConsole{}
var terminalServiceBaseURL = "http://unix"

const terminalCleanupTimeout = 5 * time.Second
const terminalPostTimeout = 3 * time.Second
const terminalEmptyInputPollDelay = 200 * time.Millisecond
const terminalLocalInputSignalContent = "<local-input>"

func runTerminal(args []string) {
	if err := runTerminalWithIO(context.Background(), args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errTerminalUsage) {
			printTerminalUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printTerminalUsage(os.Stderr)
		os.Exit(1)
	}
}

func runTerminalWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		return errTerminalUsage
	}

	subcommand := args[0]
	if subcommand != "claude" {
		return fmt.Errorf("unknown terminal subcommand: %s", subcommand)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	opts, err := parseTerminalClaudeArgs(args[1:], cwd)
	if err != nil {
		return err
	}
	return runTerminalClaude(ctx, opts, stdin, stdout, stderr)
}

func parseTerminalClaudeArgs(args []string, cwd string) (terminalClaudeOptions, error) {
	opts := terminalClaudeOptions{
		workDir:          cwd,
		project:          strings.TrimSpace(os.Getenv("CC_PROJECT")),
		reportLocalInput: true,
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", args[i])
			}
			i++
			opts.project = args[i]
		case "--workdir", "--work-dir", "-C":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", args[i])
			}
			i++
			opts.workDir = args[i]
		case "--resume":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--resume requires a value")
			}
			i++
			opts.resumeID = args[i]
		case "--data-dir":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--data-dir requires a value")
			}
			i++
			opts.dataDir = args[i]
		case "--report-local-input":
			opts.reportLocalInput = true
		case "--no-report-local-input":
			opts.reportLocalInput = false
		case "--help", "-h":
			return opts, errTerminalUsage
		default:
			return opts, fmt.Errorf("unknown terminal claude option: %s", args[i])
		}
	}

	return opts, nil
}

func runTerminalClaude(ctx context.Context, opts terminalClaudeOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	sockPath := resolveSocketPath(opts.dataDir)
	if _, err := os.Stat(sockPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cc-connect service is not running (socket not found: %s). Start start-feishu.cmd to launch it", sockPath)
		}
		return fmt.Errorf("failed to check cc-connect socket: %w", err)
	}

	client := terminalHTTPClient(sockPath)
	terminalID, err := registerTerminal(ctx, client, opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, terminalID)

	process, err := defaultTerminalPTY.Start(ctx, opts)
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), terminalCleanupTimeout)
		unregisterTerminal(cleanupCtx, client, terminalID, 1)
		cleanupCancel()
		return err
	}
	defer process.Close()

	restoreTerminal, err := defaultTerminalConsole.Prepare(stdin, stdout, process)
	if err != nil {
		return err
	}
	defer restoreTerminal()

	runtimeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	exitCode := 1
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), terminalCleanupTimeout)
		defer cleanupCancel()
		unregisterTerminal(cleanupCtx, client, terminalID, exitCode)
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- process.Wait()
	}()

	errCh := make(chan error, 3)
	go func() {
		errCh <- sendTerminalInputToProcess(runtimeCtx, process, stdin, stderr, client, terminalID, opts.reportLocalInput)
	}()
	go func() {
		errCh <- readTerminalOutputAndForward(runtimeCtx, process, stdout, stderr, client, terminalID)
	}()
	go func() {
		errCh <- pollTerminalInput(runtimeCtx, client, terminalID, process)
	}()

	for {
		select {
		case err := <-waitCh:
			if err == nil {
				exitCode = 0
			}
			return err
		case err := <-errCh:
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				continue
			}
			if err != nil {
				return err
			}
		case <-runtimeCtx.Done():
			if err := process.Close(); err != nil {
				fmt.Fprintf(stderr, "warning: failed to close terminal process: %v\n", err)
			}
			if runtimeCtx.Err() != nil {
				return runtimeCtx.Err()
			}
			return nil
		}
	}
}

func terminalHTTPClient(sockPath string) *http.Client {
	if terminalServiceBaseURL == "http://unix" {
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		}
	}
	return &http.Client{}
}

func registerTerminal(ctx context.Context, client *http.Client, opts terminalClaudeOptions) (string, error) {
	reqBody, err := json.Marshal(core.TerminalRegisterRequest{
		Project:         opts.project,
		WorkDir:         opts.workDir,
		ClaudeSessionID: opts.resumeID,
	})
	if err != nil {
		return "", fmt.Errorf("encode terminal register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalServiceBaseURL+"/terminal/register", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build terminal register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("register terminal: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if len(respBody) == 0 {
			return "", fmt.Errorf("register terminal failed with status %d", resp.StatusCode)
		}
		return "", errors.New(strings.TrimSpace(string(respBody)))
	}

	var info core.TerminalSessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode terminal register response: %w", err)
	}
	if info.ID == "" {
		return "", errors.New("terminal register response missing id")
	}
	return info.ID, nil
}

func unregisterTerminal(ctx context.Context, client *http.Client, terminalID string, exitCode int) {
	if terminalID == "" {
		return
	}
	reqBody, err := json.Marshal(core.TerminalUnregisterRequest{
		TerminalID: terminalID,
		ExitCode:   exitCode,
	})
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalServiceBaseURL+"/terminal/unregister", bytes.NewReader(reqBody))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func postTerminalOutput(ctx context.Context, client *http.Client, terminalID, typ, content string) error {
	payload := core.TerminalOutputRequest{
		TerminalID: terminalID,
		Type:       typ,
	}
	if typ == "error" {
		payload.Error = content
	} else {
		payload.Content = content
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode terminal output: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalServiceBaseURL+"/terminal/output", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build terminal output request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post terminal output: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if len(respBody) == 0 {
			return fmt.Errorf("post terminal output failed with status %d", resp.StatusCode)
		}
		return errors.New(strings.TrimSpace(string(respBody)))
	}
	return nil
}

func postTerminalLocalInput(ctx context.Context, client *http.Client, terminalID, content string) error {
	payload := core.TerminalLocalInputRequest{
		TerminalID: terminalID,
		Content:    content,
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode terminal local input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalServiceBaseURL+"/terminal/local-input", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build terminal local input request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post terminal local input: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if len(respBody) == 0 {
			return fmt.Errorf("post terminal local input failed with status %d", resp.StatusCode)
		}
		return errors.New(strings.TrimSpace(string(respBody)))
	}
	return nil
}

func pollTerminalInput(ctx context.Context, client *http.Client, terminalID string, out io.Writer) error {
	reqBody, err := json.Marshal(struct {
		TerminalID string `json:"terminal_id"`
	}{
		TerminalID: terminalID,
	})
	if err != nil {
		return fmt.Errorf("encode terminal input poll request: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalServiceBaseURL+"/terminal/input/next", bytes.NewReader(reqBody))
		if err != nil {
			return fmt.Errorf("build terminal input poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("poll terminal input: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if len(body) == 0 {
				return fmt.Errorf("poll terminal input failed with status %d", resp.StatusCode)
			}
			return errors.New(strings.TrimSpace(string(body)))
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("decode terminal input response: %w", err)
		}
		_ = resp.Body.Close()

		if payload.Content == "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(terminalEmptyInputPollDelay):
			}
			continue
		}
		if _, err := out.Write([]byte(payload.Content + "\r")); err != nil {
			return fmt.Errorf("write terminal input to process: %w", err)
		}
	}
}

type realTerminalConsole struct{}

type terminalResizer interface {
	Resize(width, height int) error
}

func (realTerminalConsole) Prepare(stdin io.Reader, stdout io.Writer, proc terminalProcess) (func(), error) {
	noop := func() {}
	in, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return noop, nil
	}
	out, ok := stdout.(*os.File)
	if !ok || !term.IsTerminal(int(out.Fd())) {
		return noop, nil
	}
	oldState, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("set local terminal raw mode: %w", err)
	}
	resizeTerminalProcess(out, proc)
	return func() { _ = term.Restore(int(in.Fd()), oldState) }, nil
}

func resizeTerminalProcess(stdout *os.File, proc terminalProcess) {
	resizer, ok := proc.(terminalResizer)
	if !ok {
		return
	}
	width, height, err := term.GetSize(int(stdout.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return
	}
	_ = resizer.Resize(width, height)
}

func sendTerminalInputToProcess(ctx context.Context, proc terminalProcess, stdin io.Reader, stderr io.Writer, client *http.Client, terminalID string, reportLocalInput bool) error {
	if stdin == nil {
		return nil
	}

	buf := make([]byte, 8*1024)
	tracker := terminalLocalInputTracker{}
	reporter := newTerminalLocalInputReporter(ctx, client, terminalID, stderr, reportLocalInput)
	defer reporter.close()
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if werr := writeAllTerminalInput(proc, chunk); werr != nil {
				return werr
			}
			if reportLocalInput {
				for _, line := range tracker.feed(chunk) {
					reporter.submit(line)
				}
			}
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("copy terminal input: %w", err)
		}
	}
}

func writeAllTerminalInput(proc terminalProcess, data []byte) error {
	for len(data) > 0 {
		n, err := proc.Write(data)
		if err != nil {
			return fmt.Errorf("write terminal input: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

const terminalLocalInputQueueSize = 16

type terminalLocalInputReporter struct {
	ctx        context.Context
	client     *http.Client
	terminalID string
	stderr     io.Writer
	lines      chan string
	done       chan struct{}
}

func newTerminalLocalInputReporter(ctx context.Context, client *http.Client, terminalID string, stderr io.Writer, enabled bool) *terminalLocalInputReporter {
	if !enabled || client == nil {
		return nil
	}
	reporter := &terminalLocalInputReporter{
		ctx:        ctx,
		client:     client,
		terminalID: terminalID,
		stderr:     stderr,
		lines:      make(chan string, terminalLocalInputQueueSize),
		done:       make(chan struct{}),
	}
	go reporter.run()
	return reporter
}

func (r *terminalLocalInputReporter) run() {
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			return
		case line, ok := <-r.lines:
			if !ok {
				return
			}
			r.post(line)
		}
	}
}

func (r *terminalLocalInputReporter) submit(line string) {
	if r == nil || line == "" {
		return
	}
	select {
	case r.lines <- terminalLocalInputSignalContent:
	default:
		fmt.Fprintln(r.stderr, "warning: dropping terminal local input: reporter queue is full")
	}
}

func (r *terminalLocalInputReporter) post(line string) {
	postCtx, cancel := context.WithTimeout(r.ctx, terminalPostTimeout)
	defer cancel()
	if err := postTerminalLocalInput(postCtx, r.client, r.terminalID, line); err != nil && r.ctx.Err() == nil {
		fmt.Fprintf(r.stderr, "warning: failed to post terminal local input: %v\n", err)
	}
}

func (r *terminalLocalInputReporter) close() {
	if r == nil {
		return
	}
	close(r.lines)
	<-r.done
}

type terminalInputTrackerState int

const (
	terminalInputTrackerGround terminalInputTrackerState = iota
	terminalInputTrackerEscape
	terminalInputTrackerCSI
	terminalInputTrackerSS3
	terminalInputTrackerOSC
	terminalInputTrackerOSCEscape
)

type terminalLocalInputTracker struct {
	pending          []byte
	state            terminalInputTrackerState
	csi              []rune
	inBracketedPaste bool
	inputRunes       int
}

func (t *terminalLocalInputTracker) feed(chunk []byte) []string {
	data := make([]byte, 0, len(t.pending)+len(chunk))
	data = append(data, t.pending...)
	data = append(data, chunk...)
	t.pending = t.pending[:0]

	var submitted []string
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 && !utf8.FullRune(data) {
			t.pending = append(t.pending, data...)
			break
		}
		data = data[size:]

		if r == utf8.RuneError && size == 1 {
			continue
		}
		if t.state != terminalInputTrackerGround {
			t.consumeControlRune(r)
			continue
		}
		if t.inBracketedPaste {
			if r == '\x1b' {
				t.state = terminalInputTrackerEscape
				continue
			}
			if r == '\r' || r == '\n' || r == '\t' || unicode.IsPrint(r) {
				t.inputRunes++
			}
			continue
		}
		if r == '\x1b' {
			t.state = terminalInputTrackerEscape
			continue
		}

		switch r {
		case '\r', '\n':
			if t.inputRunes > 0 {
				submitted = append(submitted, terminalLocalInputSignalContent)
			}
			t.inputRunes = 0
		case '\b', '\x7f':
			if t.inputRunes > 0 {
				t.inputRunes--
			}
		case '\t':
			t.inputRunes++
		default:
			if unicode.IsPrint(r) {
				t.inputRunes++
			}
		}
	}
	return submitted
}

func (t *terminalLocalInputTracker) consumeControlRune(r rune) {
	switch t.state {
	case terminalInputTrackerGround:
		if r == '\x1b' {
			t.state = terminalInputTrackerEscape
		}
	case terminalInputTrackerEscape:
		switch r {
		case '[':
			t.state = terminalInputTrackerCSI
			t.csi = t.csi[:0]
		case 'O':
			t.state = terminalInputTrackerSS3
		case ']':
			t.state = terminalInputTrackerOSC
		case '\x1b':
			t.state = terminalInputTrackerEscape
		default:
			t.state = terminalInputTrackerGround
		}
	case terminalInputTrackerCSI:
		t.csi = append(t.csi, r)
		if isTerminalControlFinal(r) {
			seq := string(t.csi)
			switch seq {
			case "200~":
				t.inBracketedPaste = true
			case "201~":
				t.inBracketedPaste = false
			}
			t.csi = t.csi[:0]
			t.state = terminalInputTrackerGround
		}
	case terminalInputTrackerSS3:
		t.state = terminalInputTrackerGround
	case terminalInputTrackerOSC:
		if r == '\a' {
			t.state = terminalInputTrackerGround
			return
		}
		if r == '\x1b' {
			t.state = terminalInputTrackerOSCEscape
		}
	case terminalInputTrackerOSCEscape:
		if r == '\\' {
			t.state = terminalInputTrackerGround
			return
		}
		t.state = terminalInputTrackerOSC
	}
}

func isTerminalControlFinal(r rune) bool {
	return r >= 0x40 && r <= 0x7e
}

func readTerminalOutputAndForward(ctx context.Context, proc terminalProcess, stdout, stderr io.Writer, client *http.Client, terminalID string) error {
	buf := make([]byte, 8*1024)
	var pendingUTF8 []byte
	postOutput := func(content string) {
		if content == "" {
			return
		}
		postCtx, cancel := context.WithTimeout(ctx, terminalPostTimeout)
		if err := postTerminalOutput(postCtx, client, terminalID, "output", content); err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "warning: failed to post terminal output: %v\n", err)
		}
		cancel()
	}
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := stdout.Write(chunk); werr != nil {
				return fmt.Errorf("write terminal output: %w", werr)
			}
			var content string
			content, pendingUTF8 = terminalCompleteUTF8Chunk(pendingUTF8, chunk)
			postOutput(content)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(pendingUTF8) > 0 {
					postOutput(string(pendingUTF8))
				}
				return nil
			}
			return fmt.Errorf("read terminal output: %w", err)
		}
	}
}

func terminalCompleteUTF8Chunk(pending, chunk []byte) (string, []byte) {
	data := make([]byte, 0, len(pending)+len(chunk))
	data = append(data, pending...)
	data = append(data, chunk...)

	pos := 0
	for pos < len(data) {
		if data[pos] < utf8.RuneSelf {
			pos++
			continue
		}
		r, size := utf8.DecodeRune(data[pos:])
		if r == utf8.RuneError && size == 1 && !utf8.FullRune(data[pos:]) {
			break
		}
		pos += size
	}

	nextPending := append([]byte(nil), data[pos:]...)
	return string(data[:pos]), nextPending
}

func printTerminalUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: cc-connect terminal claude [options]

Attach a local terminal as a Claude broker runtime.

Options:
  -p, --project <name>     Target project (optional if only one project)
  -C, --workdir <path>     Working directory for terminal session (default: current)
      --work-dir <path>    Working directory for terminal session (default: current)
      --resume <id>        Resume existing terminal broker session by ID
      --data-dir <path>    Data directory (default: ~/.cc-connect)
      --report-local-input Report local submitted-line signals to attached chat (default)
      --no-report-local-input
                           Disable local submitted-line reporting
  -h, --help               Show this help`)
}
