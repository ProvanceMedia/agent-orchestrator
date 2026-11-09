//go:build !darwin || !cgo

package keychain

import "github.com/aoagents/agent-orchestrator/backend/internal/service/connections"

// available is false: this build has no keychain, so connections are off.
const available = false

func get(string, string) ([]byte, error) { return nil, connections.ErrStoreUnavailable }
func put(string, string, []byte) error   { return connections.ErrStoreUnavailable }
func remove(string, string) error        { return connections.ErrStoreUnavailable }
