package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// ApplyFunc is the hook state machine, injected so the store stays free of it.
type ApplyFunc func(cur wire.HookState, exists bool, h wire.HookPost, at int64) (next wire.HookState, ev *wire.Event, ok bool)

const ctxCols = `machine, project, branch, repo, issue, session_id, name, run`

func ctxArgs(c wire.Ctx) []any {
	return []any{c.Machine, c.Project, c.Branch, c.Repo, c.Issue, c.Session, c.Name, c.Run}
}

// sessionCtxUpdate is the ON CONFLICT clause shared by every session upsert.
const sessionCtxUpdate = `machine = excluded.machine, project = excluded.project, branch = excluded.branch,
  repo = excluded.repo, issue = excluded.issue,
  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE sessions.name END,
  run = CASE WHEN excluded.run <> '' THEN excluded.run ELSE sessions.run END`

// touch applies the CLI-write upsert rules for c.Session and registers c.Run.
func touch(ctx context.Context, tx *sql.Tx, c wire.Ctx, at int64) error {
	if err := ensureRun(ctx, tx, c.Run, at); err != nil {
		return err
	}
	if c.Session == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions
  (id, machine, project, branch, repo, issue, name, run, state, started_at, last_seen_at)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'idle', ?, ?)
  ON CONFLICT(id) DO UPDATE SET `+sessionCtxUpdate+`,
  last_seen_at = CASE WHEN sessions.state = 'ended' THEN sessions.last_seen_at ELSE excluded.last_seen_at END`,
		c.Session, c.Machine, c.Project, c.Branch, c.Repo, c.Issue, c.Name, c.Run, at, at)
	if err != nil {
		return fmt.Errorf("upsert session %s: %w", c.Session, err)
	}
	return nil
}

func ensureRun(ctx context.Context, tx *sql.Tx, name string, at int64) error {
	if name == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs (name, started_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`, name, at); err != nil {
		return fmt.Errorf("upsert run %s: %w", name, err)
	}
	return nil
}

// addEvent records an event stamped with the session's current state; it is a
// no-op when the session row does not exist.
func addEvent(ctx context.Context, tx *sql.Tx, sessionID string, at int64, kind, note string) error {
	if sessionID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, at, kind, state, note)
  SELECT id, ?, ?, state, ? FROM sessions WHERE id = ?`, at, kind, note, sessionID)
	if err != nil {
		return fmt.Errorf("add %s event for session %s: %w", kind, sessionID, err)
	}
	return nil
}

func (s *Store) ApplyHook(ctx context.Context, h wire.HookPost, at int64, apply ApplyFunc) error {
	c := h.Ctx
	if c.Session == "" {
		return ErrNoSession
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var cur wire.HookState
		exists := true
		err := tx.QueryRowContext(ctx, `SELECT state, waiting_on, tool, tool_use_id, tool_since, last_seen_at, ended_at
  FROM sessions WHERE id = ?`, c.Session).Scan(&cur.State, &cur.WaitingOn, &cur.Tool, &cur.ToolUseID, &cur.ToolSince, &cur.LastSeenAt, &cur.EndedAt)
		if errors.Is(err, sql.ErrNoRows) {
			exists = false
			cur = wire.HookState{}
		} else if err != nil {
			return fmt.Errorf("read session %s: %w", c.Session, err)
		}
		next, ev, ok := apply(cur, exists, h, at)
		if !ok {
			return nil
		}
		if err := ensureRun(ctx, tx, c.Run, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions
  (id, machine, project, branch, repo, issue, name, run, state, waiting_on, tool, tool_use_id, tool_since,
   started_at, last_seen_at, ended_at)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  ON CONFLICT(id) DO UPDATE SET `+sessionCtxUpdate+`,
  state = excluded.state, waiting_on = excluded.waiting_on, tool = excluded.tool,
  tool_use_id = excluded.tool_use_id, tool_since = excluded.tool_since,
  last_seen_at = excluded.last_seen_at, ended_at = excluded.ended_at`,
			c.Session, c.Machine, c.Project, c.Branch, c.Repo, c.Issue, c.Name, c.Run,
			next.State, next.WaitingOn, next.Tool, next.ToolUseID, next.ToolSince,
			at, next.LastSeenAt, next.EndedAt)
		if err != nil {
			return fmt.Errorf("apply hook %s to session %s: %w", h.Event, c.Session, err)
		}
		if ev == nil {
			return nil
		}
		evAt, kind, state := ev.At, ev.Kind, ev.State
		if evAt == 0 {
			evAt = at
		}
		if kind == "" {
			kind = "state"
		}
		if state == "" {
			state = next.State
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, at, kind, state, note) VALUES (?, ?, ?, ?, ?)`,
			c.Session, evAt, kind, state, ev.Note); err != nil {
			return fmt.Errorf("add hook event for session %s: %w", c.Session, err)
		}
		return nil
	})
}

func (s *Store) SetStateNote(ctx context.Context, c wire.Ctx, note string, at int64) error {
	if c.Session == "" {
		return ErrNoSession
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET note = ? WHERE id = ?`, note, c.Session); err != nil {
			return fmt.Errorf("set note on session %s: %w", c.Session, err)
		}
		return addEvent(ctx, tx, c.Session, at, "note", note)
	})
}

