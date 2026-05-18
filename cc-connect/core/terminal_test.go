package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTerminalRegistry_UnregisterUnblocksNextInput(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})

	started := make(chan struct{})
	done := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		close(started)
		_, ok := registry.NextInputContext(ctx, info.ID)
		done <- ok
	}()

	<-started
	if !registry.Unregister(info.ID, 0) {
		t.Fatal("expected unregister to succeed")
	}

	select {
	case ok := <-done:
		if ok {
			t.Fatal("NextInputContext returned input after unregister")
		}
	case <-time.After(time.Second):
		t.Fatal("NextInputContext did not unblock after unregister")
	}
}

func TestTerminalRegistry_SendInputAfterUnregisterReturnsError(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	if !registry.Unregister(info.ID, 0) {
		t.Fatal("expected unregister to succeed")
	}
	if err := registry.SendInput(info.ID, "late input"); err == nil {
		t.Fatal("expected SendInput to fail after unregister")
	}
}

func TestTerminalRegistry_DetachActiveTurnReturnsErrorAndKeepsAttachment(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	if err := registry.Attach(info.ID, "feishu:user1", "reply-ctx"); err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if _, _, _, err := registry.StartLocalInputForTurnTarget(info.ID, "local prompt"); err != nil {
		t.Fatalf("StartLocalInputForTurnTarget returned error: %v", err)
	}

	if err := registry.Detach("feishu:user1"); err == nil {
		t.Fatal("expected Detach to fail while turn is active")
	}
	got, replyCtx, ok := registry.AttachedTarget(info.ID)
	if !ok || got.AttachedKey != "feishu:user1" || replyCtx != "reply-ctx" {
		t.Fatalf("AttachedTarget = (%#v, %#v, %v), want original attachment", got, replyCtx, ok)
	}
}

func TestTerminalRegistry_AttachActiveTurnReturnsErrorAndKeepsAttachment(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	if err := registry.Attach(info.ID, "feishu:user1", "reply-ctx"); err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if _, _, _, err := registry.StartLocalInputForTurnTarget(info.ID, "local prompt"); err != nil {
		t.Fatalf("StartLocalInputForTurnTarget returned error: %v", err)
	}

	if err := registry.Attach(info.ID, "feishu:user1", "new-reply-ctx"); err == nil {
		t.Fatal("expected same-session reattach to fail while turn is active")
	}
	got, replyCtx, ok := registry.AttachedTarget(info.ID)
	if !ok || got.AttachedKey != "feishu:user1" || replyCtx != "reply-ctx" {
		t.Fatalf("AttachedTarget after same-session reattach = (%#v, %#v, %v), want original attachment", got, replyCtx, ok)
	}
	if err := registry.Attach(info.ID, "feishu:user2", "other-reply-ctx"); err == nil {
		t.Fatal("expected different-session attach to fail while turn is active")
	}
	got, replyCtx, ok = registry.AttachedTarget(info.ID)
	if !ok || got.AttachedKey != "feishu:user1" || replyCtx != "reply-ctx" {
		t.Fatalf("AttachedTarget after different-session attach = (%#v, %#v, %v), want original attachment", got, replyCtx, ok)
	}
}

func TestTerminalRegistry_AttachDetachWorkWhenNoActiveTurn(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	if err := registry.Attach(info.ID, "feishu:user1", "reply-ctx"); err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if err := registry.Attach(info.ID, "feishu:user1", "new-reply-ctx"); err != nil {
		t.Fatalf("same-session reattach without active turn returned error: %v", err)
	}
	got, replyCtx, ok := registry.AttachedTarget(info.ID)
	if !ok || got.AttachedKey != "feishu:user1" || replyCtx != "new-reply-ctx" {
		t.Fatalf("AttachedTarget after reattach = (%#v, %#v, %v), want updated attachment", got, replyCtx, ok)
	}
	if err := registry.Detach("feishu:user1"); err != nil {
		t.Fatalf("Detach without active turn returned error: %v", err)
	}
	if _, _, ok := registry.AttachedTarget(info.ID); ok {
		t.Fatal("expected terminal to be detached")
	}
	if err := registry.Attach(info.ID, "feishu:user2", "other-reply-ctx"); err != nil {
		t.Fatalf("attach after detach returned error: %v", err)
	}
	got, replyCtx, ok = registry.AttachedTarget(info.ID)
	if !ok || got.AttachedKey != "feishu:user2" || replyCtx != "other-reply-ctx" {
		t.Fatalf("AttachedTarget after attach to new session = (%#v, %#v, %v), want new attachment", got, replyCtx, ok)
	}
}

