package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func agentTurnConversation(enabled bool) *conversation {
	return &conversation{
		log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		providerScopeID:   "test-scope",
		sessionID:         "session-1",
		events:            make(chan ports.ChatEvent, 64),
		messages:          make(map[string]string),
		thoughts:          make(map[string]string),
		nestedMessages:    make(map[string]nestedMessageState),
		tools:             make(map[string]*toolState),
		agentStartedTurns: enabled,
	}
}

func drainEvents(conv *conversation) []ports.ChatEvent {
	var out []ports.ChatEvent
	for {
		select {
		case event := <-conv.events:
			out = append(out, event)
		default:
			return out
		}
	}
}

func sdkMessage(t *testing.T, conv *conversation, message map[string]any) {
	t.Helper()
	params, err := json.Marshal(map[string]any{"sessionId": "session-1", "message": message})
	if err != nil {
		t.Fatal(err)
	}
	if !conv.handleNotification(claudeSDKMessageMethod, params) {
		t.Fatalf("%s was not handled", claudeSDKMessageMethod)
	}
}

func kinds(events []ports.ChatEvent) string {
	var out []string
	for _, event := range events {
		name := string(event.Kind)
		if event.Kind == ports.ChatEventControllerState {
			name += ":" + string(event.ControllerState)
		}
		out = append(out, name)
	}
	return strings.Join(out, " ")
}

