package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

type TerminalRegisterRequest struct {
	Project         string `json:"project"`
	WorkDir         string `json:"workdir"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
}

type TerminalRegisterResponse struct {
	ID string `json:"id"`
}

type TerminalUnregisterRequest struct {
	TerminalID string `json:"terminal_id"`
	ExitCode   int    `json:"exit_code"`
}

type TerminalOutputRequest struct {
	TerminalID string `json:"terminal_id"`
	Type       string `json:"type"`
	Content    string `json:"content,omitempty"`
	Error      string `json:"error,omitempty"`
}

type TerminalInputRequest struct {
	TerminalID string `json:"terminal_id"`
	Content    string `json:"content"`
}

type TerminalLocalInputRequest struct {
	TerminalID string `json:"terminal_id"`
	Content    string `json:"content"`
}

type TerminalNextInputRequest struct {
	TerminalID string `json:"terminal_id"`
}

type TerminalNextInputResponse struct {
	Content string `json:"content"`
}

type TerminalAttachRequest struct {
	TerminalID string `json:"terminal_id"`
	SessionKey string `json:"session_key"`
}

type TerminalDetachRequest struct {
	SessionKey string `json:"session_key"`
}

type TerminalSessionInfo struct {
	ID              string    `json:"id"`
	Project         string    `json:"project"`
	WorkDir         string    `json:"workdir"`
	ClaudeSessionID string    `json:"claude_session_id,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	AttachedKey     string    `json:"attached_key,omitempty"`
	Alive           bool      `json:"alive"`
}

type TerminalRegistry struct {
	project  string
	mu       sync.RWMutex
	sessions map[string]*terminalSession
}

type terminalReplyMode int

const (
	terminalReplyModeText terminalReplyMode = iota
	terminalReplyModeScreenshot
	terminalReplyModeScreenshotProgress
)

var terminalProgressScreenshotMinInterval = 10 * time.Second
var terminalScreenshotFinalIdleDelay = 1500 * time.Millisecond

var ErrTerminalTurnActive = errors.New("terminal turn still processing")

const terminalProgressScreenshotMaxPerTurn = 5

func terminalReplyModeName(mode terminalReplyMode) string {
	switch mode {
	case terminalReplyModeScreenshot:
		return "screenshot"
	case terminalReplyModeScreenshotProgress:
		return "screenshot-progress"
	default:
		return "text"
	}
}

func parseTerminalReplyMode(value string) (terminalReplyMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "screenshot-progress":
		return terminalReplyModeScreenshotProgress, true
	default:
		return terminalReplyModeText, false
	}
}

func terminalReplyModeUsesScreenshots(mode terminalReplyMode) bool {
	return mode == terminalReplyModeScreenshot || mode == terminalReplyModeScreenshotProgress
}

type terminalLocalInputTarget struct {
	Attached   bool
	SessionKey string
	ReplyCtx   any
}

type terminalSession struct {
	info                           TerminalSessionInfo
	replyCtx                       any
	localOutputSessionKey          string
	localOutputReplyCtx            any
	inputCh                        chan string
	done                           chan struct{}
	status                         terminalStatusPreview
	output                         terminalOutputBuffer
	screen                         *terminalScreen
	replyMode                      terminalReplyMode
	turnID                         uint64
	activeTurnID                   uint64
	activeTurnMode                 terminalReplyMode
	activeTurnLocalOutput          bool
	turnScreenshotInFlight         bool
	turnMessages                   []string
	turnScreen                     *terminalScreen
	turnProgressScreenshotInFlight bool
	turnProgressScreenshotsSent    int
	turnLastProgressScreenshotAt   time.Time
	turnLastProgressSignature      string
	turnLastOutputAt               time.Time
	turnIdleGeneration             uint64
	turnCompletionCandidate        bool
	lastTurnScreen                 *terminalScreen
}

type terminalOutputBuffer struct {
	pending     []string
	lastEmitted string
}

type terminalStatusPreview struct {
	handle  any
	metrics terminalProgressMetrics
}

type terminalProgressMetrics struct {
	compacting       bool
	progress         string
	elapsed          string
	tokens           string
	thought          string
	contextUsed      string
	contextRemaining string
	contextNA        bool
}

var terminalIDCounter atomic.Uint64

var (
	terminalOSCSequence     = regexp.MustCompile(`\x1b\][^\a]*(?:\a|\x1b\\)`)
	terminalCSISequence     = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	terminalESCSequence     = regexp.MustCompile(`\x1b[ -/]*[@-~]`)
	terminalElapsedParen    = regexp.MustCompile(`\((\d+(?:h|m|s)(?:\s+\d+s)?)\s*·`)
	terminalElapsedCooked   = regexp.MustCompile(`(?i)(?:cooked|sautéed|sauteed|crunched|baked)\s+(?:for|fo)\s+([0-9hm\s]+s)`)
	terminalTokenUsage      = regexp.MustCompile(`([↓↑])\s*([0-9.]+k?)\s*tokens`)
	terminalThoughtDuration = regexp.MustCompile(`(?i)thought\s+for\s+([^\)·]+)`)
	terminalContextUsage    = regexp.MustCompile(`(?i)context:\s*(\d+)%\s*used,\s*(\d+)%\s*remaining`)
	terminalAutoCompact     = regexp.MustCompile(`(?i)(\d+)%\s*until\s*auto-compact`)
	terminalPercent         = regexp.MustCompile(`(\d{1,3})%`)
)

func NewTerminalRegistry(project string) *TerminalRegistry {
	return &TerminalRegistry{
		project:  project,
		sessions: make(map[string]*terminalSession),
	}
}

func (r *TerminalRegistry) nextTerminalID() string {
	return fmt.Sprintf("term_%06d", terminalIDCounter.Add(1))
}

func (r *TerminalRegistry) Register(req TerminalRegisterRequest) TerminalSessionInfo {
	project := strings.TrimSpace(req.Project)
	if project == "" {
		project = r.project
	}
	info := TerminalSessionInfo{
		ID:              r.nextTerminalID(),
		Project:         project,
		WorkDir:         req.WorkDir,
		ClaudeSessionID: req.ClaudeSessionID,
		StartedAt:       time.Now(),
		Alive:           true,
	}

	r.mu.Lock()
	r.sessions[info.ID] = &terminalSession{
		info:      info,
		inputCh:   make(chan string, 64),
		done:      make(chan struct{}),
		screen:    newTerminalScreen(0, 0),
		replyMode: terminalReplyModeScreenshotProgress,
	}
	r.mu.Unlock()
	return info
}

func (r *TerminalRegistry) List(project string) []TerminalSessionInfo {
	r.mu.RLock()
	out := make([]TerminalSessionInfo, 0, len(r.sessions))
	for _, session := range r.sessions {
		if project != "" && session.info.Project != project {
			continue
		}
		out = append(out, session.info)
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *TerminalRegistry) Get(id string) (TerminalSessionInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return TerminalSessionInfo{}, false
	}
	return session.info, true
}

