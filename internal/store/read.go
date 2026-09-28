package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

const (
	boardLogCap     = 200
	boardNotesCap   = 100
	boardItemsCap   = 200
	boardRunsCap    = 50
	detailDecisions = 50
	detailEventsCap = 200
	detailLogCap    = 200
)

const sessionSelect = `SELECT s.id, s.machine, s.project, s.branch, s.repo, s.issue, s.name, s.run,
  s.state, s.waiting_on, s.tool, s.tool_use_id, s.tool_since, s.last_seen_at, s.ended_at,
  s.note, s.started_at, COALESCE(i.pr, 0)
  FROM sessions s LEFT JOIN items i ON i.repo = s.repo AND i.number = s.issue`

func ctxDest(c *wire.Ctx) []any {
	return []any{&c.Machine, &c.Project, &c.Branch, &c.Repo, &c.Issue, &c.Session, &c.Name, &c.Run}
}

func scanSession(sc interface{ Scan(...any) error }) (wire.Session, error) {
	var s wire.Session
	err := sc.Scan(&s.ID, &s.Ctx.Machine, &s.Ctx.Project, &s.Ctx.Branch, &s.Ctx.Repo, &s.Ctx.Issue, &s.Ctx.Name, &s.Ctx.Run,
		&s.State, &s.WaitingOn, &s.Tool, &s.ToolUseID, &s.ToolSince, &s.LastSeenAt, &s.EndedAt,
		&s.Note, &s.StartedAt, &s.PR)
	s.Ctx.Session = s.ID
	return s, err
}

