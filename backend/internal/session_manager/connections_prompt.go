package sessionmanager

import "context"

// SetConnectionsPrompt supplies the block that names the services connected
// in Settings > Connections. Each one is a tool on the chat session's `ao mcp`
// server, so only chat sessions get the block. A session gets the block when
// it starts or restores; a session already running keeps what it started with.
func (m *Manager) SetConnectionsPrompt(prompt func(context.Context) string) {
	m.connectionsMu.Lock()
	defer m.connectionsMu.Unlock()
	m.connectionsPrompt = prompt
}

func (m *Manager) connectionsBlock(ctx context.Context) string {
	m.connectionsMu.Lock()
	prompt := m.connectionsPrompt
	m.connectionsMu.Unlock()
	if prompt == nil {
		return ""
	}
	return prompt(ctx)
}
