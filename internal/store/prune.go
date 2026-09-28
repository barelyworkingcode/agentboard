package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

const notOpenDecision = `id NOT IN (SELECT session_id FROM decisions WHERE status = 'open')`

// prunePredicate returns the WHERE clause (without the open-decision
// exclusion) and its arguments for a retention mode.
func prunePredicate(mode string, olderThan time.Duration, now int64) (string, []any, error) {
	older := olderThan.Milliseconds()
	switch mode {
	case "ended_older":
		return `state = 'ended' AND ended_at <= ?`, []any{now - older}, nil
	case "ended_all":
		return `state = 'ended'`, nil, nil
	case "quiet":
		older = max(older, wire.QuietAfter.Milliseconds())
		return `state = 'active' AND tool = '' AND last_seen_at <= ?`, []any{now - older}, nil
	}
	return "", nil, fmt.Errorf("unknown prune mode %q", mode)
}

func (s *Store) Prune(ctx context.Context, mode string, olderThan time.Duration, now int64) (wire.PruneResp, error) {
	pred, args, err := prunePredicate(mode, olderThan, now)
	if err != nil {
		return wire.PruneResp{}, err
	}
	var matched, deleted int
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE `+pred, args...).Scan(&matched); err != nil {
			return fmt.Errorf("count %s sessions: %w", mode, err)
		}
		victims := `SELECT id FROM sessions WHERE ` + pred + ` AND ` + notOpenDecision
		if _, err := tx.ExecContext(ctx, `DELETE FROM decisions WHERE status <> 'open' AND session_id IN (`+victims+`)`, args...); err != nil {
			return fmt.Errorf("prune %s decisions: %w", mode, err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE `+pred+` AND `+notOpenDecision, args...)
		if err != nil {
			return fmt.Errorf("prune %s sessions: %w", mode, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("prune %s sessions: %w", mode, err)
		}
		deleted = int(n)
		return nil
	})
	if err != nil {
		return wire.PruneResp{}, err
	}
	if deleted > 0 {
		s.checkpoint(ctx)
	}
	return wire.PruneResp{Deleted: deleted, Kept: matched - deleted}, nil
}

// checkpoint truncates the WAL after deletes. Its failure is logged, never returned.
func (s *Store) checkpoint(ctx context.Context) {
	var busy, logFrames, checkpointed int
	err := s.w.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		slog.Warn("wal checkpoint failed", "err", err)
		return
	}
	slog.Debug("wal checkpoint", "busy", busy, "log", logFrames, "checkpointed", checkpointed)
}

func (s *Store) ClearCounts(ctx context.Context, now int64) (wire.ClearCounts, error) {
	var cc wire.ClearCounts
	counts := []struct {
		mode  string
		older time.Duration
		dst   *int
	}{
		{"ended_older", 24 * time.Hour, &cc.EndedOlder24h},
		{"ended_all", 0, &cc.EndedAll},
		{"quiet", 2 * time.Hour, &cc.Quiet2h},
	}
	for _, c := range counts {
		pred, args, err := prunePredicate(c.mode, c.older, now)
		if err != nil {
			return cc, err
		}
		q := `SELECT count(*) FROM sessions WHERE ` + pred + ` AND ` + notOpenDecision
		if err := s.r.QueryRowContext(ctx, q, args...).Scan(c.dst); err != nil {
			return cc, fmt.Errorf("count %s sessions: %w", c.mode, err)
		}
	}
	return cc, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		var open int
		err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM decisions WHERE session_id = ?1 AND status = 'open')
  FROM sessions WHERE id = ?1`, id).Scan(&open)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read session %s: %w", id, err)
		}
		if open > 0 {
			return ErrOpenDecision
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM decisions WHERE session_id = ?`, id); err != nil {
			return fmt.Errorf("delete decisions of session %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete session %s: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.checkpoint(ctx)
	return nil
}
