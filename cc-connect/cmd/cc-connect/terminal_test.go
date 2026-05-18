package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestParseTerminalClaudeArgs_DefaultsToCwd(t *testing.T) {
	t.Setenv("CC_PROJECT", "EnvProject")
	opts, err := parseTerminalClaudeArgs(nil, "/tmp/repo")
	if err != nil {
		t.Fatalf("parseTerminalClaudeArgs returned error: %v", err)
	}
	if opts.project != "EnvProject" {
		t.Fatalf("project = %q, want EnvProject", opts.project)
	}
	if opts.workDir != "/tmp/repo" {
		t.Fatalf("workDir = %q, want /tmp/repo", opts.workDir)
	}
	if opts.resumeID != "" {
		t.Fatalf("resumeID = %q, want empty", opts.resumeID)
	}
	if opts.dataDir != "" {
		t.Fatalf("dataDir = %q, want empty", opts.dataDir)
	}
}

func TestParseTerminalClaudeArgs_WorkdirResumeDataDir(t *testing.T) {
	opts, err := parseTerminalClaudeArgs([]string{
		"--project", "CmdProject",
		"--workdir", "/tmp/working",
		"--resume", "sess-1",
		"--data-dir", "/tmp/data-dir",
	}, "/cwd")
	if err != nil {
		t.Fatalf("parseTerminalClaudeArgs returned error: %v", err)
	}
	if opts.project != "CmdProject" {
		t.Fatalf("project = %q, want CmdProject", opts.project)
	}
	if opts.workDir != "/tmp/working" {
		t.Fatalf("workDir = %q, want /tmp/working", opts.workDir)
	}
	if opts.resumeID != "sess-1" {
		t.Fatalf("resumeID = %q, want sess-1", opts.resumeID)
	}
	if opts.dataDir != "/tmp/data-dir" {
		t.Fatalf("dataDir = %q, want /tmp/data-dir", opts.dataDir)
	}
}

func TestRunTerminalRejectsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runTerminalWithIO(context.Background(), []string{"codex"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected unknown subcommand error")
	}
	if !strings.Contains(err.Error(), "unknown terminal subcommand") {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestParseTerminalClaudeArgs_UnknownOption(t *testing.T) {
	_, err := parseTerminalClaudeArgs([]string{"--invalid"}, "/tmp")
	if err == nil {
		t.Fatal("expected unknown option error")
	}
	if !strings.Contains(err.Error(), "unknown terminal claude option") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterTerminal(t *testing.T) {
	var req core.TerminalRegisterRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/register" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read register request body: %v", err)
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal register request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(core.TerminalSessionInfo{ID: "term-001"})
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() {
		terminalServiceBaseURL = oldBaseURL
	})

	id, err := registerTerminal(context.Background(), terminalHTTPClient("ignored"), terminalClaudeOptions{
		project:  "ClaudeCode",
		workDir:  "/tmp/repo",
		resumeID: "sess-1",
	})
	if err != nil {
		t.Fatalf("registerTerminal returned error: %v", err)
	}
	if id != "term-001" {
		t.Fatalf("terminal id = %q, want term-001", id)
	}
	if req.Project != "ClaudeCode" {
		t.Fatalf("request project = %q, want ClaudeCode", req.Project)
	}
	if req.WorkDir != "/tmp/repo" {
		t.Fatalf("request workDir = %q, want /tmp/repo", req.WorkDir)
	}
	if req.ClaudeSessionID != "sess-1" {
		t.Fatalf("request claudeSessionID = %q, want sess-1", req.ClaudeSessionID)
	}
}

func TestPostTerminalOutput(t *testing.T) {
	var got core.TerminalOutputRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/output" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read output request body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal output request: %v", err)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	if err := postTerminalOutput(context.Background(), terminalHTTPClient("ignored"), "term-001", "output", "hello"); err != nil {
		t.Fatalf("post terminal output error: %v", err)
	}
	if got.TerminalID != "term-001" {
		t.Fatalf("got.TerminalID = %q, want term-001", got.TerminalID)
	}
	if got.Type != "output" {
		t.Fatalf("got.Type = %q, want output", got.Type)
	}
	if got.Content != "hello" {
		t.Fatalf("got.Content = %q, want hello", got.Content)
	}

	if err := postTerminalOutput(context.Background(), terminalHTTPClient("ignored"), "term-001", "error", "bad"); err != nil {
		t.Fatalf("post terminal output error: %v", err)
	}
	if got.Type != "error" {
		t.Fatalf("got.Type = %q, want error", got.Type)
	}
	if got.Error != "bad" {
		t.Fatalf("got.Error = %q, want bad", got.Error)
	}
}