func (r *TerminalRegistry) Unregister(id string, _ int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return false
	}
	delete(r.sessions, id)
	close(session.done)
	return true
}

func (r *TerminalRegistry) Attach(id, sessionKey string, replyCtx any) error {
	if id == "" {
		return errors.New("terminal id is required")
	}
	if sessionKey == "" {
		return errors.New("session key is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}
	if session.activeTurnID != 0 {
		return fmt.Errorf("%w: terminal %q is processing an active turn", ErrTerminalTurnActive, id)
	}
	if session.info.AttachedKey != "" && session.info.AttachedKey != sessionKey {
		return fmt.Errorf("terminal %q is already attached to %q", id, session.info.AttachedKey)
	}
	for otherID, other := range r.sessions {
		if otherID != id && other.info.AttachedKey == sessionKey {
			return fmt.Errorf("session %q is already attached to terminal %q", sessionKey, otherID)
		}
	}

	session.info.AttachedKey = sessionKey
	session.replyCtx = replyCtx
	session.localOutputSessionKey = sessionKey
	session.localOutputReplyCtx = replyCtx
	return nil
}

func (r *TerminalRegistry) Detach(sessionKey string) error {
	if sessionKey == "" {
		return errors.New("session key is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, session := range r.sessions {
		if session.info.AttachedKey == sessionKey {
			if session.activeTurnID != 0 {
				return fmt.Errorf("%w: terminal %q is processing an active turn", ErrTerminalTurnActive, session.info.ID)
			}
			session.info.AttachedKey = ""
			session.replyCtx = nil
			return nil
		}
	}
	return fmt.Errorf("no terminal attached for session key %q", sessionKey)
}

func (r *TerminalRegistry) SetReplyMode(id string, mode terminalReplyMode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return false
	}
	session.replyMode = mode
	return true
}

func (r *TerminalRegistry) ReplyMode(id string) (terminalReplyMode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return terminalReplyModeText, false
	}
	return session.replyMode, true
}

func (r *TerminalRegistry) SetAttachedReplyMode(sessionKey string, mode terminalReplyMode) bool {
	if sessionKey == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, session := range r.sessions {
		if session.info.AttachedKey != sessionKey {
			continue
		}
		session.replyMode = mode
		return true
	}
	return false
}

func (r *TerminalRegistry) AttachedReplyMode(sessionKey string) (terminalReplyMode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, session := range r.sessions {
		if session.info.AttachedKey == sessionKey {
			return session.replyMode, true
		}
	}
	return terminalReplyModeText, false
}

func (r *TerminalRegistry) AttachedForSession(sessionKey string) (TerminalSessionInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, session := range r.sessions {
		if session.info.AttachedKey == sessionKey {
			return session.info, true
		}
	}
	return TerminalSessionInfo{}, false
}

func (r *TerminalRegistry) AttachedTarget(terminalID string) (TerminalSessionInfo, any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[terminalID]
	if !ok || session.info.AttachedKey == "" {
		return TerminalSessionInfo{}, nil, false
	}
	return session.info, session.replyCtx, true
}

func (r *TerminalRegistry) TerminalDeliveryTarget(terminalID string) (TerminalSessionInfo, any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[terminalID]
	if !ok {
		return TerminalSessionInfo{}, nil, false
	}
	if session.info.AttachedKey != "" {
		return session.info, session.replyCtx, true
	}
	if session.localOutputSessionKey == "" {
		return TerminalSessionInfo{}, nil, false
	}
	info := session.info
	info.AttachedKey = session.localOutputSessionKey
	return info, session.localOutputReplyCtx, true
}

func (r *TerminalRegistry) TerminalScreenSnapshot(id string) (*terminalScreen, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return nil, false
	}
	if session.screen == nil {
		return nil, true
	}
	return session.screen.clone(), true
}

func (r *TerminalRegistry) LatestTurnScreenSnapshot(id string) (*terminalScreen, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return nil, false
	}
	if session.lastTurnScreen == nil {
		return nil, true
	}
	return session.lastTurnScreen.clone(), true
}

func (r *TerminalRegistry) ActiveOrLatestTurnScreenSnapshot(id string) (*terminalScreen, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return nil, false
	}
	if session.activeTurnID != 0 {
		if screen := session.activeTurnScreenSnapshot(); screen != nil {
			return screen, true
		}
	}
	if session.lastTurnScreen == nil {
		return nil, true
	}
	return session.lastTurnScreen.clone(), true
}

func (r *TerminalRegistry) SendInput(id, content string) error {
	mode, ok := r.ReplyMode(id)
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}
	_, err := r.startTerminalInputForTurn(id, content, mode)
	return err
}

func (r *TerminalRegistry) StartTerminalInputForTurn(id, content string, mode terminalReplyMode) (uint64, error) {
	return r.startTerminalInputForTurn(id, content, mode)
}

func (r *TerminalRegistry) StartLocalInputForTurn(id, content string) (uint64, terminalReplyMode, bool, error) {
	turnID, mode, target, err := r.StartLocalInputForTurnTarget(id, content)
	return turnID, mode, target.Attached, err
}

func (r *TerminalRegistry) StartLocalInputForTurnTarget(id, content string) (uint64, terminalReplyMode, terminalLocalInputTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return 0, terminalReplyModeText, terminalLocalInputTarget{}, fmt.Errorf("terminal %q not found", id)
	}
	mode := session.replyMode
	select {
	case <-session.done:
		return 0, mode, terminalLocalInputTarget{}, fmt.Errorf("terminal %q not found", id)
	default:
	}
	target := terminalLocalInputTarget{}
	if session.info.AttachedKey != "" {
		target = terminalLocalInputTarget{Attached: true, SessionKey: session.info.AttachedKey, ReplyCtx: session.replyCtx}
	} else if session.localOutputSessionKey != "" {
		target = terminalLocalInputTarget{Attached: true, SessionKey: session.localOutputSessionKey, ReplyCtx: session.localOutputReplyCtx}
	} else {
		return 0, mode, terminalLocalInputTarget{}, nil
	}
	if session.activeTurnID != 0 {
		if session.activeTurnLocalOutput {
			return session.activeTurnID, session.activeTurnMode, target, nil
		}
		session.saveLatestTurnScreen()
		for {
			select {
			case <-session.inputCh:
				continue
			default:
				return session.beginTurn(mode, true), mode, target, nil
			}
		}
	}
	return session.beginTurn(mode, true), mode, target, nil
}

func (r *TerminalRegistry) StartAttachedOutputTurnIfIdle(id string) (uint64, terminalReplyMode, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return 0, terminalReplyModeText, false
	}
	select {
	case <-session.done:
		return 0, session.replyMode, false
	default:
	}
	if session.info.AttachedKey == "" || session.activeTurnID != 0 || !terminalReplyModeUsesScreenshots(session.replyMode) {
		return 0, session.replyMode, false
	}
	mode := session.replyMode
	return session.beginTurn(mode, true), mode, true
}

