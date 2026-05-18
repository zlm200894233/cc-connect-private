package core

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

type CLIBridgeAttachRequest struct {
	Project    string `json:"project"`
	SessionKey string `json:"session_key"`
}

type CLIBridgeInputRequest struct {
	Project    string `json:"project"`
	SessionKey string `json:"session_key"`
	Message    string `json:"message"`
}

type CLIBridgeFrame struct {
	Type           string `json:"type"`
	Project        string `json:"project,omitempty"`
	SessionKey     string `json:"session_key,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	Content        string `json:"content,omitempty"`
	Error          string `json:"error,omitempty"`
}

var cliSinkCounter atomic.Uint64

func (e *Engine) AttachCLI(sessionKey string) (<-chan CLIBridgeFrame, func(), error) {
	resolvedKey, state, agentSession, err := e.resolveCLILiveState(sessionKey)
	if err != nil {
		return nil, nil, err
	}

	ch := make(chan CLIBridgeFrame, 32)
	sinkID := fmt.Sprintf("cli-%d", cliSinkCounter.Add(1))

	state.mu.Lock()
	if state.cliSinks == nil {
		state.cliSinks = make(map[string]chan CLIBridgeFrame)
	}
	state.cliSinks[sinkID] = ch
	state.mu.Unlock()

	ch <- CLIBridgeFrame{
		Type:           "ready",
		Project:        e.name,
		SessionKey:     resolvedKey,
		AgentSessionID: agentSession.CurrentSessionID(),
	}

	var once sync.Once
	detach := func() {
		once.Do(func() {
			shouldClose := false
			state.mu.Lock()
			if state.cliSinks != nil {
				if _, ok := state.cliSinks[sinkID]; ok {
					delete(state.cliSinks, sinkID)
					shouldClose = true
				}
			}
			state.mu.Unlock()
			if shouldClose {
				close(ch)
			}
		})
	}

	return ch, detach, nil
}

func (e *Engine) SubmitCLIMessage(sessionKey, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("message is required")
	}

	resolvedKey, state, _, err := e.resolveCLILiveState(sessionKey)
	if err != nil {
		return err
	}

	state.mu.Lock()
	p := state.platform
	replyCtx := state.replyCtx
	state.mu.Unlock()
	if p == nil {
		return fmt.Errorf("session %q has no platform", resolvedKey)
	}

	if !e.emitCLIBridgeFrameToExpectedState(resolvedKey, state, CLIBridgeFrame{Type: "input", Content: message}) {
		return fmt.Errorf("session %q is no longer live", resolvedKey)
	}

	invalid := false
	e.handleMessage(p, &Message{
		Platform:                        p.Name(),
		SessionKey:                      resolvedKey,
		ReplyCtx:                        replyCtx,
		UserID:                          "local-cli",
		UserName:                        "Local CLI",
		Content:                         message,
		existingInteractiveKey:          resolvedKey,
		existingInteractiveState:        state,
		expectedInteractiveStateInvalid: &invalid,
		expectedInteractiveStateStarted: make(chan bool, 1),
	})
	if invalid {
		return fmt.Errorf("session %q is no longer live", resolvedKey)
	}
	return nil
}

func (e *Engine) resolveCLILiveState(sessionKey string) (string, *interactiveState, AgentSession, error) {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()

	if sessionKey != "" {
		state := e.interactiveStates[sessionKey]
		if state == nil || state.agentSession == nil || !state.agentSession.Alive() {
			return "", nil, nil, fmt.Errorf("no live session for %q", sessionKey)
		}
		return sessionKey, state, state.agentSession, nil
	}

	var resolvedKey string
	var resolvedState *interactiveState
	var resolvedSession AgentSession
	for key, state := range e.interactiveStates {
		if state == nil || state.agentSession == nil || !state.agentSession.Alive() {
			continue
		}
		if resolvedState != nil {
			return "", nil, nil, errors.New("session key is required when multiple live sessions exist")
		}
		resolvedKey = key
		resolvedState = state
		resolvedSession = state.agentSession
	}
	if resolvedState == nil {
		return "", nil, nil, errors.New("no live session available")
	}
	return resolvedKey, resolvedState, resolvedSession, nil
}

func (e *Engine) getExpectedLiveInteractiveState(interactiveKey string, expectedState *interactiveState) (*interactiveState, AgentSession, bool) {
	if expectedState == nil {
		return nil, nil, false
	}

	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	if state != expectedState || state == nil {
		e.interactiveMu.Unlock()
		return nil, nil, false
	}
	agentSession := state.agentSession
	e.interactiveMu.Unlock()

	if agentSession == nil || !agentSession.Alive() {
		return nil, nil, false
	}
	return state, agentSession, true
}

func (e *Engine) emitCLIBridgeFrame(sessionKey string, frame CLIBridgeFrame) bool {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return false
	}
	return e.emitCLIBridgeFrameToState(sessionKey, state, frame)
}

func (e *Engine) emitCLIBridgeFrameToExpectedState(sessionKey string, expectedState *interactiveState, frame CLIBridgeFrame) bool {
	if expectedState == nil {
		return false
	}

	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state != expectedState {
		return false
	}
	return e.emitCLIBridgeFrameToState(sessionKey, expectedState, frame)
}

func (e *Engine) emitCLIBridgeFrameToState(sessionKey string, state *interactiveState, frame CLIBridgeFrame) bool {
	if frame.Project == "" {
		frame.Project = e.name
	}
	if frame.SessionKey == "" {
		frame.SessionKey = sessionKey
	}

	state.mu.Lock()
	sinks := make([]chan CLIBridgeFrame, 0, len(state.cliSinks))
	for _, sink := range state.cliSinks {
		sinks = append(sinks, sink)
	}
	state.mu.Unlock()
	if len(sinks) == 0 {
		return true
	}

	for _, sink := range sinks {
		select {
		case sink <- frame:
		default:
		}
	}
	return true
}