func TestParseTerminalClaudeArgs_ReportLocalInputDefaultAndDisableFlag(t *testing.T) {
	opts, err := parseTerminalClaudeArgs(nil, "/tmp/repo")
	if err != nil {
		t.Fatalf("parseTerminalClaudeArgs returned error: %v", err)
	}
	if !opts.reportLocalInput {
		t.Fatal("reportLocalInput = false, want default true")
	}

	opts, err = parseTerminalClaudeArgs([]string{"--no-report-local-input"}, "/tmp/repo")
	if err != nil {
		t.Fatalf("parseTerminalClaudeArgs returned error: %v", err)
	}
	if opts.reportLocalInput {
		t.Fatal("reportLocalInput = true, want false with --no-report-local-input")
	}
}

func TestLocalInputDefaultReportsSafeSignalAndWritesPTY(t *testing.T) {
	proc := newFakeTerminalProcess()
	posted := make(chan core.TerminalLocalInputRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalLocalInputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode local input request: %v", err)
		}
		posted <- req
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stderr bytes.Buffer
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("secret\r"), &stderr, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "secret\r" {
		t.Fatalf("process writes = %q, want secret\\r", got)
	}
	select {
	case req := <-posted:
		if req.TerminalID != "term-local" {
			t.Fatalf("posted terminal id = %q, want term-local", req.TerminalID)
		}
		if req.Content == "" || req.Content == "secret" || strings.Contains(req.Content, "secret") {
			t.Fatalf("posted content = %q, want safe non-raw signal", req.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input post")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLocalInputReportingDisabledDoesNotPostAndWritesPTY(t *testing.T) {
	proc := newFakeTerminalProcess()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		calls++
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stderr bytes.Buffer
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("secret\r"), &stderr, server.Client(), "term-local", false); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "secret\r" {
		t.Fatalf("process writes = %q, want secret\\r", got)
	}
	if calls != 0 {
		t.Fatalf("local input posts = %d, want 0", calls)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLocalInputWritesToPTYAndPostsSubmittedLineWhenEnabled(t *testing.T) {
	proc := newFakeTerminalProcess()
	posted := make(chan core.TerminalLocalInputRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalLocalInputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode local input request: %v", err)
		}
		posted <- req
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stderr bytes.Buffer
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("hello\r"), &stderr, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "hello\r" {
		t.Fatalf("process writes = %q, want hello\\r", got)
	}
	select {
	case req := <-posted:
		if req.TerminalID != "term-local" {
			t.Fatalf("posted terminal id = %q, want term-local", req.TerminalID)
		}
		if req.Content == "" || req.Content == "hello" || strings.Contains(req.Content, "hello") {
			t.Fatalf("posted content = %q, want safe non-raw signal", req.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input post")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSubmittedLineBackspaceEditsPostedLine(t *testing.T) {
	proc := newFakeTerminalProcess()
	posted := make(chan core.TerminalLocalInputRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalLocalInputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode local input request: %v", err)
		}
		posted <- req
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("hel\x7flo\n"), io.Discard, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "hel\x7flo\n" {
		t.Fatalf("process writes = %q, want exact stdin", got)
	}
	select {
	case req := <-posted:
		if req.Content == "" || req.Content == "helo" || strings.Contains(req.Content, "helo") {
			t.Fatalf("posted content = %q, want safe non-raw signal", req.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input post")
	}
}

func TestSubmittedLineEmptyLineIsNotPosted(t *testing.T) {
	proc := newFakeTerminalProcess()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		calls++
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("\r\n"), io.Discard, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "\r\n" {
		t.Fatalf("process writes = %q, want exact empty lines", got)
	}
	if calls != 0 {
		t.Fatalf("local input posts = %d, want 0", calls)
	}
}

func TestSubmittedLineControlSequenceStillPostsSafeSignal(t *testing.T) {
	proc := newFakeTerminalProcess()
	posted := make(chan core.TerminalLocalInputRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalLocalInputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode local input request: %v", err)
		}
		posted <- req
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	input := "he\x1b[Dllo\r"
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader(input), io.Discard, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != input {
		t.Fatalf("process writes = %q, want exact stdin", got)
	}
	select {
	case req := <-posted:
		if req.Content == "" || strings.Contains(req.Content, "hello") || strings.Contains(req.Content, "[D") {
			t.Fatalf("posted content = %q, want safe non-raw signal", req.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input post")
	}
}

func TestSubmittedLineBracketedPastePostsOneSafeSignal(t *testing.T) {
	proc := newFakeTerminalProcess()
	posted := make(chan core.TerminalLocalInputRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalLocalInputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode local input request: %v", err)
		}
		posted <- req
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	input := "\x1b[200~secret paste\n\x1b[201~\r"
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader(input), io.Discard, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != input {
		t.Fatalf("process writes = %q, want exact stdin", got)
	}
	select {
	case req := <-posted:
		if req.Content == "" || strings.Contains(req.Content, "secret") || strings.Contains(req.Content, "paste") {
			t.Fatalf("posted content = %q, want safe non-raw signal", req.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input post")
	}
	select {
	case req := <-posted:
		t.Fatalf("unexpected extra local input post: %#v", req)
	default:
	}
}

func TestSubmittedLinePreservesSplitUTF8(t *testing.T) {
	tracker := terminalLocalInputTracker{}
	data := []byte("北京天气")
	if submitted := tracker.feed(data[:2]); len(submitted) != 0 {
		t.Fatalf("first feed submitted %#v, want none", submitted)
	}
	submitted := tracker.feed(append(data[2:], '\n'))
	if len(submitted) != 1 || submitted[0] == "" || strings.Contains(submitted[0], "北京天气") {
		t.Fatalf("submitted = %#v, want safe non-raw signal", submitted)
	}
}

func TestLocalInputPostDoesNotBlockPTYWrites(t *testing.T) {
	proc := newFakeTerminalProcess()
	postStarted := make(chan struct{}, 1)
	releasePost := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePost) }) }
	defer release()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		select {
		case postStarted <- struct{}{}:
		default:
		}
		select {
		case <-releasePost:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	done := make(chan error, 1)
	reader := &chunkReader{chunks: [][]byte{[]byte("first\n"), []byte("second\n")}}
	go func() {
		done <- sendTerminalInputToProcess(context.Background(), proc, reader, io.Discard, server.Client(), "term-local", true)
	}()

	select {
	case <-postStarted:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first local input post")
	}
	deadline := time.After(500 * time.Millisecond)
	for foundSecond := false; !foundSecond; {
		select {
		case got := <-proc.writeNfy:
			foundSecond = got == "second\n"
		case <-deadline:
			t.Fatal("second PTY write was blocked by local input post")
		}
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local input copy to finish")
	}
	if got := strings.Join(proc.Writes(), ""); got != "first\nsecond\n" {
		t.Fatalf("process writes = %q, want both chunks", got)
	}
}

func TestLocalInputPostFailureWarnsAndStillWritesPTY(t *testing.T) {
	proc := newFakeTerminalProcess()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/local-input" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		http.Error(w, "local input unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stderr bytes.Buffer
	if err := sendTerminalInputToProcess(context.Background(), proc, strings.NewReader("keep\r"), &stderr, server.Client(), "term-local", true); err != nil {
		t.Fatalf("sendTerminalInputToProcess returned error: %v", err)
	}
	if got := strings.Join(proc.Writes(), ""); got != "keep\r" {
		t.Fatalf("process writes = %q, want keep\\r", got)
	}
	if !strings.Contains(stderr.String(), "warning: failed to post terminal local input") {
		t.Fatalf("stderr = %q, want local input warning", stderr.String())
	}
}

type chunkReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

type captureWriter struct {
	onWrite  func(string)
	writesMu sync.Mutex
	content  string
}

func (w *captureWriter) Write(p []byte) (int, error) {
	text := string(p)
	w.writesMu.Lock()
	w.content += text
	w.writesMu.Unlock()
	if w.onWrite != nil {
		w.onWrite(text)
	}
	return len(p), nil
}

func (w *captureWriter) String() string {
	w.writesMu.Lock()
	defer w.writesMu.Unlock()
	return w.content
}

func TestPollTerminalInputIgnoresEmptyContent(t *testing.T) {
	var calls int
	writer := &captureWriter{}
	firstWrite := make(chan struct{}, 1)
	writer.onWrite = func(text string) {
		if text == "" {
			return
		}
		select {
		case firstWrite <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/input/next" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		calls++
		content := ""
		if calls == 2 {
			content = "input-after-empty"
		}
		_ = json.NewEncoder(w).Encode(struct {
			Content string `json:"content"`
		}{Content: content})
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- pollTerminalInput(ctx, terminalHTTPClient("ignored"), "term-001", writer)
	}()

	select {
	case <-firstWrite:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeout waiting for poll input write")
	}

	if err := <-resultCh; err == nil {
		t.Fatal("expected pollTerminalInput to return after cancellation")
	}
	if got := writer.String(); got != "input-after-empty\r" {
		t.Fatalf("writer content = %q, want input-after-empty\\r", got)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 input/next calls, got %d", calls)
	}
}

func TestPollTerminalInputWritesCarriageReturnAndContent(t *testing.T) {
	var calls int
	writer := &captureWriter{}
	firstWrite := make(chan struct{}, 1)
	writer.onWrite = func(text string) {
		if text == "" {
			return
		}
		select {
		case firstWrite <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/input/next" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		calls++
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(struct {
				Content string `json:"content"`
			}{Content: "input-from-remote"})
			return
		}
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- pollTerminalInput(ctx, terminalHTTPClient("ignored"), "term-001", writer)
	}()

	select {
	case <-firstWrite:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeout waiting for poll input write")
	}

	if err := <-resultCh; err == nil {
		t.Fatal("expected pollTerminalInput to return after cancellation")
	}
	if got := writer.String(); got != "input-from-remote\r" {
		t.Fatalf("writer content = %q, want input-from-remote\\r", got)
	}
	if calls < 1 {
		t.Fatalf("expected input/next calls, got %d", calls)
	}
}

type fakeTerminalProcess struct {
	readCh    chan []byte
	waitCh    chan struct{}
	writes    []string
	writeNfy  chan string
	writesMu  sync.Mutex
	closeRead sync.Once
	closeWait sync.Once
	waitErr   error
}

func newFakeTerminalProcess() *fakeTerminalProcess {
	return &fakeTerminalProcess{
		readCh:   make(chan []byte, 8),
		writeNfy: make(chan string, 8),
		waitCh:   make(chan struct{}),
	}
}

func (p *fakeTerminalProcess) Read(b []byte) (int, error) {
	data, ok := <-p.readCh
	if !ok {
		return 0, io.EOF
	}
	return copy(b, data), nil
}

func (p *fakeTerminalProcess) Write(data []byte) (int, error) {
	text := string(data)
	p.writesMu.Lock()
	p.writes = append(p.writes, text)
	p.writesMu.Unlock()
	select {
	case p.writeNfy <- text:
	default:
	}
	return len(data), nil
}

func (p *fakeTerminalProcess) Close() error {
	p.closeRead.Do(func() {
		close(p.readCh)
	})
	p.closeWaitCh()
	return nil
}

func (p *fakeTerminalProcess) closeWaitCh() {
	p.closeWait.Do(func() {
		close(p.waitCh)
	})
}

func (p *fakeTerminalProcess) Wait() error {
	<-p.waitCh
	return p.waitErr
}

func (p *fakeTerminalProcess) Writes() []string {
	p.writesMu.Lock()
	defer p.writesMu.Unlock()
	out := make([]string, len(p.writes))
	copy(out, p.writes)
	return out
}

type fakeTerminalPTY struct {
	proc terminalProcess
	err  error
}

func (p *fakeTerminalPTY) Start(_ context.Context, _ terminalClaudeOptions) (terminalProcess, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.proc, nil
}

type fakeTerminalConsole struct {
	prepared int
	restored int
}

func (c *fakeTerminalConsole) Prepare(_ io.Reader, _ io.Writer, _ terminalProcess) (func(), error) {
	c.prepared++
	return func() { c.restored++ }, nil
}

func TestRunTerminalClaude_PreparesAndRestoresLocalTerminal(t *testing.T) {
	proc := newFakeTerminalProcess()
	proc.closeWaitCh()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/terminal/register":
			_ = json.NewEncoder(w).Encode(core.TerminalSessionInfo{ID: "term-raw"})
		case "/terminal/input/next":
			_ = json.NewEncoder(w).Encode(struct {
				Content string `json:"content"`
			}{})
		case "/terminal/unregister":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	oldPTY := defaultTerminalPTY
	defaultTerminalPTY = &fakeTerminalPTY{proc: proc}
	t.Cleanup(func() { defaultTerminalPTY = oldPTY })

	console := &fakeTerminalConsole{}
	oldConsole := defaultTerminalConsole
	defaultTerminalConsole = console
	t.Cleanup(func() { defaultTerminalConsole = oldConsole })

	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "run", "api.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte("socket"), 0o644); err != nil {
		t.Fatalf("write socket marker: %v", err)
	}

	if err := runTerminalClaude(context.Background(), terminalClaudeOptions{project: "ClaudeCode", workDir: "/tmp/repo", dataDir: tmp}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatalf("runTerminalClaude returned error: %v", err)
	}
	if console.prepared != 1 {
		t.Fatalf("console prepared = %d, want 1", console.prepared)
	}
	if console.restored != 1 {
		t.Fatalf("console restored = %d, want 1", console.restored)
	}
}

func TestRealTerminalPTY_WindowsUsesConPTY(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only ConPTY check")
	}

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "claude.sh")
	script := "#!/bin/sh\nprintf 'ready\\n'\nif IFS= read -r line; then printf 'got:%s\\n' \"$line\"; fi\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	wrapper := "@echo off\r\nbash \"%~dp0claude.sh\" %*\r\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude.cmd"), []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write fake claude wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := realTerminalPTY{}.Start(ctx, terminalClaudeOptions{workDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start() error = %v, want pipe fallback process", err)
	}
	defer proc.Close()

	if got := readTerminalProcessUntilContainsForTest(t, proc, "ready"); !strings.Contains(got, "ready") {
		t.Fatalf("first output = %q, want ready", got)
	}
	if _, err := proc.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := readTerminalProcessUntilContainsForTest(t, proc, "got:hello"); !strings.Contains(got, "got:hello") {
		t.Fatalf("second output = %q, want got:hello", got)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func readTerminalProcessUntilContainsForTest(t *testing.T, proc terminalProcess, want string) string {
	t.Helper()
	type readResult struct {
		n   int
		err error
		buf []byte
	}
	var out strings.Builder
	deadline := time.After(2 * time.Second)
	for {
		resultCh := make(chan readResult, 1)
		go func() {
			buf := make([]byte, 256)
			n, err := proc.Read(buf)
			resultCh <- readResult{n: n, err: err, buf: buf}
		}()
		select {
		case result := <-resultCh:
			if result.err != nil {
				t.Fatalf("Read() error = %v", result.err)
			}
			out.Write(result.buf[:result.n])
			if strings.Contains(out.String(), want) {
				return out.String()
			}
		case <-deadline:
			t.Fatalf("timeout waiting for process output %q; got %q", want, out.String())
			return out.String()
		}
	}
}

func TestReadTerminalOutputAndForwardPreservesSplitUTF8(t *testing.T) {
	proc := newFakeTerminalProcess()
	text := []byte("北京天气")
	proc.readCh <- text[:2]
	proc.readCh <- text[2:5]
	proc.readCh <- text[5:]
	if err := proc.Close(); err != nil {
		t.Fatalf("close fake process: %v", err)
	}

	var got strings.Builder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/output" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		var req core.TerminalOutputRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode output request: %v", err)
		}
		got.WriteString(req.Content)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stdout, stderr bytes.Buffer
	if err := readTerminalOutputAndForward(context.Background(), proc, &stdout, &stderr, server.Client(), "term-utf8"); err != nil {
		t.Fatalf("readTerminalOutputAndForward returned error: %v", err)
	}
	if stdout.String() != "北京天气" {
		t.Fatalf("stdout = %q, want 北京天气", stdout.String())
	}
	if got.String() != "北京天气" {
		t.Fatalf("posted output = %q, want 北京天气", got.String())
	}
}

func TestReadTerminalOutputAndForward_PostTimeoutDoesNotHang(t *testing.T) {
	proc := newFakeTerminalProcess()
	proc.readCh <- []byte("slow output")
	if err := proc.Close(); err != nil {
		t.Fatalf("close fake process: %v", err)
	}

	outputStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminal/output" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return
		}
		select {
		case outputStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(func() {
		close(releaseHandler)
		server.Close()
	})

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- readTerminalOutputAndForward(context.Background(), proc, &stdout, &stderr, server.Client(), "term-timeout")
	}()

	select {
	case <-outputStarted:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for output post")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readTerminalOutputAndForward returned error: %v", err)
		}
	case <-time.After(terminalPostTimeout + 2*time.Second):
		t.Fatal("terminal output forwarding hung after post timeout")
	}
	if elapsed := time.Since(start); elapsed > terminalPostTimeout+time.Second {
		t.Fatalf("output forwarding took %s, want within post timeout", elapsed)
	}
	if stdout.String() != "slow output" {
		t.Fatalf("stdout = %q, want slow output", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning: failed to post terminal output") {
		t.Fatalf("stderr = %q, want post warning", stderr.String())
	}
}

func TestRunTerminalClaude_UnregistersAfterPTYStartFailure(t *testing.T) {
	unregistered := make(chan core.TerminalUnregisterRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/terminal/register":
			_ = json.NewEncoder(w).Encode(core.TerminalSessionInfo{ID: "term-start-fail"})
		case "/terminal/unregister":
			var req core.TerminalUnregisterRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read unregister body: %v", err)
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal unregister body: %v", err)
			}
			unregistered <- req
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	oldPTY := defaultTerminalPTY
	defaultTerminalPTY = &fakeTerminalPTY{err: io.ErrClosedPipe}
	t.Cleanup(func() { defaultTerminalPTY = oldPTY })

	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "run", "api.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte("socket"), 0o644); err != nil {
		t.Fatalf("write socket marker: %v", err)
	}

	err := runTerminalClaude(context.Background(), terminalClaudeOptions{project: "ClaudeCode", workDir: "/tmp/repo", dataDir: tmp}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected PTY start error")
	}

	select {
	case req := <-unregistered:
		if req.TerminalID != "term-start-fail" {
			t.Fatalf("unregister terminal id = %q, want term-start-fail", req.TerminalID)
		}
		if req.ExitCode != 1 {
			t.Fatalf("unregister exit code = %d, want 1", req.ExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for unregister")
	}
}

func TestRunTerminalClaude_UnregistersAfterContextCancellation(t *testing.T) {
	proc := newFakeTerminalProcess()
	unregistered := make(chan core.TerminalUnregisterRequest, 1)
	terminalPrinted := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/terminal/register":
			_ = json.NewEncoder(w).Encode(core.TerminalSessionInfo{ID: "term-cancel"})
		case "/terminal/input/next":
			_ = json.NewEncoder(w).Encode(struct {
				Content string `json:"content"`
			}{})
		case "/terminal/unregister":
			var req core.TerminalUnregisterRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read unregister body: %v", err)
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal unregister body: %v", err)
			}
			unregistered <- req
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/terminal/output":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	oldPTY := defaultTerminalPTY
	defaultTerminalPTY = &fakeTerminalPTY{proc: proc}
	t.Cleanup(func() { defaultTerminalPTY = oldPTY })

	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "run", "api.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte("socket"), 0o644); err != nil {
		t.Fatalf("write socket marker: %v", err)
	}

	stdout := &captureWriter{}
	stdout.onWrite = func(text string) {
		if strings.Contains(text, "term-cancel") {
			select {
			case terminalPrinted <- struct{}{}:
			default:
			}
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runTerminalClaude(runCtx, terminalClaudeOptions{project: "ClaudeCode", workDir: "/tmp/repo", dataDir: tmp}, strings.NewReader(""), stdout, io.Discard)
	}()

	select {
	case <-terminalPrinted:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeout waiting for terminal id print")
	}

	select {
	case req := <-unregistered:
		if req.TerminalID != "term-cancel" {
			t.Fatalf("unregister terminal id = %q, want term-cancel", req.TerminalID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for unregister")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for runTerminalClaude exit")
	}
}

func TestRunTerminalClaude_StreamsOutputAndInput(t *testing.T) {
	proc := newFakeTerminalProcess()
	proc.readCh <- []byte("session output")

	var mu sync.Mutex
	var outputRequests []core.TerminalOutputRequest
	var unregister core.TerminalUnregisterRequest
	var terminalID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/terminal/register":
			var req core.TerminalRegisterRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read register body: %v", err)
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal register body: %v", err)
			}
			terminalID = "term-007"
			_ = json.NewEncoder(w).Encode(core.TerminalSessionInfo{ID: terminalID, Project: req.Project})
		case "/terminal/output":
			var req core.TerminalOutputRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read output body: %v", err)
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal output body: %v", err)
			}
			mu.Lock()
			outputRequests = append(outputRequests, req)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/terminal/unregister":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read unregister body: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if err := json.Unmarshal(body, &unregister); err != nil {
				t.Fatalf("unmarshal unregister body: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/terminal/input/next":
			_ = json.NewEncoder(w).Encode(struct {
				Content string `json:"content"`
			}{Content: "remote-input"})
		}
	}))
	defer server.Close()

	oldBaseURL := terminalServiceBaseURL
	terminalServiceBaseURL = server.URL
	t.Cleanup(func() { terminalServiceBaseURL = oldBaseURL })

	oldPTY := defaultTerminalPTY
	defaultTerminalPTY = &fakeTerminalPTY{proc: proc}
	t.Cleanup(func() { defaultTerminalPTY = oldPTY })

	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "run", "api.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte("socket"), 0o644); err != nil {
		t.Fatalf("write socket marker: %v", err)
	}

	var stdout, stderr bytes.Buffer
	go func() {
		for {
			select {
			case line := <-proc.writeNfy:
				if strings.TrimSpace(line) == "remote-input" {
					proc.closeWaitCh()
					return
				}
			case <-runCtx.Done():
				return
			}
		}
	}()

	err := runTerminalClaude(runCtx, terminalClaudeOptions{project: "ClaudeCode", workDir: "/tmp/repo", dataDir: tmp}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("runTerminalClaude returned error: %v", err)
	}

	target := stdout.String()
	if !strings.Contains(target, "term-007") {
		t.Fatalf("stdout = %q, want include terminal id", target)
	}
	if !strings.Contains(target, "session output") {
		t.Fatalf("stdout = %q, want include session output", target)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	mu.Lock()
	if len(outputRequests) == 0 {
		mu.Unlock()
		t.Fatal("expected terminal output posts")
	}
	first := outputRequests[0]
	mu.Unlock()
	if first.TerminalID != "term-007" {
		t.Fatalf("first output terminal id = %q, want term-007", first.TerminalID)
	}
	if first.Content != "session output" {
		t.Fatalf("first output content = %q, want session output", first.Content)
	}
	if unregister.TerminalID != "term-007" {
		t.Fatalf("unregister terminal id = %q, want term-007", unregister.TerminalID)
	}
	if unregister.ExitCode != 0 {
		t.Fatalf("unregister exit code = %d, want 0", unregister.ExitCode)
	}

	writes := proc.Writes()
	found := false
	for _, w := range writes {
		if w == "remote-input\r" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("process writes = %#v, want remote-input\\r", writes)
	}
}