func (r *TerminalRegistry) SendControlInput(id, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("terminal %q not found", id)
	}
	select {
	case <-session.done:
		return fmt.Errorf("terminal %q not found", id)
	default:
	}
	select {
	case <-session.done:
		return fmt.Errorf("terminal %q not found", id)
	case session.inputCh <- content:
		return nil
	default:
		return fmt.Errorf("terminal %q input queue is full", id)
	}
}

func (s *terminalSession) beginTurn(mode terminalReplyMode, localOutput bool) uint64 {
	s.turnID++
	s.activeTurnID = s.turnID
	s.activeTurnMode = mode
	s.activeTurnLocalOutput = localOutput
	s.turnScreenshotInFlight = false
	s.turnProgressScreenshotInFlight = false
	s.turnProgressScreenshotsSent = 0
	s.turnLastProgressScreenshotAt = time.Time{}
	s.turnLastProgressSignature = ""
	s.turnLastOutputAt = time.Time{}
	s.turnIdleGeneration = 0
	s.turnCompletionCandidate = false
	s.turnMessages = nil
	width, height := defaultTerminalScreenWidth, defaultTerminalScreenHeight
	if s.screen != nil {
		width, height = s.screen.width, s.screen.height
	}
	s.turnScreen = newTerminalScreen(width, height)
	s.output = terminalOutputBuffer{}
	return s.activeTurnID
}

func (s *terminalSession) saveLatestTurnScreen() {
	if screen := s.turnMessagesScreen(); screen != nil {
		s.lastTurnScreen = screen
		return
	}
	if s.turnScreen != nil {
		s.lastTurnScreen = s.turnScreen.clone()
	}
}

func (s *terminalSession) turnMessagesScreen() *terminalScreen {
	messages := terminalFinalMessages(s.turnMessages)
	if len(messages) == 0 {
		return nil
	}
	width, height := s.screenSize()
	screen := newTerminalScreen(width, height)
	screen.ingest(strings.Join(messages, "\n\n"))
	return screen
}

func (s *terminalSession) activeTurnScreenSnapshot() *terminalScreen {
	if screen := s.turnMessagesScreen(); screen != nil {
		return screen
	}
	if s.turnScreen != nil {
		return s.turnScreen.clone()
	}
	if s.screen == nil {
		return nil
	}
	return s.screen.clone()
}

func (s *terminalSession) screenSize() (int, int) {
	if s.screen != nil {
		return s.screen.width, s.screen.height
	}
	if s.turnScreen != nil {
		return s.turnScreen.width, s.turnScreen.height
	}
	return defaultTerminalScreenWidth, defaultTerminalScreenHeight
}

func (r *TerminalRegistry) startTerminalInputForTurn(id, content string, mode terminalReplyMode) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok {
		return 0, fmt.Errorf("terminal %q not found", id)
	}

	select {
	case <-session.done:
		return 0, fmt.Errorf("terminal %q not found", id)
	default:
	}
	if session.activeTurnID != 0 {
		return 0, fmt.Errorf("terminal %q is still processing previous input", id)
	}

	select {
	case <-session.done:
		return 0, fmt.Errorf("terminal %q not found", id)
	case session.inputCh <- content:
		return session.beginTurn(mode, false), nil
	default:
		return 0, fmt.Errorf("terminal %q input queue is full", id)
	}
}

func (r *TerminalRegistry) ActiveTurn(id string) (uint64, terminalReplyMode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, ok := r.sessions[id]
	if !ok {
		return 0, terminalReplyModeText, false
	}
	if session.activeTurnID == 0 {
		return 0, session.activeTurnMode, false
	}
	return session.activeTurnID, session.activeTurnMode, true
}

func (r *TerminalRegistry) AppendTurnMessages(id string, turnID uint64, messages []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID != turnID {
		return append([]string(nil), messages...)
	}
	for _, message := range messages {
		message = strings.TrimSpace(message)
		if message != "" {
			session.turnMessages = append(session.turnMessages, message)
		}
	}
	return append([]string(nil), session.turnMessages...)
}

func (r *TerminalRegistry) CompleteActiveTurn(id string, turnID uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID {
		return false
	}
	session.saveLatestTurnScreen()
	session.turnScreenshotInFlight = false
	session.turnProgressScreenshotInFlight = false
	session.turnProgressScreenshotsSent = 0
	session.turnLastProgressScreenshotAt = time.Time{}
	session.turnLastProgressSignature = ""
	session.turnLastOutputAt = time.Time{}
	session.turnIdleGeneration = 0
	session.turnCompletionCandidate = false
	session.turnMessages = nil
	session.turnScreen = nil
	session.activeTurnID = 0
	session.activeTurnMode = terminalReplyModeText
	session.activeTurnLocalOutput = false
	return true
}

func (r *TerminalRegistry) SetStatusPreview(id string, handle any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || handle == nil {
		return false
	}
	session.status = terminalStatusPreview{handle: handle}
	return true
}

func (r *TerminalRegistry) MergeStatusPreview(id string, metrics terminalProgressMetrics) (terminalStatusPreview, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.status.handle == nil {
		return terminalStatusPreview{}, false
	}
	session.status.metrics = mergeTerminalProgressMetrics(session.status.metrics, metrics)
	return session.status, true
}

func (r *TerminalRegistry) NextInput(terminalID string) (string, bool) {
	return r.NextInputContext(context.Background(), terminalID)
}

func (r *TerminalRegistry) NextInputContext(ctx context.Context, terminalID string) (string, bool) {
	r.mu.RLock()
	session, ok := r.sessions[terminalID]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}

	select {
	case <-ctx.Done():
		return "", false
	default:
	}

	select {
	case <-ctx.Done():
		return "", false
	case <-session.done:
		return "", false
	case input := <-session.inputCh:
		return input, true
	}
}

func (e *Engine) SetTerminalRegistry(r *TerminalRegistry) {
	e.terminalRegistry = r
}

func (e *Engine) TerminalRegistry() *TerminalRegistry {
	if e.terminalRegistry == nil {
		e.terminalRegistry = NewTerminalRegistry(e.name)
	}
	return e.terminalRegistry
}

func (e *Engine) terminalReplyContext(sessionKey string) (any, bool) {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return nil, false
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.replyCtx == nil {
		return nil, false
	}
	return state.replyCtx, true
}

func (e *Engine) terminalPlatformFromKey(sessionKey string) (Platform, error) {
	if sessionKey == "" {
		return nil, errors.New("session key is required")
	}

	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}
	if platformName == "" {
		return nil, fmt.Errorf("unsupported session key %q for terminal output", sessionKey)
	}

	for _, p := range e.platforms {
		if p.Name() == platformName {
			return p, nil
		}
	}
	for _, p := range e.platforms {
		needle := ":" + p.Name() + ":"
		if strings.Contains(sessionKey, needle) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unsupported session key %q for terminal output", sessionKey)
}