func TestTerminalRegistry_StartLocalInputForTurnDoesNotQueueInputAndUsesCurrentReplyModeWhenAttached(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	if err := registry.Attach(info.ID, "feishu:user1", "reply-ctx"); err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if !registry.SetReplyMode(info.ID, terminalReplyModeScreenshotProgress) {
		t.Fatal("expected SetReplyMode to succeed")
	}

	turnID, mode, target, err := registry.StartLocalInputForTurnTarget(info.ID, "local prompt")
	if err != nil {
		t.Fatalf("StartLocalInputForTurnTarget returned error: %v", err)
	}
	if !target.Attached {
		t.Fatal("expected attached receiver")
	}
	if target.SessionKey != "feishu:user1" || target.ReplyCtx != "reply-ctx" {
		t.Fatalf("target = %#v, want atomically captured attachment", target)
	}
	if turnID == 0 {
		t.Fatal("expected active turn id")
	}
	if mode != terminalReplyModeScreenshotProgress {
		t.Fatalf("mode = %v, want %v", mode, terminalReplyModeScreenshotProgress)
	}
	activeTurnID, activeMode, active := registry.ActiveTurn(info.ID)
	if !active || activeTurnID != turnID || activeMode != terminalReplyModeScreenshotProgress {
		t.Fatalf("ActiveTurn = (%d, %v, %v), want (%d, %v, true)", activeTurnID, activeMode, active, turnID, terminalReplyModeScreenshotProgress)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if input, ok := registry.NextInputContext(ctx, info.ID); ok {
		t.Fatalf("NextInputContext returned queued input %q, want none", input)
	}
}

func TestTerminalRegistry_StartLocalInputForTurnNoOpsWhenTerminalIsNotAttached(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})

	turnID, _, attached, err := registry.StartLocalInputForTurn(info.ID, "local prompt")
	if err != nil {
		t.Fatalf("StartLocalInputForTurn returned error: %v", err)
	}
	if attached {
		t.Fatal("expected no attached receiver")
	}
	if turnID != 0 {
		t.Fatalf("turnID = %d, want 0", turnID)
	}
	if activeTurnID, _, active := registry.ActiveTurn(info.ID); active || activeTurnID != 0 {
		t.Fatalf("ActiveTurn = (%d, active=%v), want no active turn", activeTurnID, active)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if input, ok := registry.NextInputContext(ctx, info.ID); ok {
		t.Fatalf("NextInputContext returned queued input %q, want none", input)
	}
}

func TestTerminalRegistry_LatestTurnScreenSnapshotSurvivesCompleteTurnScreenshot(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	turnID, err := registry.StartTerminalInputForTurn(info.ID, "remote prompt", terminalReplyModeScreenshot)
	if err != nil {
		t.Fatalf("StartTerminalInputForTurn returned error: %v", err)
	}
	registry.IngestOutput(info.ID, "latest output")

	if !registry.CompleteTurnScreenshot(info.ID, turnID, true) {
		t.Fatal("expected CompleteTurnScreenshot to succeed")
	}
	if _, _, active := registry.ActiveTurn(info.ID); active {
		t.Fatal("expected active turn to be cleared")
	}
	screen, ok := registry.LatestTurnScreenSnapshot(info.ID)
	if !ok || screen == nil {
		t.Fatalf("LatestTurnScreenSnapshot = (%v, %v), want screen", screen, ok)
	}
	if got := screen.text(); !strings.Contains(got, "latest output") {
		t.Fatalf("latest screen text = %q, want latest output", got)
	}
}

func TestTerminalRegistry_ActiveOrLatestTurnScreenSnapshotPrefersActiveTurnOverPreviousLatest(t *testing.T) {
	registry := NewTerminalRegistry("test")
	info := registry.Register(TerminalRegisterRequest{Project: "test", WorkDir: "/tmp/project"})
	previousTurnID, err := registry.StartTerminalInputForTurn(info.ID, "remote prompt", terminalReplyModeScreenshot)
	if err != nil {
		t.Fatalf("StartTerminalInputForTurn returned error: %v", err)
	}
	registry.IngestOutput(info.ID, "previous output")
	if !registry.CompleteTurnScreenshot(info.ID, previousTurnID, true) {
		t.Fatal("expected first CompleteTurnScreenshot to succeed")
	}
	if !registry.SetReplyMode(info.ID, terminalReplyModeText) {
		t.Fatal("expected SetReplyMode to succeed")
	}
	activeTurnID, _, attached, err := registry.StartLocalInputForTurn(info.ID, "local prompt")
	if err != nil {
		t.Fatalf("StartLocalInputForTurn returned error: %v", err)
	}
	if attached {
		t.Fatal("expected no remote receiver for local turn")
	}
	if activeTurnID != 0 {
		t.Fatalf("activeTurnID = %d, want 0 without attached receiver", activeTurnID)
	}
	if err := registry.Attach(info.ID, "feishu:user1", "reply-ctx"); err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	activeTurnID, _, attached, err = registry.StartLocalInputForTurn(info.ID, "local prompt")
	if err != nil {
		t.Fatalf("StartLocalInputForTurn returned error: %v", err)
	}
	if !attached || activeTurnID == 0 {
		t.Fatalf("StartLocalInputForTurn = (%d, attached=%v), want active attached turn", activeTurnID, attached)
	}
	registry.IngestOutput(info.ID, "active text output")

	screen, ok := registry.ActiveOrLatestTurnScreenSnapshot(info.ID)
	if !ok || screen == nil {
		t.Fatalf("ActiveOrLatestTurnScreenSnapshot = (%v, %v), want active screen", screen, ok)
	}
	got := screen.text()
	if !strings.Contains(got, "active text output") {
		t.Fatalf("active/latest screen text = %q, want active text output", got)
	}
	if strings.Contains(got, "previous output") {
		t.Fatalf("active/latest screen text = %q, should prefer active over previous latest", got)
	}
}