// queryAll runs q and appends one scanned row per result; the result is never nil.
func queryAll[T any](ctx context.Context, tx *sql.Tx, scan func(interface{ Scan(...any) error }) (T, error), q string, args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanDecision(sc interface{ Scan(...any) error }) (wire.Decision, error) {
	var d wire.Decision
	err := sc.Scan(append([]any{&d.ID, &d.Kind, &d.Question, &d.Rec, &d.Status, &d.Answer, &d.CreatedAt, &d.ClosedAt}, ctxDest(&d.Ctx)...)...)
	return d, err
}

func scanRun(sc interface{ Scan(...any) error }) (wire.Run, error) {
	var r wire.Run
	err := sc.Scan(&r.Name, &r.StartedAt, &r.EndedAt, &r.CoordinatorSession, &r.CoordinatorName)
	return r, err
}

func scanMeter(sc interface{ Scan(...any) error }) (wire.Meter, error) {
	var m wire.Meter
	err := sc.Scan(&m.Run, &m.OpenStart, &m.OpenNow, &m.Filed, &m.Closed, &m.UpdatedAt)
	return m, err
}

func scanItem(sc interface{ Scan(...any) error }) (wire.Item, error) {
	var it wire.Item
	err := sc.Scan(&it.Repo, &it.Number, &it.Title, &it.PR, &it.State, &it.Tier, &it.Run, &it.UpdatedAt,
		&it.Ctx.Machine, &it.Ctx.Project, &it.Ctx.Session, &it.Ctx.Name)
	return it, err
}

func scanNote(sc interface{ Scan(...any) error }) (wire.Note, error) {
	var n wire.Note
	err := sc.Scan(append([]any{&n.ID, &n.Kind, &n.Text, &n.At}, ctxDest(&n.Ctx)...)...)
	return n, err
}

func scanLog(sc interface{ Scan(...any) error }) (wire.LogLine, error) {
	var l wire.LogLine
	err := sc.Scan(append([]any{&l.ID, &l.Kind, &l.Text, &l.At}, ctxDest(&l.Ctx)...)...)
	return l, err
}

func scanEvent(sc interface{ Scan(...any) error }) (wire.Event, error) {
	var e wire.Event
	err := sc.Scan(&e.At, &e.Kind, &e.State, &e.Note)
	return e, err
}

const (
	decisionSelect = `SELECT id, kind, question, rec, status, answer, created_at, closed_at, ` + ctxCols + ` FROM decisions`
	itemSelect     = `SELECT repo, number, title, pr, state, tier, run, updated_at, machine, project, session_id, name FROM items`
	logSelect      = `SELECT id, kind, text, at, ` + ctxCols + ` FROM log`
)

// read runs fn in one read transaction so every query sees the same snapshot.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}

func (s *Store) Board(ctx context.Context, now int64) (wire.Board, error) {
	b := wire.Board{Now: now}
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		if b.Sessions, err = queryAll(ctx, tx, scanSession, sessionSelect+` ORDER BY s.last_seen_at DESC, s.id`); err != nil {
			return fmt.Errorf("board sessions: %w", err)
		}
		if b.Decisions, err = queryAll(ctx, tx, scanDecision, decisionSelect+` WHERE status = 'open' ORDER BY created_at DESC, id DESC`); err != nil {
			return fmt.Errorf("board decisions: %w", err)
		}
		if b.Runs, err = queryAll(ctx, tx, scanRun, `SELECT name, started_at, ended_at, coordinator_session, coordinator_name
  FROM runs ORDER BY started_at DESC, name LIMIT ?`, boardRunsCap); err != nil {
			return fmt.Errorf("board runs: %w", err)
		}
		if b.Meters, err = queryAll(ctx, tx, scanMeter, `SELECT run, open_start, open_now, filed, closed, updated_at
  FROM meter ORDER BY updated_at DESC, run`); err != nil {
			return fmt.Errorf("board meters: %w", err)
		}
		if b.Items, err = queryAll(ctx, tx, scanItem, itemSelect+` ORDER BY updated_at DESC, repo, number LIMIT ?`, boardItemsCap); err != nil {
			return fmt.Errorf("board items: %w", err)
		}
		if b.Notes, err = queryAll(ctx, tx, scanNote, `SELECT id, kind, text, at, `+ctxCols+`
  FROM notes ORDER BY at DESC, id DESC LIMIT ?`, boardNotesCap); err != nil {
			return fmt.Errorf("board notes: %w", err)
		}
		if b.Log, err = queryAll(ctx, tx, scanLog, logSelect+` ORDER BY at DESC, id DESC LIMIT ?`, boardLogCap); err != nil {
			return fmt.Errorf("board log: %w", err)
		}
		return nil
	})
	return b, err
}

func (s *Store) SessionDetail(ctx context.Context, id string) (wire.SessionDetail, error) {
	var d wire.SessionDetail
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		d.Session, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+` WHERE s.id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read session %s: %w", id, err)
		}
		if d.Decisions, err = queryAll(ctx, tx, scanDecision, decisionSelect+` WHERE session_id = ?
  ORDER BY created_at DESC, id DESC LIMIT ?`, id, detailDecisions); err != nil {
			return fmt.Errorf("session %s decisions: %w", id, err)
		}
		if d.Events, err = queryAll(ctx, tx, scanEvent, `SELECT at, kind, state, note FROM events WHERE session_id = ?
  ORDER BY at DESC, id DESC LIMIT ?`, id, detailEventsCap); err != nil {
			return fmt.Errorf("session %s events: %w", id, err)
		}
		if d.Log, err = queryAll(ctx, tx, scanLog, logSelect+` WHERE session_id = ? ORDER BY at DESC, id DESC LIMIT ?`,
			id, detailLogCap); err != nil {
			return fmt.Errorf("session %s log: %w", id, err)
		}
		it, err := scanItem(tx.QueryRowContext(ctx, itemSelect+` WHERE repo = ? AND number = ?`, d.Session.Ctx.Repo, d.Session.Ctx.Issue))
		switch {
		case err == nil:
			d.Item = &it
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("session %s item: %w", id, err)
		}
		return nil
	})
	return d, err
}