func terminalDeliveryReplyContext(p Platform, replyCtx any) any {
	provider, ok := p.(TerminalDeliveryReplyContextProvider)
	if !ok {
		return replyCtx
	}
	ctx, ok := provider.TerminalDeliveryReplyCtx(replyCtx)
	if !ok || ctx == nil {
		return replyCtx
	}
	return ctx
}

func (e *Engine) handleAttachedTerminalInput(p Platform, msg *Message, content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	session, ok := e.TerminalRegistry().AttachedForSession(msg.SessionKey)
	if !ok {
		return false
	}
	if err := e.TerminalRegistry().SendInput(session.ID, content); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTerminalSendFailed, err))
		return true
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTerminalProcessing))
	return true
}

func (r *TerminalRegistry) IngestOutput(id, content string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return false
	}
	if session.screen != nil {
		session.screen.ingest(content)
	}
	if session.activeTurnID != 0 {
		if session.turnScreen == nil {
			width, height := defaultTerminalScreenWidth, defaultTerminalScreenHeight
			if session.screen != nil {
				width, height = session.screen.width, session.screen.height
			}
			session.turnScreen = newTerminalScreen(width, height)
		}
		session.turnScreen.ingest(content)
		session.turnLastOutputAt = time.Now()
		session.turnIdleGeneration++
	}
	return true
}

func (r *TerminalRegistry) CollectOutput(id, content string) []string {
	if !r.IngestOutput(id, content) {
		return nil
	}
	return r.collectOutput(id, content)
}

func (r *TerminalRegistry) collectOutput(id, content string, flushTail ...bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return nil
	}
	flush := len(flushTail) > 0 && flushTail[0]
	return session.output.collect(content, flush)
}

func (r *TerminalRegistry) TryClaimTerminalScreenshot(id string) (*terminalScreen, bool) {
	turnID, _, active := r.ActiveTurn(id)
	if !active {
		return nil, false
	}
	return r.TryClaimTurnScreenshot(id, turnID)
}

func (r *TerminalRegistry) TryClaimTurnScreenshot(id string, turnID uint64) (*terminalScreen, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.activeTurnID != turnID || !terminalReplyModeUsesScreenshots(session.activeTurnMode) || session.turnScreenshotInFlight {
		return nil, false
	}
	session.turnScreenshotInFlight = true
	if session.turnScreen != nil {
		return session.turnScreen.clone(), true
	}
	if session.screen == nil {
		return nil, true
	}
	return session.screen.clone(), true
}

func (r *TerminalRegistry) TryClaimTurnProgressScreenshot(id string, turnID uint64, signature string) (*terminalScreen, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.activeTurnID != turnID || session.activeTurnMode != terminalReplyModeScreenshotProgress || session.turnProgressScreenshotInFlight {
		return nil, false
	}
	if session.turnProgressScreenshotsSent >= terminalProgressScreenshotMaxPerTurn {
		return nil, false
	}
	now := time.Now()
	if !session.turnLastProgressScreenshotAt.IsZero() && now.Sub(session.turnLastProgressScreenshotAt) < terminalProgressScreenshotMinInterval {
		return nil, false
	}
	if session.turnProgressScreenshotsSent > 0 && signature == session.turnLastProgressSignature {
		return nil, false
	}

	session.turnProgressScreenshotInFlight = true
	session.turnProgressScreenshotsSent++
	session.turnLastProgressScreenshotAt = now
	session.turnLastProgressSignature = signature
	if session.turnScreen != nil {
		return session.turnScreen.clone(), true
	}
	if session.screen == nil {
		return nil, true
	}
	return session.screen.clone(), true
}

func (r *TerminalRegistry) MarkTurnCompletionCandidate(id string, turnID uint64) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID {
		return 0, false
	}
	session.turnCompletionCandidate = true
	return session.turnIdleGeneration, true
}

func (r *TerminalRegistry) TurnCompletionCandidateGeneration(id string, turnID uint64) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID || !session.turnCompletionCandidate {
		return 0, false
	}
	return session.turnIdleGeneration, true
}

func (r *TerminalRegistry) TryClaimIdleFinalScreenshot(id string, turnID uint64, generation uint64, idleDelay time.Duration) (*terminalScreen, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID || !terminalReplyModeUsesScreenshots(session.activeTurnMode) || !session.turnCompletionCandidate || session.turnScreenshotInFlight || generation != session.turnIdleGeneration || time.Since(session.turnLastOutputAt) < idleDelay {
		return nil, false
	}
	session.turnScreenshotInFlight = true
	if screen := session.turnMessagesScreen(); screen != nil {
		return screen, true
	}
	if session.turnScreen != nil {
		return session.turnScreen.clone(), true
	}
	if session.screen == nil {
		return nil, true
	}
	return session.screen.clone(), true
}

func (r *TerminalRegistry) TryClaimIdleFinalText(id string, turnID uint64, generation uint64, idleDelay time.Duration) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID || session.activeTurnMode != terminalReplyModeText || !session.turnCompletionCandidate || generation != session.turnIdleGeneration || time.Since(session.turnLastOutputAt) < idleDelay {
		return nil, false
	}
	messages := session.output.collect("", true)
	for _, message := range messages {
		message = strings.TrimSpace(message)
		if message != "" {
			session.turnMessages = append(session.turnMessages, message)
		}
	}
	return terminalFinalMessages(session.turnMessages), true
}

func (r *TerminalRegistry) FinishTerminalScreenshot(id string, success bool) bool {
	turnID, _, active := r.ActiveTurn(id)
	if !active {
		return false
	}
	return r.FinishTurnScreenshot(id, turnID, success)
}

func (r *TerminalRegistry) FinishTurnScreenshot(id string, turnID uint64, _ bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.activeTurnID != turnID {
		return false
	}
	session.turnScreenshotInFlight = false
	return true
}

func (r *TerminalRegistry) CompleteTurnScreenshot(id string, turnID uint64, _ bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.sessions[id]
	if !ok || session.activeTurnID == 0 || session.activeTurnID != turnID {
		return false
	}
	session.saveLatestTurnScreen()
	session.turnScreenshotInFlight = false
	session.turnProgressScreenshotInFlight = false
	session.turnProgressScreenshotsSent = 0
	session.turnLastProgressScreenshotAt = time.Time{}
	session.turnLastProgressSignature = ""
	session.turnLastOutputAt = time.Time{}
	session.turnIdleGeneration = 0
	session.turnCompletionCandidate = false
	session.turnMessages = nil
	session.turnScreen = nil
	session.activeTurnID = 0
	session.activeTurnMode = terminalReplyModeText
	session.activeTurnLocalOutput = false
	return true
}

