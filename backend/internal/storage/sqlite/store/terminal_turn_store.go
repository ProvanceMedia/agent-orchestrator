package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordTerminalTurnRequest stores what the user asked a Terminal UI agent. A
// request that arrives while the session's newest turn is still open on the
// same thread was queued behind it, so it joins that turn instead of starting
// another.
func (s *Store) RecordTerminalTurnRequest(ctx context.Context, turn domain.TerminalTurn) error {
	text := strings.TrimSpace(turn.UserRequest)
	if text == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "record terminal turn request", func(q *gen.Queries) error {
		joined, err := q.AppendTerminalTurnRequest(ctx, gen.AppendTerminalTurnRequestParams{
			UserRequest: text, SessionID: string(turn.SessionID),
			Harness: string(turn.Harness), NativeSessionID: turn.NativeSessionID,
		})
		if err != nil || joined > 0 {
			return err
		}
		if err := q.InsertTerminalTurn(ctx, gen.InsertTerminalTurnParams{
			SessionID: string(turn.SessionID), Harness: string(turn.Harness),
			NativeSessionID: turn.NativeSessionID, UserRequest: text, RequestedAt: turn.RequestedAt,
		}); err != nil {
			return err
		}
		return q.TrimTerminalTurns(ctx, gen.TrimTerminalTurnsParams{
			SessionID: string(turn.SessionID), Keep: domain.TerminalTurnRetention,
		})
	})
}

// RecordTerminalTurnResponse closes the session's newest turn with the agent's
// final message. A stop with no open turn has nothing to answer and is ignored.
func (s *Store) RecordTerminalTurnResponse(ctx context.Context, id domain.SessionID, response string, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.CloseTerminalTurn(ctx, gen.CloseTerminalTurnParams{
		AgentResponse: strings.TrimSpace(response), RespondedAt: sql.NullTime{Time: at, Valid: true}, SessionID: string(id),
	}); err != nil {
		return fmt.Errorf("record terminal turn response: %w", err)
	}
	return nil
}

// RecordTerminalTurnMarker opens a turn that no user request started: AO's
// own message opened it, or the prompt hook carried no text. It closes any turn
// still open first. The marker's request stays empty, so history leaves it out,
// and the coming stop closes the marker instead of reading as a turn the agent
// started itself.
func (s *Store) RecordTerminalTurnMarker(ctx context.Context, turn domain.TerminalTurn) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "record terminal turn marker", func(q *gen.Queries) error {
		if err := q.CloseTerminalTurn(ctx, gen.CloseTerminalTurnParams{
			AgentResponse: "", RespondedAt: sql.NullTime{Time: turn.RequestedAt, Valid: true}, SessionID: string(turn.SessionID),
		}); err != nil {
			return err
		}
		if err := q.InsertTerminalTurn(ctx, gen.InsertTerminalTurnParams{
			SessionID: string(turn.SessionID), Harness: string(turn.Harness),
			NativeSessionID: turn.NativeSessionID, UserRequest: "", RequestedAt: turn.RequestedAt,
		}); err != nil {
			return err
		}
		return q.TrimTerminalTurns(ctx, gen.TrimTerminalTurnsParams{
			SessionID: string(turn.SessionID), Keep: domain.TerminalTurnRetention,
		})
	})
}

// RecordTerminalTurnStop closes the session's newest open turn with the
// agent's final message. A stop with no open turn ends a turn the agent
// started itself, for example after a background task finished, because AO
// opens a turn or a marker for every prompt. That turn is stored closed, with
// domain.AgentStartedTurnRequest as its request. A stop with no text, or one
// that repeats the newest turn's answer, adds nothing.
func (s *Store) RecordTerminalTurnStop(ctx context.Context, turn domain.TerminalTurn, response string, at time.Time) error {
	response = strings.TrimSpace(response)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTxDB(ctx, "record terminal turn stop", func(q *gen.Queries, db gen.DBTX) error {
		closeTurn := func() error {
			return q.CloseTerminalTurn(ctx, gen.CloseTerminalTurnParams{
				AgentResponse: response, RespondedAt: sql.NullTime{Time: at, Valid: true}, SessionID: string(turn.SessionID),
			})
		}
		if db == nil {
			return closeTurn()
		}
		var open bool
		var lastResponse string
		err := db.QueryRowContext(ctx, `
SELECT responded_at IS NULL, agent_response FROM session_terminal_turns
WHERE session_id = ? ORDER BY id DESC LIMIT 1`, string(turn.SessionID)).Scan(&open, &lastResponse)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if open {
			return closeTurn()
		}
		if response == "" || (err == nil && lastResponse == response) {
			return nil
		}
		if err := q.InsertTerminalTurn(ctx, gen.InsertTerminalTurnParams{
			SessionID: string(turn.SessionID), Harness: string(turn.Harness),
			NativeSessionID: turn.NativeSessionID, UserRequest: domain.AgentStartedTurnRequest, RequestedAt: at,
		}); err != nil {
			return err
		}
		if err := closeTurn(); err != nil {
			return err
		}
		return q.TrimTerminalTurns(ctx, gen.TrimTerminalTurnsParams{
			SessionID: string(turn.SessionID), Keep: domain.TerminalTurnRetention,
		})
	})
}

// ListTerminalTurns returns a session's stored terminal turns, oldest first.
// Markers for turns that no user request started are left out.
func (s *Store) ListTerminalTurns(ctx context.Context, id domain.SessionID) ([]domain.HandoffTurn, error) {
	rows, err := s.qr.ListTerminalTurns(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("list terminal turns: %w", err)
	}
	turns := make([]domain.HandoffTurn, 0, len(rows))
	for _, row := range rows {
		request := strings.TrimSpace(row.UserRequest)
		if request == "" {
			continue
		}
		turns = append(turns, domain.HandoffTurn{UserRequest: request, AgentResponse: row.AgentResponse, At: row.RequestedAt, Harness: domain.AgentHarness(row.Harness)})
	}
	return turns, nil
}

// DeleteTerminalTurnsForThread removes the turns of one provider thread after
// its history was imported into the conversation tables, so a turn never sits
// in both stores. Rows recorded before the hook reported a thread id match on
// the provider alone. Other providers' turns were not imported and stay.
func (s *Store) DeleteTerminalTurnsForThread(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, nativeSessionID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.DeleteTerminalTurnsForThread(ctx, gen.DeleteTerminalTurnsForThreadParams{
		SessionID: string(id), Harness: string(harness), NativeSessionID: nativeSessionID,
	}); err != nil {
		return fmt.Errorf("delete imported terminal turns: %w", err)
	}
	return nil
}
