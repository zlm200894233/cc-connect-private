package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type streamRecorder struct {
	header http.Header
	writes chan []byte
	mu     sync.Mutex
	status int
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{
		header: make(http.Header),
		writes: make(chan []byte, 8),
	}
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	cp := append([]byte(nil), p...)
	r.writes <- cp
	return len(p), nil
}

func (r *streamRecorder) Flush() {}

func (r *streamRecorder) Status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func TestHandleCLIBridgeAttach_ProjectNotFound(t *testing.T) {
	api := &APIServer{engines: map[string]*Engine{"test": newTestEngine()}}
	body, err := json.Marshal(CLIBridgeAttachRequest{Project: "missing"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/cli-bridge/attach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleCLIBridgeAttach(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCLIBridgeAttach_StreamsReadyFrame(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "feishu:user1"
	e.interactiveStates[key] = &interactiveState{
		agentSession: newControllableSession("agent-session-123"),
		platform:     p,
		replyCtx:     "reply-ctx",
	}
	api := &APIServer{engines: map[string]*Engine{"test": e}}
	body, err := json.Marshal(CLIBridgeAttachRequest{Project: "test", SessionKey: key})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/cli-bridge/attach", bytes.NewReader(body)).WithContext(ctx)
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		api.handleCLIBridgeAttach(rec, req)
		close(done)
	}()

	var frame CLIBridgeFrame
	select {
	case chunk := <-rec.writes:
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(chunk))), &frame); err != nil {
			t.Fatalf("unmarshal frame %q: %v", string(chunk), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ready frame")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attach handler did not exit after context cancel")
	}

	if rec.Status() != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Status())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}
	if frame.Type != "ready" || frame.Project != "test" || frame.SessionKey != key || frame.AgentSessionID != "agent-session-123" {
		t.Fatalf("ready frame = %#v", frame)
	}
}

func TestHandleCLIBridgeInput_SubmitsMessage(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	sess := newQueuingSession("busy-session")
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)
	key := "feishu:user1"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID(sess.CurrentSessionID(), e.agent.Name())
	if !session.TryLock() {
		t.Fatal("expected session lock")
	}
	defer session.UnlockWithoutUpdate()
	state := &interactiveState{
		agentSession: sess,
		platform:     p,
		replyCtx:     "reply-ctx",
	}
	e.interactiveStates[key] = state
	api := &APIServer{engines: map[string]*Engine{"test": e}}
	body, err := json.Marshal(CLIBridgeInputRequest{Project: "test", SessionKey: key, Message: " hello from api "})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/cli-bridge/input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleCLIBridgeInput(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 1 {
		t.Fatalf("pendingMessages len = %d, want 1", len(state.pendingMessages))
	}
	if got := state.pendingMessages[0].content; got != "hello from api" {
		t.Fatalf("queued content = %q, want hello from api", got)
	}
}