func (r *TerminalRegistry) FinishTurnProgressScreenshot(id string, turnID uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok || session.activeTurnID != turnID || session.activeTurnMode != terminalReplyModeScreenshotProgress {
		return false
	}
	session.turnProgressScreenshotInFlight = false
	return true
}

func terminalScreenshotsEmpty(images []ImageAttachment) bool {
	for _, img := range images {
		if len(img.Data) == 0 {
			return true
		}
	}
	return false
}

func (e *Engine) sendTerminalScreenshotImages(sender ImageSender, replyCtx any, images []ImageAttachment) error {
	for _, img := range images {
		if err := sender.SendImage(e.ctx, replyCtx, img); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) maybeSendTerminalProgressScreenshot(target Platform, replyCtx any, terminalID string, turnID uint64, content string) {
	if !isTerminalProgressScreenshotSignal(content) {
		return
	}
	imageSender, ok := target.(ImageSender)
	if !ok {
		return
	}
	signature := terminalProgressSignature(content)
	reg := e.TerminalRegistry()
	screen, claimed := reg.TryClaimTurnProgressScreenshot(terminalID, turnID, signature)
	if !claimed {
		return
	}
	defer reg.FinishTurnProgressScreenshot(terminalID, turnID)

	progressAt := time.Now().UnixNano()
	progressTerminalID := fmt.Sprintf("%s-%d-%d", terminalID, turnID, progressAt)
	images, err := terminalScreenshotsRenderer(screen, progressTerminalID)
	if err != nil {
		slog.Warn("terminal progress screenshot render failed", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "error", err)
		return
	}
	if len(images) == 0 || terminalScreenshotsEmpty(images) {
		slog.Warn("terminal progress screenshot render produced empty image", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID)
		return
	}
	if err := e.sendTerminalScreenshotImages(imageSender, replyCtx, images); err != nil {
		slog.Warn("terminal progress screenshot send failed", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "error", err)
		return
	}

	slog.Info("terminal progress screenshot sent", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "images", len(images))
}

func (e *Engine) scheduleTerminalFinalScreenshot(target Platform, replyCtx any, terminalID string, turnID uint64, generation uint64) {
	time.AfterFunc(terminalScreenshotFinalIdleDelay, func() {
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		e.sendTerminalFinalScreenshotIfIdle(target, replyCtx, terminalID, turnID, generation, terminalScreenshotFinalIdleDelay)
	})
}

func (e *Engine) scheduleTerminalFinalText(target Platform, replyCtx any, terminalID string, turnID uint64, generation uint64) {
	time.AfterFunc(terminalScreenshotFinalIdleDelay, func() {
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		e.sendTerminalFinalTextIfIdle(target, replyCtx, terminalID, turnID, generation, terminalScreenshotFinalIdleDelay)
	})
}

func (e *Engine) scheduleTerminalFinalForMode(target Platform, replyCtx any, terminalID string, turnID uint64, mode terminalReplyMode, generation uint64) {
	if terminalReplyModeUsesScreenshots(mode) {
		e.scheduleTerminalFinalScreenshot(target, replyCtx, terminalID, turnID, generation)
		return
	}
	if mode == terminalReplyModeText {
		e.scheduleTerminalFinalText(target, replyCtx, terminalID, turnID, generation)
	}
}

func (e *Engine) sendTerminalFinalTextIfIdle(target Platform, replyCtx any, terminalID string, turnID uint64, generation uint64, idleDelay time.Duration) {
	reg := e.TerminalRegistry()
	messages, claimed := reg.TryClaimIdleFinalText(terminalID, turnID, generation, idleDelay)
	if !claimed {
		return
	}
	for _, content := range messages {
		if strings.TrimSpace(content) == "" {
			continue
		}
		if err := e.sendWithError(target, replyCtx, content); err != nil {
			reg.CompleteActiveTurn(terminalID, turnID)
			return
		}
	}
	reg.CompleteActiveTurn(terminalID, turnID)
}

func (e *Engine) sendTerminalFinalScreenshotIfIdle(target Platform, replyCtx any, terminalID string, turnID uint64, generation uint64, idleDelay time.Duration) {
	reg := e.TerminalRegistry()
	fail := func(notice string) {
		reg.CompleteTurnScreenshot(terminalID, turnID, false)
		if strings.TrimSpace(notice) == "" {
			return
		}
		if err := e.sendWithError(target, replyCtx, notice); err != nil {
			slog.Warn("failed to send terminal screenshot failure notice", "terminal_id", terminalID, "platform", target.Name(), "error", err)
		}
	}

	screen, claimed := reg.TryClaimIdleFinalScreenshot(terminalID, turnID, generation, idleDelay)
	if !claimed {
		return
	}

	imageSender, ok := target.(ImageSender)
	if !ok {
		fail(e.i18n.T(MsgTerminalScreenshotImageUnsupported))
		return
	}

	images, err := terminalScreenshotsRenderer(screen, terminalID)
	if err != nil {
		slog.Warn("terminal screenshot render failed", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "error", err)
		fail(e.i18n.Tf(MsgTerminalScreenshotRenderFailed, err))
		return
	}
	if len(images) == 0 || terminalScreenshotsEmpty(images) {
		slog.Warn("terminal screenshot render returned empty image", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID)
		fail(e.i18n.T(MsgTerminalScreenshotEmpty))
		return
	}
	if err := e.sendTerminalScreenshotImages(imageSender, replyCtx, images); err != nil {
		slog.Warn("terminal screenshot send failed", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "error", err)
		fail(e.i18n.Tf(MsgTerminalScreenshotSendFailed, err))
		return
	}

	reg.CompleteTurnScreenshot(terminalID, turnID, true)
	slog.Info("terminal screenshot sent", "terminal_id", terminalID, "platform", target.Name(), "turn_id", turnID, "images", len(images))
}

func (e *Engine) HandleTerminalLocalInput(req TerminalLocalInputRequest) error {
	terminalID := strings.TrimSpace(req.TerminalID)
	if terminalID == "" {
		return errors.New("terminal id is required")
	}

	reg := e.TerminalRegistry()
	if _, ok := reg.Get(terminalID); !ok {
		return fmt.Errorf("terminal %q not found", terminalID)
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil
	}

	turnID, mode, attachedTarget, err := reg.StartLocalInputForTurnTarget(terminalID, req.Content)
	if err != nil {
		slog.Warn("terminal local input rejected", "terminal_id", terminalID, "turn_id", turnID, "mode", terminalReplyModeName(mode), "error", err)
		return err
	}
	if !attachedTarget.Attached {
		return nil
	}

	target, err := e.terminalPlatformFromKey(attachedTarget.SessionKey)
	if err != nil {
		return err
	}
	replyCtx := terminalDeliveryReplyContext(target, attachedTarget.ReplyCtx)
	return e.sendWithError(target, replyCtx, e.i18n.T(MsgTerminalLocalInput))
}

func (e *Engine) HandleTerminalOutput(req TerminalOutputRequest) error {
	reg := e.TerminalRegistry()
	if req.Error != "" {
		session, replyCtx, ok := reg.TerminalDeliveryTarget(req.TerminalID)
		if !ok {
			return nil
		}

		target, err := e.terminalPlatformFromKey(session.AttachedKey)
		if err != nil {
			return err
		}
		replyCtx = terminalDeliveryReplyContext(target, replyCtx)

		content := "[terminal error] " + req.Error
		if strings.TrimSpace(content) == "" {
			return nil
		}
		return e.sendWithError(target, replyCtx, content)
	}

	plain := plainTerminalOutput(req.Content)
	session, replyCtx, ok := reg.TerminalDeliveryTarget(req.TerminalID)
	var target Platform
	if ok {
		var err error
		target, err = e.terminalPlatformFromKey(session.AttachedKey)
		if err != nil {
			reg.IngestOutput(req.TerminalID, req.Content)
			return err
		}
		replyCtx = terminalDeliveryReplyContext(target, replyCtx)
		if _, ok := target.(ImageSender); ok && shouldStartAttachedOutputFallbackTurn(plain) {
			reg.StartAttachedOutputTurnIfIdle(req.TerminalID)
		}
	}
	reg.IngestOutput(req.TerminalID, req.Content)
	if !ok {
		return nil
	}

	turnID, turnMode, hasTurn := reg.ActiveTurn(req.TerminalID)
	promptReturn := hasTurn && isTerminalPromptReturnCompletionSignal(plain)
	messages := reg.collectOutput(req.TerminalID, req.Content, promptReturn)
	completion := isTerminalCompletionSignal(plain)
	if hasTurn {
		messages = reg.AppendTurnMessages(req.TerminalID, turnID, messages)
		finalMessages := terminalFinalMessages(messages)
		if !completion && promptReturn {
			completion = true
		}
		if !completion {
			if turnMode == terminalReplyModeScreenshotProgress {
				e.maybeSendTerminalProgressScreenshot(target, replyCtx, req.TerminalID, turnID, plain)
			}
			if generation, ok := reg.MarkTurnCompletionCandidate(req.TerminalID, turnID); ok {
				e.scheduleTerminalFinalForMode(target, replyCtx, req.TerminalID, turnID, turnMode, generation)
			}
			return nil
		}
		messages = finalMessages
	}

	if completion && hasTurn && (terminalReplyModeUsesScreenshots(turnMode) || turnMode == terminalReplyModeText) {
		if generation, ok := reg.MarkTurnCompletionCandidate(req.TerminalID, turnID); ok {
			e.scheduleTerminalFinalForMode(target, replyCtx, req.TerminalID, turnID, turnMode, generation)
		}
		return nil
	}

	for _, content := range messages {
		if strings.TrimSpace(content) == "" {
			continue
		}
		if err := e.sendWithError(target, replyCtx, content); err != nil {
			if completion && hasTurn {
				reg.CompleteActiveTurn(req.TerminalID, turnID)
			}
			return err
		}
	}
	if completion && hasTurn {
		reg.CompleteActiveTurn(req.TerminalID, turnID)
	}
	return nil
}

func parseTerminalProgressMetrics(content string) terminalProgressMetrics {
	var metrics terminalProgressMetrics
	if strings.Contains(content, "Compacting conversation") || strings.Contains(content, "压缩上下文") {
		metrics.compacting = true
	}
	if matches := terminalElapsedParen.FindStringSubmatch(content); len(matches) == 2 {
		metrics.elapsed = strings.TrimSpace(matches[1])
	} else if matches := terminalElapsedCooked.FindStringSubmatch(content); len(matches) == 2 {
		metrics.elapsed = strings.TrimSpace(matches[1])
	}
	if matches := terminalTokenUsage.FindStringSubmatch(content); len(matches) == 3 {
		metrics.tokens = strings.TrimSpace(matches[1]) + strings.TrimSpace(matches[2])
	}
	if matches := terminalThoughtDuration.FindStringSubmatch(content); len(matches) == 2 {
		metrics.thought = strings.TrimSpace(matches[1])
	}
	if strings.Contains(strings.ToLower(content), "context:n/a") || strings.Contains(strings.ToLower(content), "context: n/a") {
		metrics.contextNA = true
	} else if matches := terminalContextUsage.FindStringSubmatch(content); len(matches) == 3 {
		metrics.contextUsed = matches[1]
		metrics.contextRemaining = matches[2]
	}
	if metrics.compacting {
		matches := terminalPercent.FindAllStringSubmatch(content, -1)
		if len(matches) > 0 {
			metrics.progress = matches[len(matches)-1][1] + "%"
		}
	}
	return metrics
}

func mergeTerminalProgressMetrics(current, next terminalProgressMetrics) terminalProgressMetrics {
	if next.compacting {
		current.compacting = true
	}
	if next.progress != "" {
		current.progress = next.progress
	}
	if next.elapsed != "" {
		current.elapsed = next.elapsed
	}
	if next.tokens != "" {
		current.tokens = next.tokens
	}
	if next.thought != "" {
		current.thought = next.thought
	}
	if next.contextNA {
		current.contextNA = true
		current.contextUsed = ""
		current.contextRemaining = ""
	}
	if next.contextUsed != "" || next.contextRemaining != "" {
		current.contextNA = false
		current.contextUsed = next.contextUsed
		current.contextRemaining = next.contextRemaining
	}
	return current
}

func hasTerminalProgressMetrics(metrics terminalProgressMetrics) bool {
	return metrics.compacting || metrics.progress != "" || metrics.elapsed != "" || metrics.tokens != "" || metrics.thought != "" || metrics.contextNA || metrics.contextUsed != "" || metrics.contextRemaining != ""
}

func renderTerminalProgressMessage(content string, metrics terminalProgressMetrics) string {
	lines := []string{}
	if strings.TrimSpace(content) != "" {
		lines = append(lines, strings.TrimSpace(content), "")
	} else if metrics.compacting {
		lines = append(lines, "正在压缩上下文…")
	} else {
		lines = append(lines, "处理中…")
	}
	if metrics.progress != "" {
		lines = append(lines, "进度: "+metrics.progress)
	}
	if metrics.elapsed != "" {
		lines = append(lines, "运行: "+metrics.elapsed)
	}
	if metrics.tokens != "" {
		lines = append(lines, "Token: "+metrics.tokens)
	}
	if metrics.thought != "" {
		lines = append(lines, "思考: "+metrics.thought)
	}
	if metrics.contextNA {
		lines = append(lines, "Context: n/a")
	} else if metrics.contextUsed != "" || metrics.contextRemaining != "" {
		lines = append(lines, fmt.Sprintf("Context: 已用 %s%%，剩余 %s%%", metrics.contextUsed, metrics.contextRemaining))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func shouldStartAttachedOutputFallbackTurn(content string) bool {
	if looksLikeTerminalStartupOutput(content) {
		return false
	}
	completion := isTerminalCompletionSignal(content) || isTerminalPromptReturnCompletionSignal(content)
	for _, line := range keptTerminalOutputLines(content) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		block := []string{line}
		if isTerminalStatusBulletBlock(block) || isNamedTerminalToolBulletBlock(block) {
			continue
		}
		if strings.HasPrefix(line, "●") || completion {
			return true
		}
	}
	return false
}

func looksLikeTerminalStartupOutput(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "claude code v") || strings.Contains(lower, "welcome to claude code")
}

func terminalFinalMessages(messages []string) []string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		message = strings.TrimSpace(message)
		if message != "" {
			parts = append(parts, message)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{strings.Join(parts, "\n\n")}
}

func sanitizeTerminalOutputForPlatform(content string) string {
	var buffer terminalOutputBuffer
	messages := buffer.collect(content, true)
	return strings.TrimSpace(strings.Join(messages, "\n\n"))
}

func isTerminalProgressScreenshotSignal(content string) bool {
	plain := plainTerminalOutput(content)
	for _, line := range strings.Split(plain, "\n") {
		if !strings.Contains(line, "⎿") {
			continue
		}
		lower := strings.ToLower(line)
		for _, phrase := range []string{"successfully loaded skill", "exit code"} {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
		for _, token := range []string{"did", "received", "timeout", "error", "searches"} {
			if terminalProgressLineHasToken(lower, token) {
				return true
			}
		}
	}
	return false
}

func terminalProgressLineHasToken(line, token string) bool {
	if token == "" {
		return false
	}
	for start := 0; ; {
		idx := strings.Index(line[start:], token)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(token)
		if terminalProgressTokenStartBoundary(line, idx) && terminalProgressTokenEndBoundary(line, end) {
			return true
		}
		start = end
	}
}

func terminalProgressTokenStartBoundary(line string, idx int) bool {
	if idx <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(line[:idx])
	return terminalProgressTokenBoundary(r)
}

func terminalProgressTokenEndBoundary(line string, idx int) bool {
	if idx >= len(line) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(line[idx:])
	return terminalProgressTokenBoundary(r)
}

func terminalProgressTokenBoundary(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

func terminalProgressSignature(content string) string {
	plain := strings.TrimSpace(plainTerminalOutput(content))
	runes := []rune(plain)
	if len(runes) <= 240 {
		return plain
	}
	return string(runes[len(runes)-240:])
}

func (b *terminalOutputBuffer) collect(content string, flushTail bool) []string {
	plain := plainTerminalOutput(content)
	kept := keptTerminalOutputLines(plain)
	flushSignal := terminalOutputFlushSignal(plain)
	out := make([]string, 0, 2)
	emit := func(lines []string) {
		message := strings.TrimSpace(strings.Join(lines, "\n"))
		if message == "" || message == b.lastEmitted {
			return
		}
		b.lastEmitted = message
		out = append(out, message)
	}

	for _, line := range kept {
		if strings.HasPrefix(strings.TrimSpace(line), "●") {
			if isAnonymousTerminalToolResultBullet(line) {
				if len(b.pending) == 0 {
					b.pending = []string{line}
				}
				continue
			}
			if len(b.pending) > 0 {
				if shouldEmitTerminalBlockOnBoundary(b.pending) {
					emit(b.pending)
				}
				b.pending = nil
			}
			b.pending = []string{line}
			continue
		}
		if len(b.pending) > 0 {
			b.pending = append(b.pending, line)
			if isTerminalStatusBulletBlock(b.pending) {
				b.pending = nil
			}
		}
	}

	if len(b.pending) > 0 {
		switch {
		case isTerminalStatusBulletBlock(b.pending), isOrphanAnonymousTerminalToolResultBlock(b.pending):
			b.pending = nil
		case isIncompleteTerminalToolHeaderBlock(b.pending) && (flushTail || flushSignal):
			b.pending = nil
		case isCompleteTerminalToolBulletBlock(b.pending):
			emit(b.pending)
			b.pending = nil
		case isTerminalCompletionSignal(plain) || flushTail:
			emit(b.pending)
			b.pending = nil
		}
	}
	if len(out) == 0 {
		if compact := renderTerminalCompactingStatus(plain); compact != "" {
			out = append(out, compact)
		}
	}
	return out
}

func plainTerminalOutput(content string) string {
	content = terminalOSCSequence.ReplaceAllString(content, "")
	content = terminalCSISequence.ReplaceAllString(content, "")
	content = terminalESCSequence.ReplaceAllString(content, "")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	var b strings.Builder
	for _, r := range content {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r >= 0x20 && r != 0x7f:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func keptTerminalOutputLines(plain string) []string {
	statusFrame := looksLikeClaudeTUIStatusFrame(plain)
	lines := strings.Split(plain, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if isTransientTerminalUILine(line, statusFrame) {
			continue
		}
		kept = append(kept, line)
	}
	return kept
}

func terminalOutputFlushSignal(content string) bool {
	return strings.Contains(content, "Context:") || strings.Contains(content, "❯") || strings.Contains(content, "›") || isTerminalCompletionSignal(content)
}

func isTerminalCompletionSignal(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "cooked for") || strings.Contains(lower, "sautéed for") || strings.Contains(lower, "sauteed for") || strings.Contains(lower, "crunched fo") || strings.Contains(lower, "cogitated for") || strings.Contains(lower, "brewed for") || strings.Contains(lower, "baked for") || strings.Contains(lower, "churned for") || strings.Contains(lower, "worked for")
}

func isTerminalPromptReturnCompletionSignal(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		switch strings.TrimSpace(line) {
		case "❯", "›":
			return true
		}
	}
	return false
}

func renderTerminalCompactingStatus(content string) string {
	if !strings.Contains(content, "Compacting conversation") && !strings.Contains(content, "压缩上下文") {
		return ""
	}
	metrics := parseTerminalProgressMetrics(content)
	metrics.compacting = true
	return renderTerminalProgressMessage("", metrics)
}

func renderTerminalContextStatus(content string) string {
	matches := terminalContextUsage.FindStringSubmatch(content)
	if len(matches) != 3 {
		return ""
	}
	lines := []string{fmt.Sprintf("Context: 已用 %s%%，剩余 %s%%", matches[1], matches[2])}
	if auto := terminalAutoCompact.FindStringSubmatch(content); len(auto) == 2 {
		lines = append(lines, "Auto-compact: "+auto[1]+"%")
	}
	return strings.Join(lines, "\n")
}

func terminalBulletBlock(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if i > 0 && strings.HasPrefix(strings.TrimSpace(line), "●") {
			break
		}
		out = append(out, line)
	}
	return out
}

func shouldEmitTerminalBlockOnBoundary(lines []string) bool {
	if shouldDropTerminalBlock(lines) {
		return false
	}
	return isCompleteTerminalToolBulletBlock(lines) || isTerminalImmediateBulletBlock(lines)
}

func shouldDropTerminalBlock(lines []string) bool {
	return isTerminalStatusBulletBlock(lines) || isOrphanAnonymousTerminalToolResultBlock(lines) || isIncompleteTerminalToolHeaderBlock(lines)
}

func isTerminalStatusBulletBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	if isNamedTerminalToolBulletBlock(lines) && !containsTerminalStatusNoise(strings.ToLower(strings.Join(lines, "\n"))) {
		return false
	}
	first := strings.TrimSpace(lines[0])
	lower := strings.ToLower(first)
	bullet := terminalBulletText(first)
	if first == "●" || isTerminalProgressNumber(strings.TrimPrefix(bullet, "*")) || strings.Trim(bullet, "*.· …0123456789") == "" {
		return true
	}
	if strings.HasPrefix(lower, "● error:") || strings.HasPrefix(lower, "● title:") {
		return false
	}
	if containsTerminalStatusNoise(lower) {
		return true
	}
	return strings.Contains(lower, "·") && strings.Contains(lower, "timeout")
}

func isTerminalToolBulletBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	if isNamedTerminalToolBulletBlock(lines) {
		return true
	}
	for _, line := range lines[1:] {
		if strings.Contains(line, "⎿") {
			return true
		}
	}
	return false
}

func isNamedTerminalToolBulletBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	first := terminalBulletText(lines[0])
	return strings.HasPrefix(first, "Skill(") || strings.HasPrefix(first, "Bash(") || strings.HasPrefix(first, "Read(") || strings.HasPrefix(first, "Edit(") || strings.HasPrefix(first, "Write(") || strings.HasPrefix(first, "TodoWrite(") || strings.HasPrefix(first, "Web Search(") || strings.HasPrefix(first, "Fetch(")
}

func isCompleteTerminalToolBulletBlock(lines []string) bool {
	return isNamedTerminalToolBulletBlock(lines) && terminalBlockHasToolResult(lines)
}

func isIncompleteTerminalToolHeaderBlock(lines []string) bool {
	return isNamedTerminalToolBulletBlock(lines) && !terminalBlockHasToolResult(lines)
}

func terminalBlockHasToolResult(lines []string) bool {
	for _, line := range lines[1:] {
		if strings.Contains(line, "⎿") {
			return true
		}
	}
	return false
}

func isTerminalImmediateBulletBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(lines[0]))
	return strings.HasPrefix(lower, "● error:") || strings.HasPrefix(lower, "● title:")
}

func isAnonymousTerminalToolResultBullet(line string) bool {
	return strings.TrimSpace(line) == "●"
}

