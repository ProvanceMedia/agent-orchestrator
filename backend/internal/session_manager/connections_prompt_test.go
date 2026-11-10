package sessionmanager

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestBuildSystemPromptNamesConnectedServicesInChatOnly(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	m := New(Deps{
		Runtime:   &fakeRuntime{},
		Agents:    singleAgent{agent: &recordingAgent{}},
		Workspace: &fakeWorkspace{},
		Store:     st,
		Messenger: &fakeMessenger{},
		Lifecycle: &fakeLCM{store: st},
		DataDir:   t.TempDir(),
		LookPath:  func(string) (string, error) { return "/bin/true", nil },
	})
	const block = "## Connected services\n\n- GitHub -> github_api"

	sp, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", true)
	if err != nil || strings.Contains(sp, "## Connected services") {
		t.Fatalf("unwired: err %v, block present %v", err, strings.Contains(sp, "## Connected services"))
	}

	current := ""
	m.SetConnectionsPrompt(func(_ context.Context, project string) string {
		if project != "mer" {
			return ""
		}
		return current
	})
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		current = ""
		if sp, _ = m.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", true); strings.Contains(sp, "## Connected services") {
			t.Fatalf("%s: block present with nothing connected", kind)
		}
		current = block
		if sp, _ = m.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", true); !strings.Contains(sp, block) {
			t.Fatalf("%s chat: block missing", kind)
		}
		// A terminal session has no `ao mcp` server, so it has no connection tools.
		if sp, _ = m.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", false); strings.Contains(sp, "## Connected services") {
			t.Fatalf("%s terminal: block present", kind)
		}
	}
}
