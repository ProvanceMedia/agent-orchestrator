package keychain

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

// TestLiveRoundTrip writes to the real login keychain, so it runs only when
// AO_KEYCHAIN_LIVE_TEST=1. It uses its own service name and removes its item.
func TestLiveRoundTrip(t *testing.T) {
	if os.Getenv("AO_KEYCHAIN_LIVE_TEST") != "1" {
		t.Skip("set AO_KEYCHAIN_LIVE_TEST=1 to write to the login keychain")
	}
	if !available {
		t.Skip("this build has no keychain")
	}
	ctx := context.Background()
	store := NewForService(Service + " (test)")
	t.Cleanup(func() { _ = store.Delete(ctx, "roundtrip") })

	if _, err := store.Get(ctx, "roundtrip"); !errors.Is(err, connections.ErrSecretNotFound) {
		t.Fatalf("Get before Put = %v, want ErrSecretNotFound", err)
	}
	if err := store.Put(ctx, "roundtrip", map[string]string{"key": "first"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(ctx, "roundtrip", map[string]string{"key": "second", "host": "https://eu.example"}); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	got, err := store.Get(ctx, "roundtrip")
	if err != nil || got["key"] != "second" || got["host"] != "https://eu.example" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if err := store.Delete(ctx, "roundtrip"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "roundtrip"); !errors.Is(err, connections.ErrSecretNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrSecretNotFound", err)
	}
}

func TestRejectsBadAccount(t *testing.T) {
	store := New()
	for _, id := range []string{"", "../x", "A", "a b"} {
		if err := store.Put(context.Background(), id, nil); err == nil {
			t.Errorf("Put(%q) = nil, want an error", id)
		}
	}
}