// Output that arrives with no prompt in flight, such as the cycle after a
// background task finishes, becomes a turn of its own. The SDK's result ends
// it, its message settles, and AO is ready again.
func TestAgentStartedOutputOpensATurnThatTheSDKResultEnds(t *testing.T) {
	conv := agentTurnConversation(true)
	sdkMessage(t, conv, map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running"})
	for _, text := range []string{"Both PRs ", "are ready."} {
		if err := conv.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", text)); err != nil {
			t.Fatal(err)
		}
	}
	started := drainEvents(conv)
	if kinds(started) != "turn.started controller.state:busy message.delta message.delta" {
		t.Fatalf("events = %s", kinds(started))
	}
	turnID := started[0].ProviderTurnID
	if !isAgentTurnID(turnID) || started[2].ProviderTurnID != turnID || started[3].ProviderTurnID != turnID {
		t.Fatalf("turn ids = %q %q %q, want one agent turn", turnID, started[2].ProviderTurnID, started[3].ProviderTurnID)
	}

	sdkMessage(t, conv, map[string]any{"type": "result", "subtype": "success", "origin": map[string]any{"kind": "task-notification"}})
	ended := drainEvents(conv)
	if kinds(ended) != "message.completed turn.completed controller.state:ready" {
		t.Fatalf("events = %s", kinds(ended))
	}
	if ended[0].Text != "Both PRs are ready." || ended[1].ProviderTurnID != turnID || ended[1].TurnState != domain.TurnStateCompleted {
		t.Fatalf("end events = %+v", ended)
	}
	if conv.activeTurn != "" || conv.agentTurn != "" {
		t.Fatalf("active turn = %q, agent turn = %q, want none", conv.activeTurn, conv.agentTurn)
	}

	// The trailing idle of that cycle changes nothing.
	sdkMessage(t, conv, map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle"})
	if events := drainEvents(conv); len(events) != 0 {
		t.Fatalf("idle with no agent turn emitted %s", kinds(events))
	}
}

// The SDK's idle ends an agent-started turn whose result did not end it, and
// the next cycle gets a new turn.
func TestAgentStartedTurnEndsAtIdleAndTheNextCycleIsNew(t *testing.T) {
	conv := agentTurnConversation(true)
	running := map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running"}
	sdkMessage(t, conv, running)
	if events := drainEvents(conv); len(events) != 0 {
		t.Fatalf("running state emitted %s", kinds(events))
	}
	_ = conv.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", "first"))
	first := drainEvents(conv)[0].ProviderTurnID
	sdkMessage(t, conv, map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle"})
	if got := kinds(drainEvents(conv)); got != "message.completed turn.completed controller.state:ready" {
		t.Fatalf("idle events = %s", got)
	}
	// Output while the SDK is idle is not part of a cycle: no turn opens.
	_ = conv.SessionUpdate(context.Background(), replayAgentChunk("session-1", "notice", "a notice"))
	if events := drainEvents(conv); kinds(events) != "message.delta" || events[0].ProviderTurnID != "" {
		t.Fatalf("idle-state output events = %s, want one turnless delta", kinds(events))
	}
	sdkMessage(t, conv, running)
	_ = conv.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m2", "second"))
	second := drainEvents(conv)[0].ProviderTurnID
	if !isAgentTurnID(second) || second == first {
		t.Fatalf("second turn = %q, want a new agent turn after %q", second, first)
	}
}

// Without a cycle-end signal, or while a prompt owns the output, output stays
// as before: no turn is opened that nothing would end.
func TestAgentStartedTurnsNeedTheSignalAndNoPrompt(t *testing.T) {
	disabled := agentTurnConversation(false)
	_ = disabled.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", "text"))
	if events := drainEvents(disabled); kinds(events) != "message.delta" || events[0].ProviderTurnID != "" {
		t.Fatalf("disabled events = %s %+v, want one turnless delta", kinds(events), events)
	}

	unknown := agentTurnConversation(true)
	_ = unknown.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", "text"))
	if events := drainEvents(unknown); kinds(events) != "message.delta" || unknown.agentTurn != "" {
		t.Fatalf("no-state events = %s, want no agent turn before the SDK reports running", kinds(events))
	}

	prepared := agentTurnConversation(true)
	prepared.sdkState = "running"
	prepared.prepared = &preparedTurn{id: "user-turn"}
	_ = prepared.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", "text"))
	if events := drainEvents(prepared); kinds(events) != "message.delta" || prepared.agentTurn != "" {
		t.Fatalf("prepared-prompt events = %s, want no agent turn", kinds(events))
	}

	prompt := agentTurnConversation(true)
	prompt.sdkState = "running"
	prompt.activeTurn = "user-turn"
	_ = prompt.SessionUpdate(context.Background(), replayAgentChunk("session-1", "m1", "text"))
	sdkMessage(t, prompt, map[string]any{"type": "result", "subtype": "success"})
	events := drainEvents(prompt)
	if kinds(events) != "message.delta" || events[0].ProviderTurnID != "user-turn" || prompt.activeTurn != "user-turn" {
		t.Fatalf("prompt events = %s, active %q: a result must not end a prompt's turn", kinds(events), prompt.activeTurn)
	}
}

// A daemon that reattaches to a live host finds an agent-started turn still
// running. Nothing on that connection would end it, so it is completed.
func TestLiveReconnectCompletesAnOpenAgentStartedTurn(t *testing.T) {
	conv := agentTurnConversation(false)
	conv.proc = &process{reconnected: true, gate: newGatedReader(strings.NewReader(""), false)}
	conv.liveState = &persistenthost.ACPState{}
	turnID := agentTurnPrefix + "left-open"
	if err := conv.ActivateLiveReconnect(context.Background(), turnID); err != nil {
		t.Fatalf("ActivateLiveReconnect: %v", err)
	}
	completed := <-conv.events
	ready := <-conv.events
	if completed.Kind != ports.ChatEventTurnCompleted || completed.ProviderTurnID != turnID || completed.TurnState != domain.TurnStateCompleted {
		t.Fatalf("first event = %+v, want the agent turn completed", completed)
	}
	if ready.Kind != ports.ChatEventControllerState || ready.ControllerState != ports.ChatControllerReady {
		t.Fatalf("second event = %+v, want ready", ready)
	}
	if conv.activeTurn != "" {
		t.Fatalf("active turn = %q, want none", conv.activeTurn)
	}
}
