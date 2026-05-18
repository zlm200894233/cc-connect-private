package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestParseCLIBridgeClaudeArgs_Defaults(t *testing.T) {
	t.Setenv("CC_PROJECT", "")
	t.Setenv("CC_SESSION_KEY", "")

	opts, err := parseCLIBridgeClaudeArgs(nil)
	if err != nil {
		t.Fatalf("parseCLIBridgeClaudeArgs returned error: %v", err)
	}
	if opts.project != "" {
		t.Fatalf("project = %q, want empty", opts.project)
	}
	if opts.sessionKey != "" {
		t.Fatalf("sessionKey = %q, want empty", opts.sessionKey)
	}
	if opts.dataDir != "" {
		t.Fatalf("dataDir = %q, want empty", opts.dataDir)
	}
	if opts.readOnly {
		t.Fatal("readOnly = true, want false")
	}
}

func TestParseCLIBridgeClaudeArgs_ProjectSessionDataDir(t *testing.T) {
	opts, err := parseCLIBridgeClaudeArgs([]string{
		"--project", "ClaudeCode",
		"--session", "feishu:chat:user",
		"--data-dir", "C:/tmp/cc-data",
		"--read-only",
	})
	if err != nil {
		t.Fatalf("parseCLIBridgeClaudeArgs returned error: %v", err)
	}
	if opts.project != "ClaudeCode" {
		t.Fatalf("project = %q, want ClaudeCode", opts.project)
	}
	if opts.sessionKey != "feishu:chat:user" {
		t.Fatalf("sessionKey = %q, want feishu:chat:user", opts.sessionKey)
	}
	if opts.dataDir != "C:/tmp/cc-data" {
		t.Fatalf("dataDir = %q, want C:/tmp/cc-data", opts.dataDir)
	}
	if !opts.readOnly {
		t.Fatal("readOnly = false, want true")
	}
}

func TestParseCLIBridgeClaudeArgs_ShortProjectSessionFlags(t *testing.T) {
	opts, err := parseCLIBridgeClaudeArgs([]string{
		"-p", "ClaudeCode",
		"-s", "feishu:chat:user",
		"--data-dir", "C:/tmp/cc-data",
	})
	if err != nil {
		t.Fatalf("parseCLIBridgeClaudeArgs returned error: %v", err)
	}
	if opts.project != "ClaudeCode" {
		t.Fatalf("project = %q, want ClaudeCode", opts.project)
	}
	if opts.sessionKey != "feishu:chat:user" {
		t.Fatalf("sessionKey = %q, want feishu:chat:user", opts.sessionKey)
	}
	if opts.dataDir != "C:/tmp/cc-data" {
		t.Fatalf("dataDir = %q, want C:/tmp/cc-data", opts.dataDir)
	}
}

func TestRunCLIBridge_RejectsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runCLIBridgeWithIO(context.Background(), []string{"codex"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected unknown subcommand error")
	}
	if !strings.Contains(err.Error(), "unknown cli-bridge subcommand") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintCLIBridgeFrame_Ready(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printCLIBridgeFrame(core.CLIBridgeFrame{
		Type:           "ready",
		Project:        "ClaudeCode",
		SessionKey:     "feishu:chat:user",
		AgentSessionID: "agent-session-123",
	}, &stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{
		"Attached to cc-connect",
		"Project: ClaudeCode",
		"Session: feishu:chat:user",
		"Claude session: agent-session-123",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ready output %q missing %q", out, want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