func (s *Store) Ask(ctx context.Context, c wire.Ctx, kind, question, rec string, at int64) (int64, error) {
	if kind == "" {
		kind = "question"
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO decisions (kind, question, rec, created_at, `+ctxCols+`)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{kind, question, rec, at}, ctxArgs(c)...)...)
		if err != nil {
			return fmt.Errorf("insert decision: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("decision id: %w", err)
		}
		if _, err := insertLog(ctx, tx, c, "ask", question, at); err != nil {
			return err
		}
		return addEvent(ctx, tx, c.Session, at, "decision", "decision posted")
	})
	return id, err
}

func (s *Store) Answer(ctx context.Context, c wire.Ctx, id int64, text string, at int64) error {
	return s.closeDecision(ctx, c, id, "answered", text, at)
}

func (s *Store) Dismiss(ctx context.Context, c wire.Ctx, id int64, at int64) error {
	return s.closeDecision(ctx, c, id, "dismissed", "", at)
}

func (s *Store) closeDecision(ctx context.Context, c wire.Ctx, id int64, status, answer string, at int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var cur, asker string
		err := tx.QueryRowContext(ctx, `SELECT status, session_id FROM decisions WHERE id = ?`, id).Scan(&cur, &asker)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read decision %d: %w", id, err)
		}
		if cur != "open" {
			return ErrClosed
		}
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE decisions SET status = ?, answer = ?, closed_at = ? WHERE id = ?`,
			status, answer, at, id); err != nil {
			return fmt.Errorf("close decision %d: %w", id, err)
		}
		if status == "answered" {
			if _, err := insertLog(ctx, tx, c, "answer", answer, at); err != nil {
				return err
			}
		}
		return addEvent(ctx, tx, asker, at, "decision", "decision "+status)
	})
}

func insertLog(ctx context.Context, tx *sql.Tx, c wire.Ctx, kind, text string, at int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO log (kind, text, at, `+ctxCols+`)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{kind, text, at}, ctxArgs(c)...)...)
	if err != nil {
		return 0, fmt.Errorf("insert %s log line: %w", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("log line id: %w", err)
	}
	return id, nil
}

func (s *Store) AppendLog(ctx context.Context, c wire.Ctx, text string, at int64) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		var err error
		id, err = insertLog(ctx, tx, c, "log", text, at)
		return err
	})
	return id, err
}

func (s *Store) AddNote(ctx context.Context, c wire.Ctx, kind, text string, at int64) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO notes (kind, text, at, `+ctxCols+`)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append([]any{kind, text, at}, ctxArgs(c)...)...)
		if err != nil {
			return fmt.Errorf("insert note: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("note id: %w", err)
		}
		return nil
	})
	return id, err
}

func (s *Store) UpsertItem(ctx context.Context, c wire.Ctx, p wire.ItemPost, at int64) (string, error) {
	repo := p.Repo
	if !strings.Contains(repo, "/") && c.Repo != "" {
		owner, _, _ := strings.Cut(c.Repo, "/")
		repo = owner + "/" + repo
	}
	key := fmt.Sprintf("%s#%d", repo, p.Number)
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO items
  (repo, number, title, pr, state, tier, run, updated_at, machine, project, session_id, name)
  VALUES (?1, ?2, COALESCE(?3, ''), COALESCE(?4, 0), COALESCE(?5, ''), COALESCE(?6, ''), ?7, ?8, ?9, ?10, ?11, ?12)
  ON CONFLICT(repo, number) DO UPDATE SET
  title = COALESCE(?3, items.title), pr = COALESCE(?4, items.pr),
  state = COALESCE(?5, items.state), tier = COALESCE(?6, items.tier),
  run = CASE WHEN ?7 <> '' THEN ?7 ELSE items.run END,
  updated_at = ?8, machine = ?9, project = ?10, session_id = ?11, name = ?12`,
			repo, p.Number, nullString(p.Title), nullInt(p.PR), nullString(p.State), nullString(p.Tier),
			c.Run, at, c.Machine, c.Project, c.Session, c.Name)
		if err != nil {
			return fmt.Errorf("upsert item %s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) StartRun(ctx context.Context, c wire.Ctx, name string, at int64) (wire.Run, error) {
	var run wire.Run
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (name, started_at, coordinator_session, coordinator_name)
  VALUES (?, ?, ?, ?)
  ON CONFLICT(name) DO UPDATE SET ended_at = 0,
  coordinator_session = excluded.coordinator_session, coordinator_name = excluded.coordinator_name`,
			name, at, c.Session, c.Name)
		if err != nil {
			return fmt.Errorf("start run %s: %w", name, err)
		}
		if c.Session != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET run = ? WHERE id = ?`, name, c.Session); err != nil {
				return fmt.Errorf("set run %s on session %s: %w", name, c.Session, err)
			}
		}
		run, err = readRun(ctx, tx, name)
		return err
	})
	return run, err
}