func TestHandleSend_AllowsAttachmentOnly(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, []Platform{&stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}, "", LangEnglish)
	engine.interactiveStates["session-1"] = &interactiveState{
		platform: &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		replyCtx: "reply-ctx",
	}

	api := &APIServer{engines: map[string]*Engine{"test": engine}}
	reqBody := SendRequest{
		Project:    "test",
		SessionKey: "session-1",
		Images: []ImageAttachment{{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "chart.png",
		}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTerminalRegisterAndList(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	regReq := TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`}
	regBody, err := json.Marshal(regReq)
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/register", bytes.NewReader(regBody))
	rec := httptest.NewRecorder()
	api.handleTerminalRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var regInfo TerminalSessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &regInfo); err != nil {
		t.Fatalf("unmarshal register response: %v", err)
	}
	if regInfo.WorkDir != `E:\\repo` {
		t.Fatalf("register response WorkDir = %q, want %q", regInfo.WorkDir, `E:\\repo`)
	}
	if regInfo.Project != "ClaudeCode" {
		t.Fatalf("register response Project = %q, want %q", regInfo.Project, "ClaudeCode")
	}

	listReq := struct {
		Project string `json:"project"`
	}{
		Project: "ClaudeCode",
	}
	listBody, err := json.Marshal(listReq)
	if err != nil {
		t.Fatalf("marshal list request: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/terminal/list", bytes.NewReader(listBody))
	rec = httptest.NewRecorder()
	api.handleTerminalList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var list []TerminalSessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list returned %d sessions, want 1", len(list))
	}
	if list[0].WorkDir != `E:\\repo` {
		t.Fatalf("listed WorkDir = %q, want %q", list[0].WorkDir, `E:\\repo`)
	}
}

func TestHandleTerminalListAllProjects(t *testing.T) {
	first := NewEngine("first", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	first.SetTerminalRegistry(NewTerminalRegistry("first"))
	second := NewEngine("second", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	second.SetTerminalRegistry(NewTerminalRegistry("second"))
	first.TerminalRegistry().Register(TerminalRegisterRequest{Project: "first", WorkDir: `E:\\first`})
	second.TerminalRegistry().Register(TerminalRegisterRequest{Project: "second", WorkDir: `E:\\second`})
	api := &APIServer{engines: map[string]*Engine{"first": first, "second": second}}

	body, err := json.Marshal(struct {
		Project string `json:"project"`
	}{})
	if err != nil {
		t.Fatalf("marshal list request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/list", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var list []TerminalSessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list returned %d sessions, want 2", len(list))
	}
}

func TestHandleTerminalAttachConflictReturnsConflict(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", nil); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	engine.interactiveStates["feishu:chat:second"] = &interactiveState{replyCtx: "reply-ctx"}
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	body, err := json.Marshal(TerminalAttachRequest{TerminalID: info.ID, SessionKey: "feishu:chat:second"})
	if err != nil {
		t.Fatalf("marshal attach request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/attach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalAttach(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("attach status = %d, body=%s, want 409", rec.Code, rec.Body.String())
	}
}

func TestHandleTerminalDetachActiveTurnReturnsConflict(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", nil); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	if err := engine.TerminalRegistry().SendInput(info.ID, "hello"); err != nil {
		t.Fatalf("start active turn: %v", err)
	}
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	body, err := json.Marshal(TerminalDetachRequest{SessionKey: "feishu:chat:first"})
	if err != nil {
		t.Fatalf("marshal detach request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/detach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalDetach(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("detach status = %d, body=%s, want 409", rec.Code, rec.Body.String())
	}
	if got, _, ok := engine.TerminalRegistry().AttachedTarget(info.ID); !ok || got.AttachedKey != "feishu:chat:first" {
		t.Fatalf("attachment after failed detach = (%#v, %v), want preserved", got, ok)
	}
}

func TestHandleTerminalAttachStoresLiveReplyContext(t *testing.T) {
	p := &ctxRecordingPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	engine.interactiveStates["feishu:chat:first"] = &interactiveState{platform: p, replyCtx: "reply-ctx"}
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	attachBody, err := json.Marshal(TerminalAttachRequest{TerminalID: info.ID, SessionKey: "feishu:chat:first"})
	if err != nil {
		t.Fatalf("marshal attach request: %v", err)
	}
	attachReq := httptest.NewRequest(http.MethodPost, "/terminal/attach", bytes.NewReader(attachBody))
	attachRec := httptest.NewRecorder()
	api.handleTerminalAttach(attachRec, attachReq)
	if attachRec.Code != http.StatusOK {
		t.Fatalf("attach status = %d, body=%s", attachRec.Code, attachRec.Body.String())
	}
	if !engine.TerminalRegistry().SetReplyMode(info.ID, terminalReplyModeText) {
		t.Fatal("set text mode failed")
	}

	outputBody, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "●hello after api attach\n✻ Sautéed for 1s"})
	if err != nil {
		t.Fatalf("marshal output request: %v", err)
	}
	outputReq := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(outputBody))
	outputRec := httptest.NewRecorder()
	api.handleTerminalOutput(outputRec, outputReq)
	if outputRec.Code != http.StatusOK {
		t.Fatalf("output status = %d, body=%s", outputRec.Code, outputRec.Body.String())
	}

	sent := p.getSent()
	if len(sent) != 1 || sent[0] != "●hello after api attach" {
		t.Fatalf("sent = %#v, want output message", sent)
	}
	ctxs := p.getCtxs()
	if len(ctxs) != 1 || ctxs[0] != "reply-ctx" {
		t.Fatalf("ctxs = %#v, want reply-ctx", ctxs)
	}
}

func TestHandleTerminalAttachWithoutLiveReplyContextReturnsNotFound(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "feishu"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	body, err := json.Marshal(TerminalAttachRequest{TerminalID: info.ID, SessionKey: "feishu:chat:first"})
	if err != nil {
		t.Fatalf("marshal attach request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/attach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalAttach(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attach status = %d, body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestHandleTerminalLocalInputStartsTurnForAttachedTerminal(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}

	body, err := json.Marshal(TerminalLocalInputRequest{TerminalID: info.ID, Content: "local hello"})
	if err != nil {
		t.Fatalf("marshal local input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/local-input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalLocalInput(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, mode, active := engine.TerminalRegistry().ActiveTurn(info.ID); !active || mode != terminalReplyModeScreenshot {
		t.Fatalf("active local turn = (active=%v, mode=%v), want screenshot turn", active, mode)
	}
	if got := p.getSent(); len(got) != 1 || got[0] != "Local terminal input received." || strings.Contains(got[0], "local hello") {
		t.Fatalf("local input notice = %#v, want safe content-free notice", got)
	}
	select {
	case input := <-engine.TerminalRegistry().sessions[info.ID].inputCh:
		t.Fatalf("local input queued PTY input %q", input)
	default:
	}
}

func TestHandleTerminalLocalInputNoOpsForUnattachedTerminal(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})

	body, err := json.Marshal(TerminalLocalInputRequest{TerminalID: info.ID, Content: "local hello"})
	if err != nil {
		t.Fatalf("marshal local input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/local-input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalLocalInput(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, _, active := engine.TerminalRegistry().ActiveTurn(info.ID); active {
		t.Fatal("unattached local input should not create active turn")
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("unattached local input sent notice = %#v", got)
	}
}

func TestHandleTerminalLocalInputUsesCurrentReplyMode(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	if !engine.TerminalRegistry().SetReplyMode(info.ID, terminalReplyModeScreenshotProgress) {
		t.Fatal("set reply mode failed")
	}

	body, err := json.Marshal(TerminalLocalInputRequest{TerminalID: info.ID, Content: "local hello"})
	if err != nil {
		t.Fatalf("marshal local input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/local-input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalLocalInput(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, mode, active := engine.TerminalRegistry().ActiveTurn(info.ID); !active || mode != terminalReplyModeScreenshotProgress {
		t.Fatalf("active local turn = (active=%v, mode=%v), want screenshot-progress turn", active, mode)
	}
}

func TestHandleTerminalLocalInputActiveTurnReturnsConflict(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	if err := engine.TerminalRegistry().SendInput(info.ID, "remote hello"); err != nil {
		t.Fatalf("start active turn: %v", err)
	}

	body, err := json.Marshal(TerminalLocalInputRequest{TerminalID: info.ID, Content: "local hello"})
	if err != nil {
		t.Fatalf("marshal local input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/local-input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalLocalInput(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body=%s, want 409", rec.Code, rec.Body.String())
	}
	got, ok := engine.TerminalRegistry().NextInput(info.ID)
	if !ok || got != "remote hello" {
		t.Fatalf("queued PTY input after conflict = (%q, %v), want original remote input", got, ok)
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("conflicted local input sent notice = %#v", got)
	}
}

func TestHandleTerminalLocalInputBlankContentNoOps(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}

	body, err := json.Marshal(TerminalLocalInputRequest{TerminalID: info.ID, Content: "   \t"})
	if err != nil {
		t.Fatalf("marshal local input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/local-input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalLocalInput(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, _, active := engine.TerminalRegistry().ActiveTurn(info.ID); active {
		t.Fatal("blank local input should not create active turn")
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("blank local input sent notice = %#v", got)
	}
}

func TestHandleTerminalInputQueuesLine(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})

	inReq := TerminalInputRequest{
		TerminalID: info.ID,
		Content:    "hello from terminal input",
	}
	inBody, err := json.Marshal(inReq)
	if err != nil {
		t.Fatalf("marshal input request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/input", bytes.NewReader(inBody))
	rec := httptest.NewRecorder()
	api.handleTerminalInput(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("input status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got, ok := engine.TerminalRegistry().NextInput(info.ID)
	if !ok {
		t.Fatal("expected queued input, got none")
	}
	if got != "hello from terminal input" {
		t.Fatalf("queued input = %q, want %q", got, "hello from terminal input")
	}
}

func TestHandleTerminalNextInputReturnsQueuedLine(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().SendInput(info.ID, "queued input"); err != nil {
		t.Fatalf("queue input: %v", err)
	}

	body, err := json.Marshal(TerminalNextInputRequest{TerminalID: info.ID})
	if err != nil {
		t.Fatalf("marshal next input request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/terminal/input/next", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	api.handleTerminalNextInput(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("next input status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response TerminalNextInputResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal next input response: %v", err)
	}
	if response.Content != "queued input" {
		t.Fatalf("next input content = %q, want queued input", response.Content)
	}
}

func TestHandleTerminalNextInputCanceledContextDoesNotConsumeQueuedLine(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().SendInput(info.ID, "preserved input"); err != nil {
		t.Fatalf("queue input: %v", err)
	}

	body, err := json.Marshal(TerminalNextInputRequest{TerminalID: info.ID})
	if err != nil {
		t.Fatalf("marshal next input request: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/terminal/input/next", bytes.NewReader(body)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	api.handleTerminalNextInput(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("next input status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response TerminalNextInputResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal next input response: %v", err)
	}
	if response.Content != "" {
		t.Fatalf("next input content = %q, want empty after cancellation", response.Content)
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	defer readCancel()
	got, ok := engine.TerminalRegistry().NextInputContext(readCtx, info.ID)
	if !ok {
		t.Fatal("queued input was consumed by canceled next-input request")
	}
	if got != "preserved input" {
		t.Fatalf("queued input after canceled request = %q, want preserved input", got)
	}
}

func TestHandleTerminalDetachFindsAttachedSessionAcrossEngines(t *testing.T) {
	first := NewEngine("first", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	first.SetTerminalRegistry(NewTerminalRegistry("first"))
	second := NewEngine("second", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	second.SetTerminalRegistry(NewTerminalRegistry("second"))
	api := &APIServer{engines: map[string]*Engine{"first": first, "second": second}}

	info := second.TerminalRegistry().Register(TerminalRegisterRequest{Project: "second", WorkDir: `E:\\repo`})
	if err := second.TerminalRegistry().Attach(info.ID, "feishu:chat:user", nil); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}

	body, err := json.Marshal(TerminalDetachRequest{SessionKey: "feishu:chat:user"})
	if err != nil {
		t.Fatalf("marshal detach request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/detach", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalDetach(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got, ok := second.TerminalRegistry().Get(info.ID)
	if !ok {
		t.Fatalf("terminal %s should still exist", info.ID)
	}
	if got.AttachedKey != "" {
		t.Fatalf("AttachedKey after detach = %q, want empty", got.AttachedKey)
	}
}

func TestHandleTerminalOutputForwardsToAttachedSession(t *testing.T) {
	p := &ctxRecordingPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	if err := engine.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	if !engine.TerminalRegistry().SetReplyMode(info.ID, terminalReplyModeText) {
		t.Fatal("set text mode failed")
	}

	body, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "●hello from terminal\n✻ Sautéed for 1s"})
	if err != nil {
		t.Fatalf("marshal output request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalOutput(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	sent := p.getSent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(sent))
	}
	if sent[0] != "●hello from terminal" {
		t.Fatalf("sent = %q, want %q", sent[0], "●hello from terminal")
	}
}

func TestHandleTerminalOutputUpdatesScreenBeforeForwarding(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": e}}

	info := e.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})

	body, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "SCREEN_UPDATE_BEFORE_ATTACH\n"})
	if err != nil {
		t.Fatalf("marshal output request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleTerminalOutput(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("expected no platform sends when terminal is unattached, got %v", got)
	}

	snapshot, ok := e.TerminalRegistry().TerminalScreenSnapshot(info.ID)
	if !ok || snapshot == nil {
		t.Fatalf("terminal screen snapshot missing for %q", info.ID)
	}
	if !strings.Contains(snapshot.text(), "SCREEN_UPDATE_BEFORE_ATTACH") {
		t.Fatalf("terminal screen did not record output; got %q", snapshot.text())
	}
}

func TestHandleTerminalOutputScreenshotDoesNotRequireTextEmission(t *testing.T) {
	withTerminalScreenshotFinalIdleDelay(t, 20*time.Millisecond, func() {
		p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
		e := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
		e.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
		info := e.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
		if err := e.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
			t.Fatalf("attach terminal: %v", err)
		}
		if err := e.TerminalRegistry().SendInput(info.ID, "hello"); err != nil {
			t.Fatalf("start screenshot turn: %v", err)
		}
		api := &APIServer{engines: map[string]*Engine{"ClaudeCode": e}}

		intermediateBody, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: strings.Repeat("line\n", defaultTerminalScreenHeight+5)})
		if err != nil {
			t.Fatalf("marshal intermediate output request: %v", err)
		}
		intermediateReq := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(intermediateBody))
		intermediateRec := httptest.NewRecorder()
		api.handleTerminalOutput(intermediateRec, intermediateReq)
		if intermediateRec.Code != http.StatusOK {
			t.Fatalf("intermediate status = %d, body=%s", intermediateRec.Code, intermediateRec.Body.String())
		}
		if images := p.getImages(); len(images) != 0 || len(p.getSent()) != 0 {
			t.Fatalf("expected no intermediate sends, images=%d text=%v", len(images), p.getSent())
		}

		body, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "✻ Sautéed for 1s"})
		if err != nil {
			t.Fatalf("marshal output request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		api.handleTerminalOutput(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		if images := p.getImages(); len(images) != 0 || len(p.getSent()) != 0 {
			t.Fatalf("expected no sends before final idle, images=%d text=%v", len(images), p.getSent())
		}
		if _, _, active := e.TerminalRegistry().ActiveTurn(info.ID); !active {
			t.Fatal("expected screenshot turn to remain active until final idle")
		}

		eventually(t, func() bool {
			_, _, active := e.TerminalRegistry().ActiveTurn(info.ID)
			return len(p.getImages()) == 2 && !active
		})
		if len(p.getSent()) != 0 {
			t.Fatalf("expected no text message when screenshot succeeds, got %v", p.getSent())
		}
		images := p.getImages()
		if images[0].FileName != "terminal-"+info.ID+"-01.png" || images[1].FileName != "terminal-"+info.ID+"-02.png" {
			t.Fatalf("screenshot filenames = %#v", []string{images[0].FileName, images[1].FileName})
		}
	})
}

func TestHandleTerminalOutputScreenshotProgressSendsProgressThenDelayedFinalImage(t *testing.T) {
	withTerminalScreenshotFinalIdleDelay(t, 20*time.Millisecond, func() {
		oldMinInterval := terminalProgressScreenshotMinInterval
		terminalProgressScreenshotMinInterval = 0
		defer func() { terminalProgressScreenshotMinInterval = oldMinInterval }()

		p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "feishu"}}
		e := NewEngine("ClaudeCode", &stubAgent{}, []Platform{p}, "", LangEnglish)
		e.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
		info := e.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
		if err := e.TerminalRegistry().Attach(info.ID, "feishu:chat:first", "reply-ctx"); err != nil {
			t.Fatalf("attach terminal: %v", err)
		}
		if _, err := e.TerminalRegistry().StartTerminalInputForTurn(info.ID, "hello", terminalReplyModeScreenshotProgress); err != nil {
			t.Fatalf("start screenshot-progress turn: %v", err)
		}
		api := &APIServer{engines: map[string]*Engine{"ClaudeCode": e}}

		progressBody, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "● Skill(searxng-search)\n  ⎿  Successfully loaded skill"})
		if err != nil {
			t.Fatalf("marshal progress output request: %v", err)
		}
		progressReq := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(progressBody))
		progressRec := httptest.NewRecorder()
		api.handleTerminalOutput(progressRec, progressReq)
		if progressRec.Code != http.StatusOK {
			t.Fatalf("progress status = %d, body=%s", progressRec.Code, progressRec.Body.String())
		}
		if images := p.getImages(); len(images) != 1 {
			t.Fatalf("expected one progress image, got %d", len(images))
		}
		if len(p.getSent()) != 0 {
			t.Fatalf("expected no text for progress screenshot, got %v", p.getSent())
		}
		if _, _, active := e.TerminalRegistry().ActiveTurn(info.ID); !active {
			t.Fatal("progress screenshot should keep active turn")
		}

		finalBody, err := json.Marshal(TerminalOutputRequest{TerminalID: info.ID, Type: "output", Content: "● final answer\n✻ Sautéed for 1s"})
		if err != nil {
			t.Fatalf("marshal final output request: %v", err)
		}
		finalReq := httptest.NewRequest(http.MethodPost, "/terminal/output", bytes.NewReader(finalBody))
		finalRec := httptest.NewRecorder()
		api.handleTerminalOutput(finalRec, finalReq)
		if finalRec.Code != http.StatusOK {
			t.Fatalf("final status = %d, body=%s", finalRec.Code, finalRec.Body.String())
		}
		if images := p.getImages(); len(images) != 1 {
			t.Fatalf("expected final screenshot to wait for idle, got %d images", len(images))
		}
		if _, _, active := e.TerminalRegistry().ActiveTurn(info.ID); !active {
			t.Fatal("completion candidate should keep active turn before final idle")
		}

		eventually(t, func() bool {
			_, _, active := e.TerminalRegistry().ActiveTurn(info.ID)
			return len(p.getImages()) == 2 && !active
		})
		if len(p.getSent()) != 0 {
			t.Fatalf("expected no text when screenshot-progress succeeds, got %v", p.getSent())
		}
		images := p.getImages()
		if !strings.HasPrefix(images[0].FileName, "terminal-"+info.ID+"-1-") || images[1].FileName != "terminal-"+info.ID+".png" {
			t.Fatalf("screenshot-progress filenames = %#v", []string{images[0].FileName, images[1].FileName})
		}
	})
}

func TestHandleTerminalMissingRequiredFields(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	info := engine.TerminalRegistry().Register(TerminalRegisterRequest{Project: "ClaudeCode", WorkDir: `E:\\repo`})
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	tests := []struct {
		name    string
		path    string
		body    any
		handler func(http.ResponseWriter, *http.Request)
	}{
		{
			name:    "input missing terminal_id",
			path:    "/terminal/input",
			body:    TerminalInputRequest{Content: "line"},
			handler: api.handleTerminalInput,
		},
		{
			name:    "next input missing terminal_id",
			path:    "/terminal/input/next",
			body:    TerminalNextInputRequest{},
			handler: api.handleTerminalNextInput,
		},
		{
			name:    "attach missing session_key",
			path:    "/terminal/attach",
			body:    TerminalAttachRequest{TerminalID: info.ID},
			handler: api.handleTerminalAttach,
		},
		{
			name:    "detach missing session_key",
			path:    "/terminal/detach",
			body:    TerminalDetachRequest{},
			handler: api.handleTerminalDetach,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(body))
			rec := httptest.NewRecorder()
			tt.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleTerminalUnknownTerminalReturnsNotFound(t *testing.T) {
	engine := NewEngine("ClaudeCode", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	engine.SetTerminalRegistry(NewTerminalRegistry("ClaudeCode"))
	api := &APIServer{engines: map[string]*Engine{"ClaudeCode": engine}}

	tests := []struct {
		name    string
		path    string
		body    any
		handler func(http.ResponseWriter, *http.Request)
	}{
		{
			name:    "input unknown terminal",
			path:    "/terminal/input",
			body:    TerminalInputRequest{TerminalID: "term_missing", Content: "line"},
			handler: api.handleTerminalInput,
		},
		{
			name:    "next input unknown terminal",
			path:    "/terminal/input/next",
			body:    TerminalNextInputRequest{TerminalID: "term_missing"},
			handler: api.handleTerminalNextInput,
		},
		{
			name:    "attach unknown terminal",
			path:    "/terminal/attach",
			body:    TerminalAttachRequest{TerminalID: "term_missing", SessionKey: "feishu:chat:user"},
			handler: api.handleTerminalAttach,
		},
		{
			name:    "output unknown terminal",
			path:    "/terminal/output",
			body:    TerminalOutputRequest{TerminalID: "term_missing", Type: "output", Content: "ignored"},
			handler: api.handleTerminalOutput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(body))
			rec := httptest.NewRecorder()
			tt.handler(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body=%s, want 404", rec.Code, rec.Body.String())
			}
		})
	}
}