func isOrphanAnonymousTerminalToolResultBlock(lines []string) bool {
	return len(lines) > 0 && isAnonymousTerminalToolResultBullet(lines[0])
}

func terminalBulletText(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "●"))
}

func containsTerminalStatusNoise(lower string) bool {
	return strings.Contains(lower, "calculating") || strings.Contains(lower, "ionizing") || strings.Contains(lower, "waiting") || strings.Contains(lower, "julienning") || strings.Contains(lower, "infusing") || strings.Contains(lower, "bunning") || strings.Contains(lower, "roosting") || strings.Contains(lower, "almost done") || strings.Contains(lower, "max effort") || strings.Contains(lower, "sautéed for") || strings.Contains(lower, "sauteed for") || strings.Contains(lower, "cogitated for") || strings.Contains(lower, "brewed for") || strings.Contains(lower, "baked for") || strings.Contains(lower, "churned for") || strings.Contains(lower, "worked for")
}

func looksLikeClaudeTUIStatusFrame(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(content, "Prestidigitating") ||
		strings.Contains(content, "Manifesting") ||
		strings.Contains(content, "Churning") ||
		strings.Contains(content, "Julienning") ||
		strings.Contains(content, "Ionizing") ||
		strings.Contains(content, "Bunning") ||
		strings.Contains(content, "Compacting conversation") ||
		strings.Contains(content, "▰") ||
		strings.Contains(content, "▱") ||
		strings.Contains(lower, "still thinking") ||
		strings.Contains(lower, "running stop hook") ||
		strings.Contains(lower, "cooked for") ||
		strings.Contains(lower, "cogitated for") ||
		strings.Contains(lower, "brewed for") ||
		strings.Contains(lower, "baked for") ||
		strings.Contains(lower, "crunched fo") ||
		strings.Contains(lower, "retrying in") ||
		strings.Contains(content, "Context:") ||
		strings.Contains(content, "✽") ||
		strings.Contains(content, "✻") ||
		strings.Contains(content, "✶") ||
		strings.Contains(content, "✢")
}