func (s *Store) EndRun(ctx context.Context, c wire.Ctx, name string, at int64) (wire.Run, error) {
	var run wire.Run
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		if name == "" {
			name = c.Run
		}
		if name == "" && c.Session != "" {
			err := tx.QueryRowContext(ctx, `SELECT name FROM runs WHERE ended_at = 0 AND coordinator_session = ?
  ORDER BY started_at DESC LIMIT 1`, c.Session).Scan(&name)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("find open run for session %s: %w", c.Session, err)
			}
		}
		if name == "" {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET ended_at = ? WHERE name = ? AND ended_at = 0`, at, name); err != nil {
			return fmt.Errorf("end run %s: %w", name, err)
		}
		var err error
		run, err = readRun(ctx, tx, name)
		return err
	})
	return run, err
}

func readRun(ctx context.Context, tx *sql.Tx, name string) (wire.Run, error) {
	var r wire.Run
	err := tx.QueryRowContext(ctx, `SELECT name, started_at, ended_at, coordinator_session, coordinator_name
  FROM runs WHERE name = ?`, name).Scan(&r.Name, &r.StartedAt, &r.EndedAt, &r.CoordinatorSession, &r.CoordinatorName)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("read run %s: %w", name, err)
	}
	return r, nil
}

func (s *Store) SetMeter(ctx context.Context, c wire.Ctx, p wire.MeterPost, at int64) error {
	run := p.Run
	if run == "" {
		run = c.Run
	}
	if run == "" {
		return ErrNoRun
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := touch(ctx, tx, c, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO meter (run, open_start, open_now, filed, closed, updated_at)
  VALUES (?1, COALESCE(?2, 0), COALESCE(?3, 0), COALESCE(?4, 0), COALESCE(?5, 0), ?6)
  ON CONFLICT(run) DO UPDATE SET
  open_start = COALESCE(?2, meter.open_start), open_now = COALESCE(?3, meter.open_now),
  filed = COALESCE(?4, meter.filed), closed = COALESCE(?5, meter.closed), updated_at = ?6`,
			run, nullInt(p.OpenStart), nullInt(p.OpenNow), nullInt(p.Filed), nullInt(p.Closed), at)
		if err != nil {
			return fmt.Errorf("set meter for run %s: %w", run, err)
		}
		return nil
	})
}

func nullString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
