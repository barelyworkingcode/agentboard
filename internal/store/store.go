// Package store keeps the board in one SQLite file: a single writer handle
// and a small pool of query-only readers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrClosed       = errors.New("decision already closed")
	ErrOpenDecision = errors.New("session has an open decision")
	ErrNoSession    = errors.New("no session")
	ErrNoRun        = errors.New("no run")
)

type Store struct {
	w *sql.DB
	r *sql.DB
}

const ctxColumns = `machine TEXT NOT NULL DEFAULT '', project TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
  repo TEXT NOT NULL DEFAULT '', issue INTEGER NOT NULL DEFAULT 0, session_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '', run TEXT NOT NULL DEFAULT ''`

// All times: server clock, unix ms. 0 = none.
var schemaV1 = strings.ReplaceAll(`
CREATE TABLE runs (
  name TEXT PRIMARY KEY, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0,
  coordinator_session TEXT NOT NULL DEFAULT '', coordinator_name TEXT NOT NULL DEFAULT '') STRICT;
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  machine TEXT NOT NULL DEFAULT '', project TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
  repo TEXT NOT NULL DEFAULT '', issue INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL DEFAULT '',
  run TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK (state IN ('active','waiting','idle','ended')),
  waiting_on TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL DEFAULT '', tool_use_id TEXT NOT NULL DEFAULT '',
  tool_since INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0) STRICT;
CREATE INDEX sessions_state_seen ON sessions(state, last_seen_at);
CREATE TABLE events (
  id INTEGER PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  at INTEGER NOT NULL, kind TEXT NOT NULL CHECK (kind IN ('state','note','decision')),
  state TEXT NOT NULL, note TEXT NOT NULL DEFAULT '') STRICT;
CREATE INDEX events_session_at ON events(session_id, at);
CREATE TABLE decisions (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('question','review','notify')),
  question TEXT NOT NULL, rec TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','answered','dismissed')),
  answer TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, closed_at INTEGER NOT NULL DEFAULT 0,
  CTX) STRICT;
CREATE INDEX decisions_status_session ON decisions(status, session_id);
CREATE TABLE items (
  repo TEXT NOT NULL, number INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', pr INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT '', tier TEXT NOT NULL DEFAULT '', run TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  machine TEXT NOT NULL DEFAULT '', project TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo, number)) STRICT;
CREATE TABLE notes (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('well','less')),
  text TEXT NOT NULL, at INTEGER NOT NULL, CTX) STRICT;
CREATE TABLE log (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('log','ask','answer')),
  text TEXT NOT NULL, at INTEGER NOT NULL, CTX) STRICT;
CREATE INDEX log_at ON log(at);
CREATE INDEX log_session_at ON log(session_id, at);
CREATE TABLE meter (run TEXT PRIMARY KEY, open_start INTEGER NOT NULL DEFAULT 0, open_now INTEGER NOT NULL DEFAULT 0,
  filed INTEGER NOT NULL DEFAULT 0, closed INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL) STRICT;
`, "CTX", ctxColumns)

// migrations are append-only; index i brings the schema to user_version i+1.
var migrations = []string{schemaV1}

const (
	writerParams = "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	readerParams = "_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)"
)

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	w, err := sql.Open("sqlite", "file:"+path+"?"+writerParams)
	if err != nil {
		return nil, fmt.Errorf("open writer %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", "file:"+path+"?"+readerParams)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open readers %s: %w", path, err)
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(2)
	r.SetConnMaxIdleTime(time.Minute)
	if err := r.Ping(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("ping readers %s: %w", path, err)
	}
	return &Store{w: w, r: r}, nil
}

func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d)", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", v+1, err)
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration v%d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("set user_version %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", v+1, err)
		}
	}
	return nil
}

// write runs fn in one immediate transaction on the writer.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write: %w", err)
	}
	return nil
}