func isTransientTerminalUILine(line string, statusFrame bool) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(trimmed, "❯") || trimmed == "›" {
		return true
	}
	if strings.Contains(trimmed, "Prestidigitating") || strings.Contains(trimmed, "Manifesting") || strings.Contains(trimmed, "Churning") || strings.Contains(trimmed, "Julienning") || strings.Contains(trimmed, "Ionizing") || strings.Contains(trimmed, "Infusing") || strings.Contains(trimmed, "Bunning") || strings.Contains(lower, "still thinking") || strings.Contains(lower, "running stop hook") || strings.Contains(lower, "cooked for") || strings.Contains(lower, "sautéed for") || strings.Contains(lower, "sauteed for") || strings.Contains(lower, "cogitated for") || strings.Contains(lower, "brewed for") || strings.Contains(lower, "baked for") || strings.Contains(lower, "crunched fo") || strings.Contains(lower, "retrying in") || containsTerminalStatusNoise(lower) {
		return true
	}
	if strings.HasPrefix(trimmed, "Context:") || strings.Contains(lower, "% used") || strings.Contains(lower, "%used") || strings.Contains(trimmed, "tokens ·") || strings.Contains(trimmed, "tokens)") {
		return true
	}
	if strings.Contains(trimmed, "Tip:") && (strings.HasPrefix(trimmed, "⎿") || strings.Contains(trimmed, "Use git worktrees")) {
		return true
	}
	if strings.Trim(trimmed, "─━═ ") == "" {
		return true
	}
	if statusFrame && strings.ContainsAny(trimmed, "▰▱") {
		return true
	}
	if statusFrame && (isTerminalProgressNumber(trimmed) || isStatusWordFragment(trimmed)) {
		return true
	}
	return false
}

func isTerminalProgressNumber(line string) bool {
	for _, r := range line {
		if r < '0' || r > '9' {
			return false
		}
	}
	return line != ""
}

func isStatusWordFragment(line string) bool {
	hasLetter := false
	for _, r := range strings.ToLower(line) {
		switch {
		case strings.ContainsRune("✽✻✶✢·* …()↓0123456789", r):
			continue
		case strings.ContainsRune("prestidigatnmanifestgchurncookdfor", r):
			hasLetter = true
		default:
			return false
		}
	}
	return hasLetter || strings.Trim(line, "✽✻✶✢·* …()↓0123456789") == ""
}
