// Package keychain keeps connection secrets in the macOS login keychain.
//
// The daemon writes each item through the Security framework, so the item's
// access list trusts only the ao binary that made it. Another program, such as
// `security find-generic-password` run from an agent's shell, gets a macOS
// prompt instead of the value. A new build of ao is another program to the
// keychain, so the first read after an update can also show a prompt; the user
// clicks Always Allow one time.
package keychain

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

// Service is the keychain service name of every connection item.
const Service = "Swarm 3.0 Connections"

// opTimeout bounds one keychain operation. A read can wait for the user to
// answer a prompt, so the bound is long; a timeout gives an error, not a hang.
const opTimeout = 2 * time.Minute

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Store is the keychain secret store for connections.
type Store struct {
	service string
}

// New returns the store for the production service name.
func New() *Store { return &Store{service: Service} }

// NewForService returns a store for another service name, for a manual test.
func NewForService(service string) *Store { return &Store{service: service} }

var _ connections.SecretStore = (*Store)(nil)

// Available reports whether this build can use the keychain.
func (s *Store) Available() bool { return available }

// Get reads the field values of one connection.
func (s *Store) Get(ctx context.Context, id string) (map[string]string, error) {
	if err := checkAccount(id); err != nil {
		return nil, err
	}
	raw, err := run(ctx, func() ([]byte, error) { return get(s.service, id) })
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("the keychain item for %s is not valid: %w", id, err)
	}
	return values, nil
}

// Put writes the field values of one connection, and replaces any old item.
func (s *Store) Put(ctx context.Context, id string, values map[string]string) error {
	if err := checkAccount(id); err != nil {
		return err
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = run(ctx, func() ([]byte, error) { return nil, put(s.service, id, raw) })
	return err
}

// Delete removes the item of one connection.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := checkAccount(id); err != nil {
		return err
	}
	_, err := run(ctx, func() ([]byte, error) { return nil, remove(s.service, id) })
	return err
}

func checkAccount(id string) error {
	if !accountName.MatchString(id) {
		return fmt.Errorf("invalid connection id %q", id)
	}
	return nil
}

// run does one keychain operation off the caller's goroutine, so that a
// prompt with no answer cannot block the caller past its deadline.
func run(ctx context.Context, op func() ([]byte, error)) ([]byte, error) {
	if !available {
		return nil, connections.ErrStoreUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := op()
		done <- result{raw, err}
	}()
	select {
	case r := <-done:
		return r.raw, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("keychain: %w", ctx.Err())
	}
}
