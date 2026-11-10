package acp

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// agentTurnPrefix marks a provider turn id that the driver made for an
// agent-started cycle. A reconnecting daemon uses it to tell such a turn from
// a prompt, because no prompt result will ever end it.
const agentTurnPrefix = "agent-turn-"

// claudeSDKMessageMethod carries raw Claude Agent SDK messages that
// claude-agent-acp relays when a session asks for them with
// _meta.claudeCode.emitRawSDKMessages.
const claudeSDKMessageMethod = "_claude/sdkMessage"

func isAgentTurnID(id string) bool { return strings.HasPrefix(id, agentTurnPrefix) }

// beginAgentStartedTurn returns the turn that owns output arriving with no
// prompt in flight. When the binding reports the end of agent-started cycles
// and the SDK reports that it is running a cycle, it opens a new turn for that
// output. Otherwise it returns "" and the output stays turnless as before, so
// no turn opens that no idle would end. A prepared prompt, a compaction, a
// settling turn or a history replay owns the output instead.
func (c *conversation) beginAgentStartedTurn() string {
	if c.historyReplayActive() {
		return ""
	}
	c.mu.Lock()
	if c.activeTurn != "" {
		id := c.activeTurn
		c.mu.Unlock()
		return id
	}
	if !c.agentStartedTurns || c.sdkState != "running" || c.closed || c.detaching || c.prepared != nil || c.settlingTurn != "" || c.compactingTurnID != "" {
		c.mu.Unlock()
		return ""
	}
	id := agentTurnPrefix + uuid.NewString()
	c.activeTurn = id
	c.agentTurn = id
	c.messages = make(map[string]string)
	c.thoughts = make(map[string]string)
	c.nestedMessages = make(map[string]nestedMessageState)
	c.tools = make(map[string]*toolState)
	c.turnDiffs = nil
	c.turnDiffTurnID = ""
	c.providerFailure = nil
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id})
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerBusy})
	return id
}

// endAgentStartedTurn settles and completes the open agent-started turn. It
// does nothing when no such turn is open. An accepted interrupt makes the turn
// interrupted rather than completed.
func (c *conversation) endAgentStartedTurn() {
	c.mu.Lock()
	id := c.agentTurn
	if id == "" || c.activeTurn != id || c.settlingTurn == id {
		c.mu.Unlock()
		return
	}
	c.settlingTurn = id
	interrupt := c.interrupt
	c.mu.Unlock()
	c.settleOpenItems(id)
	state := domain.TurnStateCompleted
	if interrupt != nil && interrupt.turnID == id {
		<-interrupt.done
		if interrupt.err == nil {
			state = domain.TurnStateInterrupted
		}
	}
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, TurnState: state})
	c.mu.Lock()
	if c.activeTurn == id {
		c.activeTurn = ""
		c.settlingTurn = ""
		c.turnCancel = nil
		c.providerFailure = nil
		if c.interrupt == interrupt {
			c.interrupt = nil
		}
	}
	c.agentTurn = ""
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
}

// handleClaudeSDKMessage ends an open agent-started turn at the SDK's own end
// of a cycle: a result message, or the session going idle. Both arrive after
// the cycle's output, because notifications are handled in wire order.
func (c *conversation) handleClaudeSDKMessage(params json.RawMessage) {
	var notification struct {
		Message struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			State   string `json:"state"`
		} `json:"message"`
	}
	if json.Unmarshal(params, &notification) != nil {
		return
	}
	message := notification.Message
	if message.Type == "system" && message.Subtype == "session_state_changed" && message.State != "" {
		c.mu.Lock()
		c.sdkState = message.State
		c.mu.Unlock()
	}
	if message.Type == "result" || (message.Type == "system" && message.Subtype == "session_state_changed" && message.State == "idle") {
		c.endAgentStartedTurn()
	}
}
