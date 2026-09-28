## Contract (agentboard#1)

### 0. Module, toolchain, layout

- Module `github.com/barelyworkingcode/agentboard`, `go 1.25.0` (the minimum `modernc.org/sqlite` v1.59.0 requires).
- **Only direct Go dependency:** `modernc.org/sqlite v1.59.0`. Its indirect dependencies are allowed. No cgo: `CGO_ENABLED=0`.
- **Test-only npm dependency:** `@playwright/test` in `e2e/`. It never ships. The product has no Node and no frontend build.
- **One command name:** `agentboard`. There is no `ab` alias anywhere (CLI, hooks, README, T8).
- Branch: `feat/1-agentboard`.

```
go.mod, go.sum
main.go                      dispatch: serve | hook | everything else → cli
internal/wire/wire.go        shared JSON + state types (pinned in §3.1)
internal/store/              SQLite: schema, migrations, queries, prune
internal/hookstate/          pure state machine + display rule
internal/server/             HTTP, ingest gate, origin check, SSE, listeners, serve flags
internal/client/             context gathering, POST, fail-soft
internal/cli/                subcommand grammar
internal/hook/               `agentboard hook` adapter (stdin → /api/hook)
web/                         embed.go, index.html, app.js, app.css
examples/claude-settings.json
build.sh, README.md, .gitignore
test/integration/            exec-the-binary tests, budget, hygiene
e2e/                         Playwright
```

### 1. File ownership

| Task | Owns (no other task edits these) |
|---|---|
| **T1 Store** | `go.mod`, `go.sum`, `internal/wire/wire.go`, `internal/store/*.go` (non-test) |
| **T2 Server** | `internal/server/*.go` (non-test) |
| **T3 CLI** | `internal/client/*.go`, `internal/cli/*.go` (non-test) |
| **T4 Hooks** | `internal/hookstate/*.go`, `internal/hook/*.go` (non-test), `examples/claude-settings.json` |
| **T5 Page** | `web/embed.go`, `web/index.html`, `web/app.js`, `web/app.css` |
| **T6 Package** | `main.go`, `build.sh`, `README.md`, `.gitignore` |
| **T7 Tests** | every `*_test.go`, every `testdata/`, `test/integration/**`, `e2e/**` |

Rules:
- Dev agents write no `_test.go` files.
- `wire.go` is pinned below. A change to it goes through the planner, not an agent.
- If a dependency hasn't landed yet, code against its pinned signature and don't create its file.

Dependency order, at most 2 agents at once:
1. T1 ‖ T3
2. T4 ‖ T2. T4 lands `hookstate` first, because T2 imports it.
3. T5 ‖ T6

T7 writes tests from this contract at any point and runs them at the end.

**`web/embed.go` (T5, verbatim):**
```go
package web

import "embed"

//go:embed index.html app.js app.css
var FS embed.FS
```

**Entry-point signatures (pinned):**
```go
// internal/server
func Main(ctx context.Context, args []string, assets fs.FS, stderr io.Writer) int
// internal/hook
func Main(ctx context.Context, stdin io.Reader, env client.Env, stderr io.Writer) int // always 0
func Parse(stdin []byte) (h wire.HookPost, cwd string, err error)                  // ctx left empty
// internal/cli
func Main(ctx context.Context, args []string, env client.Env, stdout, stderr io.Writer) int
```
`main.go` (T6) calls these:
- `serve` → `server.Main(ctx, args[1:], web.FS, os.Stderr)`
- `hook` → `hook.Main(ctx, os.Stdin, client.EnvFromOS(), os.Stderr)`
- anything else → `cli.Main(ctx, args, client.EnvFromOS(), os.Stdout, os.Stderr)`

### 2. Storage (T1)

**Open.** There are two `*sql.DB` handles on the same file:

- **Writer:** `file:<path>?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate`, with `SetMaxOpenConns(1)`.
- **Readers:** `file:<path>?_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)`, with `SetMaxOpenConns(4)`, `SetMaxIdleConns(2)`, `SetConnMaxIdleTime(1m)`.

The writer opens first. Migrations are an ordered `[]string`, each applied in its own transaction and tracked by `PRAGMA user_version`. v1 is:

```sql
-- CTX = machine TEXT NOT NULL DEFAULT '', project TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
--       repo TEXT NOT NULL DEFAULT '', issue INTEGER NOT NULL DEFAULT 0, session_id TEXT NOT NULL DEFAULT '',
--       name TEXT NOT NULL DEFAULT '', run TEXT NOT NULL DEFAULT ''
-- All times: server clock, unix ms. 0 = none.
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
```

**Session upsert rules.** These apply to every write that carries a non-empty `ctx.session`, in the same transaction:

- `machine`, `project`, `branch`, `repo` and `issue` overwrite.
- `name` overwrites only when non-empty.
- `run` is sticky: it is set when non-empty and never cleared by an empty value.
- A row created by a CLI write starts `state='idle'` with `started_at=at`. A row created by a hook takes its state from `hookstate.Apply` (§6).
- CLI writes set `last_seen_at=at` unless the session has ended. They never change `state`.

**Retention.** Every mode excludes sessions that have an open decision: `id NOT IN (SELECT session_id FROM decisions WHERE status='open')`.

| mode | predicate |
|---|---|
| `ended_older` | `state='ended' AND ended_at <= now - older_ms` (browser: 24 h) |
| `ended_all` | `state='ended'` |
| `quiet` | `state='active' AND tool='' AND last_seen_at <= now - max(older_ms, 20 min)` (browser: 2 h) |

- **One transaction:** delete non-open decisions for the matched sessions, then delete the sessions (events cascade).
- **Afterwards:** if `deleted > 0`, run `PRAGMA wal_checkpoint(TRUNCATE)`. Log its result and don't fail on it.
- **What stays:** log, notes, items and meter.
- **`kept`:** the number of rows that matched without the exclusion minus the rows deleted.
- Rows left behind by a destroyed VM (no SessionEnd) are removed with "Delete this session" or the existing modes. There is no extra mode.

**Store API (pinned).**
```go
func Open(path string) (*Store, error)
func (s *Store) Close() error
type ApplyFunc func(cur wire.HookState, exists bool, h wire.HookPost, at int64) (next wire.HookState, ev *wire.Event, ok bool)
func (s *Store) ApplyHook(ctx context.Context, h wire.HookPost, at int64, apply ApplyFunc) error // !ok → no write at all
func (s *Store) SetStateNote(ctx context.Context, c wire.Ctx, note string, at int64) error          // ErrNoSession; writes event kind=note
func (s *Store) Ask(ctx context.Context, c wire.Ctx, kind, question, rec string, at int64) (int64, error) // + log kind=ask, event kind=decision "decision posted"
func (s *Store) Answer(ctx context.Context, c wire.Ctx, id int64, text string, at int64) error      // ErrNotFound, ErrClosed; + log kind=answer, event
func (s *Store) Dismiss(ctx context.Context, c wire.Ctx, id int64, at int64) error                  // ErrNotFound, ErrClosed; + event
func (s *Store) AppendLog(ctx context.Context, c wire.Ctx, text string, at int64) (int64, error)
func (s *Store) UpsertItem(ctx context.Context, c wire.Ctx, p wire.ItemPost, at int64) (key string, err error)
func (s *Store) AddNote(ctx context.Context, c wire.Ctx, kind, text string, at int64) (int64, error)
func (s *Store) StartRun(ctx context.Context, c wire.Ctx, name string, at int64) (wire.Run, error)
func (s *Store) EndRun(ctx context.Context, c wire.Ctx, name string, at int64) (wire.Run, error)    // ErrNotFound
func (s *Store) SetMeter(ctx context.Context, c wire.Ctx, p wire.MeterPost, at int64) error         // ErrNoRun
func (s *Store) Prune(ctx context.Context, mode string, olderThan time.Duration, now int64) (wire.PruneResp, error)
func (s *Store) ClearCounts(ctx context.Context, now int64) (wire.ClearCounts, error)
func (s *Store) DeleteSession(ctx context.Context, id string) error                                 // ErrNotFound, ErrOpenDecision
func (s *Store) Board(ctx context.Context, now int64) (wire.Board, error)                            // Display and Version left empty
func (s *Store) SessionDetail(ctx context.Context, id string) (wire.SessionDetail, error)            // ErrNotFound
var ErrNotFound, ErrClosed, ErrOpenDecision, ErrNoSession, ErrNoRun error
```

Store behaviour:

- **Board caps, newest first:** `log` 200, `notes` 100, `items` 200, `runs` 50. `decisions` are open ones only. `sessions` are all rows. Slices are never nil.
- **Session detail:** decisions for the session, 50 max. Events and log, 200 each, newest first.
- **Item:** matched on `(repo, issue)`, or `null`.
- **`Session.PR`:** `items.pr` where `items.repo = sessions.repo AND items.number = sessions.issue`.
- **Item repo:** if `repo` has no `/` and `ctx.repo` is set, it becomes `owner(ctx.repo)/repo`. If `repo` has no `/` and `ctx.repo` is empty, nothing is stored and the endpoint answers 400 (§3.2).
- **Item fields:** absent pointer fields are kept. `run` is sticky from `ctx.run`.
- **`StartRun`:** upserts by name. An ended run is reopened (`ended_at=0`) and keeps `started_at`. It sets `coordinator_*` from the ctx and sets `sessions.run = name` for `ctx.session`.
- **`EndRun` name resolution:** the `name` argument, else `ctx.run`, else the most recent open run with `coordinator_session = ctx.session`.
- **`SetMeter` run:** `p.Run`, else `ctx.run`, else `ErrNoRun`. Absent pointer fields are kept.
- **Any non-empty `ctx.run`** upserts a `runs` row with `started_at=at` when the run is new.

### 3. Wire

#### 3.1 `internal/wire/wire.go` (pinned; T1 writes it exactly so)

```go
package wire

import "time"

const QuietAfter = 20 * time.Minute

// Ctx labels a post. It is never used for authorization.
type Ctx struct {
	Machine string `json:"machine"`
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Repo    string `json:"repo"`    // "owner/name" from a GitHub origin, else ""
	Issue   int    `json:"issue"`   // 0 = none
	Session string `json:"session"` // "" = none
	Name    string `json:"name"`
	Run     string `json:"run"`     // "" = ad hoc
}

type HookPost struct {
	Ctx              Ctx    `json:"ctx"`
	Event            string `json:"event"`
	Tool             string `json:"tool,omitempty"`
	ToolUseID        string `json:"tool_use_id,omitempty"`
	NotificationType string `json:"notification_type,omitempty"`
	Source           string `json:"source,omitempty"` // SessionStart
	Reason           string `json:"reason,omitempty"` // SessionEnd
}
type AskPost struct {
	Ctx      Ctx    `json:"ctx"`
	Kind     string `json:"kind"` // question|review|notify; "" = question
	Question string `json:"question"`
	Rec      string `json:"rec"`
}
type AnswerPost struct {
	Ctx  Ctx    `json:"ctx"`
	Text string `json:"text"`
}
type CtxPost struct {
	Ctx Ctx `json:"ctx"`
}
type StatePost struct {
	Ctx  Ctx    `json:"ctx"`
	Note string `json:"note"`
}
type LogPost struct {
	Ctx  Ctx    `json:"ctx"`
	Text string `json:"text"`
}
type ItemPost struct {
	Ctx    Ctx     `json:"ctx"`
	Repo   string  `json:"repo"` // "owner/name" or "name"
	Number int     `json:"number"`
	Title  *string `json:"title,omitempty"`
	PR     *int    `json:"pr,omitempty"`
	State  *string `json:"state,omitempty"`
	Tier   *string `json:"tier,omitempty"`
}
type NotePost struct {
	Ctx  Ctx    `json:"ctx"`
	Kind string `json:"kind"` // well|less
	Text string `json:"text"`
}
type RunPost struct {
	Ctx  Ctx    `json:"ctx"`
	Name string `json:"name"`
}
type MeterPost struct {
	Ctx       Ctx    `json:"ctx"`
	Run       string `json:"run"`
	OpenStart *int   `json:"open_start,omitempty"`
	OpenNow   *int   `json:"open_now,omitempty"`
	Filed     *int   `json:"filed,omitempty"`
	Closed    *int   `json:"closed,omitempty"`
}
type PrunePost struct {
	Ctx        Ctx    `json:"ctx"`
	Mode       string `json:"mode"` // ended_older|ended_all|quiet
	OlderThanS int64  `json:"older_than_s"`
}
type ClearPost struct {
	Mode string `json:"mode"`
}

type IDResp struct {
	ID int64 `json:"id"`
}
type DecisionResp struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}
type ItemResp struct {
	Key string `json:"key"` // "owner/name#N"
}
type OKResp struct {
	OK bool `json:"ok"`
}
type PruneResp struct {
	Deleted int `json:"deleted"`
	Kept    int `json:"kept"`
}
type ErrorResp struct {
	Error string `json:"error"`
}
type ClearCounts struct {
	EndedOlder24h int `json:"ended_older_24h"`
	EndedAll      int `json:"ended_all"`
	Quiet2h       int `json:"quiet_2h"`
}

type HookState struct {
	State      string `json:"state"`      // active|waiting|idle|ended
	WaitingOn  string `json:"waiting_on"` // notification_type while waiting
	Tool       string `json:"tool"`
	ToolUseID  string `json:"-"`
	ToolSince  int64  `json:"tool_since"`
	LastSeenAt int64  `json:"last_seen_at"`
	EndedAt    int64  `json:"ended_at"`
}
type Session struct {
	ID  string `json:"id"`
	Ctx Ctx    `json:"ctx"`
	HookState
	Display   string `json:"display"` // active|waiting|quiet|idle|ended
	Note      string `json:"note"`
	PR        int    `json:"pr"`
	StartedAt int64  `json:"started_at"`
}
type Decision struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Question  string `json:"question"`
	Rec       string `json:"rec"`
	Status    string `json:"status"`
	Answer    string `json:"answer"`
	CreatedAt int64  `json:"created_at"`
	ClosedAt  int64  `json:"closed_at"`
	Ctx       Ctx    `json:"ctx"`
}
type Run struct {
	Name               string `json:"name"`
	StartedAt          int64  `json:"started_at"`
	EndedAt            int64  `json:"ended_at"`
	CoordinatorSession string `json:"coordinator_session"`
	CoordinatorName    string `json:"coordinator_name"`
}
type Meter struct {
	Run       string `json:"run"`
	OpenStart int    `json:"open_start"`
	OpenNow   int    `json:"open_now"`
	Filed     int    `json:"filed"`
	Closed    int    `json:"closed"`
	UpdatedAt int64  `json:"updated_at"`
}
type Item struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	PR        int    `json:"pr"`
	State     string `json:"state"`
	Tier      string `json:"tier"`
	Run       string `json:"run"`
	UpdatedAt int64  `json:"updated_at"`
	Ctx       Ctx    `json:"ctx"`
}
type Note struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	At   int64  `json:"at"`
	Ctx  Ctx    `json:"ctx"`
}
type LogLine struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"` // log|ask|answer
	Text string `json:"text"`
	At   int64  `json:"at"`
	Ctx  Ctx    `json:"ctx"`
}
type Event struct {
	At    int64  `json:"at"`
	Kind  string `json:"kind"` // state|note|decision
	State string `json:"state"`
	Note  string `json:"note"`
}
type Board struct {
	Version   string     `json:"version"`
	Now       int64      `json:"now"`
	Sessions  []Session  `json:"sessions"`
	Decisions []Decision `json:"decisions"`
	Runs      []Run      `json:"runs"`
	Meters    []Meter    `json:"meters"`
	Items     []Item     `json:"items"`
	Notes     []Note     `json:"notes"`
	Log       []LogLine  `json:"log"`
}
type SessionDetail struct {
	Version   string     `json:"version"`
	Now       int64      `json:"now"`
	Session   Session    `json:"session"`
	Decisions []Decision `json:"decisions"`
	Events    []Event    `json:"events"`
	Log       []LogLine  `json:"log"`
	Item      *Item      `json:"item"`
}
```

#### 3.2 Endpoints (T2)

All bodies are JSON and every error is `{"error":"…"}`.

- **Body size:** 64 KiB max → 413.
- **Unknown JSON fields:** ignored.
- **Too-long strings:** truncated at a UTF-8 boundary. Limits: question, rec, answer, log and note text 2000 bytes; state note 200; title 200; item state and tier 64; name, machine and run 64; project 128; branch 255.
- **Bad `ctx` values never cause a 400:**
  - an invalid `ctx.repo` (not `^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`) becomes `""`
  - an invalid `ctx.run` becomes `""`
  - an invalid `ctx.session` (not `^[A-Za-z0-9._:-]{1,128}$`) becomes `""`
- **Run names:** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.

**Ingest.** Every route here is `POST` and passes the gate (§3.3) before the body is read.

| Path | Body | Success | Errors |
|---|---|---|---|
| `/api/hook` | `HookPost` | 204 | 400: event not one of the 8 in §6, or empty `ctx.session` |
| `/api/decisions` | `AskPost` | 200 `IDResp` | 400: empty question, bad kind, empty rec when kind ≠ notify |
| `/api/decisions/{id}/answer` | `AnswerPost` | 200 `DecisionResp{status:"answered"}` | 400 empty text · 404 · 409 already closed |
| `/api/decisions/{id}/dismiss` | `CtxPost` | 200 `DecisionResp{status:"dismissed"}` | 404 · 409 |
| `/api/state` | `StatePost` | 200 `OKResp` | 400 empty `ctx.session` |
| `/api/log` | `LogPost` | 200 `IDResp` | 400 empty text |
| `/api/items` | `ItemPost` | 200 `ItemResp` | 400: bad repo, number < 1, pr < 1; a repo without `/` when the sanitised `ctx.repo` is empty gives `repo needs owner/name (no git repo here to infer the owner)` |
| `/api/notes` | `NotePost` | 200 `IDResp` | 400 |
| `/api/runs/start` | `RunPost` | 200 `Run` | 400 bad name |
| `/api/runs/end` | `RunPost` (name may be "") | 200 `Run` | 404 |
| `/api/meter` | `MeterPost` | 200 `OKResp` | 400: no run resolvable, no value given, negative value |
| `/api/prune` | `PrunePost` | 200 `PruneResp` | 400: bad mode, or `older_than_s` < 60 for `ended_older`/`quiet` |

**Read.** Open to every listener, `GET` only.

| Path | Response |
|---|---|
| `/` | `index.html` |
| `/app.js`, `/app.css` | embedded assets, `Cache-Control: no-cache` |
| `/api/board` | `Board`, with ETag |
| `/api/sessions/{id}` | `SessionDetail`, with ETag. 404 if unknown. |
| `/api/clear-counts` | `ClearCounts`, computed at 24 h and 2 h |
| `/events` | SSE (§3.5) |

Any other path returns 404. A wrong method returns 405 with `Allow`.

**Browser actions.** These are `POST` routes under the origin check (§3.4), not the ingest gate.

| Path | Body | Success | Errors |
|---|---|---|---|
| `/ui/clear` | `ClearPost` (thresholds fixed at 24 h and 2 h) | 200 `PruneResp` | 400 |
| `/ui/sessions/{id}/delete` | none | 200 `{"deleted":1}` | 404 · 409 `{"error":"session has an open decision"}` |
| `/ui/decisions/{id}/dismiss` | none | 200 `DecisionResp` | 404 · 409 |

**HTTP server settings.** `ReadHeaderTimeout` 5 s, `IdleTimeout` 60 s, SSE clients capped at 64 (503 beyond that). The server sends no CORS headers anywhere.

#### 3.3 Ingest gate (T2), pure and pinned

```go
func IsIngestListener(bound netip.Addr, allow []netip.Prefix) bool // bound.IsLoopback() || in any allow prefix; unspecified (0.0.0.0, ::) → false
func IngestAllowed(listenerIngest bool, remote netip.Addr, allow []netip.Prefix) bool // listenerIngest && remote.Unmap() in any allow prefix
```

- **Listener flag:** each listener gets its own `http.Server` whose `BaseContext` stamps the flag.
- **Remote address:** comes from `r.RemoteAddr` only, parsed with `netip.ParseAddrPort` and unmapped. The gate reads no header: `X-Forwarded-For`, `X-Real-IP`, `Forwarded` and the rest are ignored.
- **Refusal:** 403 `{"error":"ingest not allowed from this address"}`, logged at WARN with the remote and listener.
- **Wildcard listeners are read-only.**

For tests:
```go
type Options struct { Allow []netip.Prefix; Assets fs.FS; Logger *slog.Logger; Now func() time.Time }
func NewHandler(st *store.Store, opt Options) *Handler
func (h *Handler) ForListener(ingest bool) http.Handler
func (h *Handler) Run(ctx context.Context) // quiet ticker + broadcaster
var RetryInterval = 30 * time.Second
var QuietTick = 30 * time.Second
var Coalesce = 250 * time.Millisecond
```

#### 3.4 Browser-action origin check (T2)

`func SameOrigin(r *http.Request) bool` passes only when all three hold:
- `Origin` is present and not `null`.
- `url.Parse(Origin).Host` equals `r.Host`, case-insensitive.
- `X-Agentboard` is exactly `1`.

A failure returns 403 `{"error":"cross-origin request refused"}`. OPTIONS requests get 405 with no CORS headers. DNS rebinding gets past Origin==Host; the issue accepts this (worst case is a cleared row).

#### 3.5 Version, ETag, SSE (T2)

- **Version:** `"<boot>.<n>"`. `boot` is 6 random hex characters picked at start. `n` is an in-memory counter.
- **Bumps:** `n` rises after every committed write, and when the quiet ticker finds the set of quiet session ids has changed.
- **Read order:** handlers read the version *before* querying.
- **ETag:** `ETag: "<version>"` and `Cache-Control: no-cache` on `/api/board` and `/api/sessions/{id}`. A matching `If-None-Match` gets 304 with no body.
- **Quiet ticker:** runs every `QuietTick` and derives the quiet set from `Board` plus `hookstate.Display`.
- **`/events`:** `Content-Type: text/event-stream`, `Cache-Control: no-store`. On connect it sends `retry: 3000` and then the current version immediately:
  ```
  event: board
  id: <n>
  data: {"version":"<boot>.<n>"}

  ```
- **Coalescing:** at most one `board` event per client per `Coalesce`, carrying the latest version. The final version is never dropped.
- **Keepalive:** `: ka` every 25 s.
- **Payload:** events carry no board data. The page refetches.

### 4. Client context envelope (T3)

```go
type Env struct{ URL, Machine, Name, Run, SessionID, TMUX, TMUXPane string } // AB_URL, AB_MACHINE, AB_NAME, AB_RUN, CLAUDE_CODE_SESSION_ID, TMUX, TMUX_PANE
func EnvFromOS() Env
func Gather(ctx context.Context, dir string, env Env, session string) wire.Ctx // session "" → env.SessionID
func ParseRemote(url string) string      // "owner/name" or ""
func IssueFromBranch(branch string) int  // 0 if none
```

| Field | Derivation |
|---|---|
| machine | `AB_MACHINE`, else `os.Hostname()` cut at the first `.` |
| project | `git -C <dir> rev-parse --path-format=absolute --show-toplevel --git-common-dir` returns 2 lines. If basename(common-dir) is `.git`, use basename(dir(common-dir)), so worktrees report the main repo. Otherwise use basename(toplevel). Not a repo gives `""`. |
| branch | `git -C <dir> symbolic-ref --short -q HEAD`. Detached HEAD gives `""`. |
| repo | `ParseRemote(git -C <dir> remote get-url origin)`. Only the slug leaves the machine; the URL can hold a token. |
| issue | `^(fix\|feat\|chore)/(\d+)-` on the branch, group 2 as an int. A parse failure gives 0. |
| session | hook: `session_id` from stdin. CLI: `CLAUDE_CODE_SESSION_ID`. |
| name | `AB_NAME`. Else, if `TMUX` is set, `tmux display-message -p [-t $TMUX_PANE] '#S'`. Else the first 8 characters of the session id. Else `""`. |
| run | `AB_RUN`, else `""` |
| time | never sent |

- `dir` is the hook's `cwd`, or `os.Getwd()` for the CLI.
- The three git calls run concurrently with `GIT_TERMINAL_PROMPT=0` and a 150 ms sub-deadline.
- `cwd`, paths, `tool_input`, `tool_response`, `message`, `prompt` and `transcript_path` are never sent.

`ParseRemote` regex (the host match is case-insensitive):
```
^(?:(?:https?|ssh|git)://(?:[^@/]+@)?github\.com(?::\d+)?/|(?:[^@/]+@)?github\.com:)([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$
```
It must accept:
- `git@github.com:acme/relay.git`
- `ssh://git@github.com/acme/relay.git`
- `ssh://git@github.com:22/acme/relay`
- `https://github.com/acme/relay`
- `https://github.com/acme/relay.git/`
- `https://x-access-token:T@github.com/acme/relay.git`

It must reject non-GitHub hosts, returning `""`.

### 5. CLI (T3)

The command is `agentboard <subcommand>`. `AB_URL` defaults to `http://127.0.0.1:8790`. Requests go to `POST <AB_URL minus trailing />/api/...`.

**Parsing:**
- Flags may appear anywhere.
- `--` ends flags. Only `--name` tokens are flags; `-h` and `--help` print usage.
- Remaining positional words are joined with single spaces.
- Both `--f v` and `--f=v` work.

| Subcommand | Request | stdout on success |
|---|---|---|
| `ask <question…> [--rec <text>] [--kind question\|review\|notify]` (`--rec` required unless notify) | `/api/decisions` | `<id>` |
| `answer <id> <text…>` | `/api/decisions/{id}/answer` | `answered <id>` |
| `dismiss <id>` | `/api/decisions/{id}/dismiss` | `dismissed <id>` |
| `state <note…>` (`""` clears; needs `CLAUDE_CODE_SESSION_ID`, else exit 2) | `/api/state` | `ok` |
| `log <text…>` | `/api/log` | `ok` |
| `item <repo>#<N> [--title T] [--pr N] [--state S] [--tier T]`, repo#N matching `^(?:[A-Za-z0-9-]+/)?[A-Za-z0-9._-]+#\d+$` | `/api/items` | `<key>` |
| `went well\|less <text…>` | `/api/notes` | `ok` |
| `run start <name>` | `/api/runs/start` | `run <name> started` |
| `run end [<name>]` | `/api/runs/end` | `run <name> ended` |
| `meter [--run R] [--open-start N] [--open-now N] [--filed N] [--closed N]` (at least one number) | `/api/meter` | `ok` |
| `prune --ended-older DUR \| --ended-all \| --quiet DUR` (exactly one; Go duration) | `/api/prune` | `cleared <n>` |
| `hook` | §6 | nothing, ever |
| `serve …` | §7 | nothing; slog to stderr |
| `help` | none | usage |

**Exit codes:**

| Code | When | stderr (one line) |
|---|---|---|
| 0 | success, **or fail-soft**: dial error, timeout, 5xx, or 403 | `agentboard: not recorded: <reason>` |
| 1 | the server rejected the request itself: 400, 404, 409 or 413 | `agentboard: <server error> (<status>)` |
| 2 | local usage error, before any network call | `agentboard: <problem>; see agentboard help` |

**Deadline:** one 250 ms deadline from `Main` entry covers context gathering and the POST, so the process exits well inside 300 ms. On fail-soft, stdout is empty.

**HTTP client:** `Transport.Proxy = nil`, because proxy variables would change the source address. No keep-alive.

### 6. Hooks (T4)

**`agentboard hook` behaviour:**
- It reads all of stdin within the deadline and decodes only these fields: `session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_use_id`, `notification_type`, `source`, `reason`.
- It posts only for these 8 events: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Notification`, `Stop`, `SessionEnd`.
- It posts nothing when the event is another type or `session_id` is empty.
- It **always exits 0 and never writes stdout**, because Claude Code adds plain stdout from SessionStart and UserPromptSubmit to the model's context. Exit-0 stderr goes to Claude Code's debug log only.
- Verified against the hooks docs: the Notification field is `notification_type`, and `agent_needs_input` needs Claude Code v2.1.198 or later.

```go
// internal/hookstate
func Apply(cur wire.HookState, exists bool, h wire.HookPost, at int64) (next wire.HookState, ev *wire.Event, ok bool)
func Display(s wire.Session, hasOpenDecision bool, now int64) string
```

**State machine.**
- Every accepted event sets `last_seen_at = at`.
- New session (`exists=false`): Apply starts from `State:"idle"` and always returns a non-nil `ev`.
- Existing session: `ev` is non-nil whenever `State` changes.

| Event | Condition | state | tool, tool_use_id, tool_since | waiting_on | ev.note |
|---|---|---|---|---|---|
| any except SessionStart | cur is `ended` | **ignored** (`ok=false`, no heartbeat) | | | |
| SessionStart | always (revives an ended session; `ended_at=0`) | idle | cleared | "" | source |
| UserPromptSubmit | — | active | cleared (drops a tool stuck by an Esc interrupt) | "" | |
| PreToolUse | — | active | `tool_name`, `tool_use_id`, `at` | "" | |
| PostToolUse / PostToolUseFailure | `tool_use_id` == current | waiting→active; otherwise unchanged | cleared | "" | |
| PostToolUse / PostToolUseFailure | `tool_use_id` ≠ current (late async, parallel) | unchanged | unchanged | unchanged | |
| Notification | `permission_prompt`, `agent_needs_input`, `elicitation_dialog` or `elicitation_url_dialog` | waiting | unchanged | type | type |
| Notification | any other type | unchanged | unchanged | unchanged | |
| Stop | — | idle | cleared | "" | |
| SessionEnd | — | ended, `ended_at=at` | cleared | "" | reason |

**Display precedence:**
1. `ended`
2. `waiting` (state is waiting, or the session has an open decision)
3. `quiet` (state is active, `tool==""`, and `now - last_seen_at >= wire.QuietAfter`)
4. otherwise the state itself, `active` or `idle`

**Known limits (accepted):**
- **Parallel tools.** A long tool that started before a shorter parallel one can show quiet once the shorter one ends.
- **Destroyed VMs.** A VM destroyed without a SessionEnd leaves its rows until "Delete this session" or a clear mode (§2) removes them.

**`examples/claude-settings.json` (T4, verbatim; T6 embeds it in the README):**
```json
{
  "hooks": {
    "SessionStart":       [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "UserPromptSubmit":   [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "PreToolUse":         [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "PostToolUse":        [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "PostToolUseFailure": [{"hooks": [{"type": "command", "command": "agentboard hook", "async": true}]}],
    "Notification":       [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "Stop":               [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}],
    "SessionEnd":         [{"hooks": [{"type": "command", "command": "agentboard hook", "timeout": 2}]}]
  }
}
```

### 7. `serve` (T2)

```
agentboard serve [--listen IP:PORT]... [--ingest-from CIDR]... [--db PATH]
```

- **`--listen`:** repeatable. The host must be an IP literal (`127.0.0.1:8790`, `[::1]:8790`) or empty (`:8790`, wildcard, read-only). A hostname is exit 2. Default: `127.0.0.1:8790`.
- **`--ingest-from`:** repeatable. Giving it once or more **replaces** the default `127.0.0.0/8, ::1/128, 192.168.64.0/24`. A bad CIDR is exit 2.
- **`--db`:** default `<os.UserConfigDir()>/agentboard/agentboard.db`. The parent directory is created 0700. An open or migrate failure is exit 1.
- **Ingest listeners:** a listener counts as "loopback or bridge" when `IsIngestListener(boundIP, allow)` is true. If no listener qualifies, log a WARN at start.
- **Bind retry:** each address binds independently. A failure (`EADDRNOTAVAIL`, `EADDRINUSE`, anything else) is logged at WARN on the first failure and whenever the error text changes, then at DEBUG. It is retried every `RetryInterval`. A bind failure is never fatal, including when every address fails. INFO `listener bound addr=… ingest=…` fires on success. If a listener's `Serve` returns any error other than `ErrServerClosed`, it goes back into the retry loop.
- **Shutdown:** SIGINT or SIGTERM triggers a 3 s graceful shutdown, then exit 0.

### 8. Page (T5)

The target is `docs/design/mockup.html` on the `design` branch.

- **Assets:** plain ES module, no framework. Only relative URLs (`api/board`, `events`).
- **Text:** posted text only ever goes into `textContent`, never `innerHTML`.
- **Theme and width:** `prefers-color-scheme` sets the theme. At 390 px wide there is no horizontal scroll.
- **Updates:**
  - `EventSource('events')`. On a new version, refetch the board, and the session detail when in that view, sending `If-None-Match`.
  - While the EventSource is not OPEN, poll every 2 s.
  - Never reload the page.
  - Recompute relative ages every 30 s using the offset from `board.now`.

**URL state:**
- `?machine=&run=&project=&session=`
- The run value `_adhoc` means ad hoc. It can't collide with a run name.
- Use `history.pushState`. Filters survive opening a session and going back.

**Filtering is client-side:**
- Sessions, decisions, log and notes filter on `ctx.machine`, `ctx.project` and `ctx.run`.
- Items filter on `ctx.machine`, `run`, and the name part of `repo` for project.
- The meter follows the run chip only: the selected run; with "all runs", the most recently started open run that has a meter; with ad hoc, hidden.
- Chip values are the distinct values across all sections.

**Ordering:**
- What needs you, grouped in this order: question, review, waiting (hook), notify. Newest first within each group.
- Session groups: open runs by `started_at` desc, then ended runs, then ad hoc.
- Rows within a group: waiting, quiet, active, idle, ended, then `last_seen_at` desc.

**Now column:**
- ended → `ended`
- waiting → `waiting on you`
- quiet → `quiet <age>`
- otherwise, join these with ` · `: the note, then `running <tool> since HH:MM` when a tool is set.
- If both are empty: `idle` or `active`.

**Links:**
- Issue: `https://github.com/<repo>/issues/<issue>`, shown as `<name>#<issue>`.
- PR: `…/pull/<pr>`.
- No link when `repo` has no `/` (empty or a bare name).

**Meter.**
- Boxes 1 and 2 come from `Meter`: `open_start → open_now`, then `filed / closed`.
- Boxes 3 and 4 are derived from the run's ledger items:
  - "PRs waiting on you" counts items with `pr > 0` whose `state`, lowercased, doesn't start with `merged` or `closed`.
  - "merged, for your review" counts items whose `state` starts with `merged` and whose `tier` is `high-confidence`.

**Ledger state colour:** lowercased `state` starting with `merged`, `closed` or `done` → done. Containing `decision`, `blocked` or `waiting` → wait. Anything else → run.

**Required `data-testid` hooks:**
- `needs-group[data-kind=question|review|waiting|notify]`
- `decision[data-id]` › `decision-rec`, `decision-dismiss`
- `needs-session[data-session]`
- `run-group[data-run]`
- `session-row[data-session][data-display]` › `a[data-testid=session-link]`
- `meter`
- `item-row[data-key]`
- `note[data-kind]`
- `log-line`
- `chip[data-dim=machine|run|project][data-value]` with `aria-pressed`; the "all" chip has `data-value=""`
- `live[data-state=live|reconnecting]`
- `clear-open`
- `<dialog data-testid=clear-dialog>` with radios `name=clear-mode` (`value=ended_older|ended_all|quiet`), `clear-count[data-mode]`, `clear-confirm`, `clear-cancel`
- `session-view[data-session]`, `session-delete`, `<dialog data-testid=delete-dialog>` › `delete-confirm`

**Requests:** browser POSTs send `X-Agentboard: 1`. Dismissing a decision needs no confirmation.

### 9. Package (T6)

**`build.sh`:**
- Runs `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` for darwin/arm64, darwin/amd64, linux/amd64, linux/arm64 and windows/amd64.
- Output goes to `dist/agentboard-<os>-<arch>[.exe]`.
- It prints sizes and exits non-zero if any artifact is ≥ 20 MiB.
- `dist/` is gitignored.

**README covers:**
- install as `agentboard` (no alias)
- a serve example using `127.0.0.1`, `192.168.64.1`, `192.0.2.10` (LAN stand-in) and `198.51.100.7` (VPN stand-in)
- the relay example: `relay service register --name agentboard --command agentboard --args serve --args=--listen=127.0.0.1:8790 --args=--listen=192.168.64.1:8790 --url http://127.0.0.1:8790 --autostart` (T6 confirms relay's `--args` form)
- devbox setup: `AB_URL=http://192.168.64.1:8790`, `AB_MACHINE=devbox`
- the hooks snippet
- a `curl` example against `/api/log`
- the CLI table

### 10. Acceptance mapping and test surface

The issue's criteria say `ab`. Under decision 15 they are met by `agentboard`.

| AC | Tasks / interface | Unit | Integration | Playwright | Devbox / real pass |
|---|---|---|---|---|---|
| 1 relay service + tray link | T6 README | — | — | — | Owner registers it with the live relay on the Mac (the machine notes require asking first). Rehearse on the devbox relay. |
| 2 LAN read; ingest refused / accepted | T2 §3.3 | `IsIngestListener`, `IngestAllowed`: 127.0.0.1, ::1, ::ffff:127.0.0.1, 192.168.64.5, 192.0.2.50, 198.51.100.7, wildcard | `ForListener(false)` → POST 403, GET `/` 200. `ForListener(true)` with allow=192.0.2.0/24 → loopback POST 403. `X-Forwarded-For: 127.0.0.1` doesn't help. | — | Post from the devbox over the bridge (accepted). Post from a LAN device (403). Open the page on a phone. |
| 3 VM stopped, bridge picked up in 30 s | T2 §7 | — | Busy-port listener freed → bound within `RetryInterval` (tests set 200 ms) while the other listener keeps serving. Binding `192.0.2.1:0` fails, is retried, and is not fatal. | — | Stop the VM, start serve, start the VM, `curl` the bridge within 30 s (on the Mac, with the owner's OK). |
| 4 new session in 2 s, fields filled | T3 §4, T4 §6, T2 | `ParseRemote` cases; `IssueFromBranch`; `Gather` on a temp repo (feat/88-x, ssh and https origin, worktree → main repo name, detached → "") | exec `agentboard hook` with SessionStart JSON → `/api/board` session has machine, project, branch, repo, issue and name, and state idle; stdout empty | row appears within 2000 ms with the issue link `href` | Real `claude` in a repo on a `feat/N-` branch, on each machine |
| 5 needs-you from permission / input; next tool clears | T4 `Apply` | table test of every §6 row, including all four waiting types, the id-mismatch rows and the ended-ignore row | hook sequence via `/api/hook` | `needs-session` appears, then goes after a PreToolUse post | Trigger a real permission prompt and wait more than 6 s without typing (Claude Code's own delay) |
| 6 ask, answer, dismiss | T3, T1, T2 | store `Ask`, `Answer`, `Dismiss`, including ErrClosed | CLI exec: `ask` prints the id; `answer` 404 → exit 1; closed → 409 → exit 1 | decision card shows the rec and context, then disappears after answer | — |
| 7 quiet 20 min, not inside a tool | T4 `Display`, T2 ticker | `Display` with injected `now`, inside and outside a tool; a SessionStart-only session never goes quiet; UserPromptSubmit clears a stuck tool | handler with fake `Now`: display flips and the version bumps | render check via a `page.route` fixture | — |
| 8 page updates in 2 s, no reload | T2 §3.5, T5 | — | SSE event within 1 s of a POST; 304 on a matching `If-None-Match` | POST a log line → it shows within 2000 ms; one navigation entry only | — |
| 9 filters narrow every section; URL keeps them | T5 §8 | — | — | seed 2 machines, 2 projects, 2 runs and ad hoc; each chip narrows each section; reloading the URL restores it | — |
| 10 clear and delete, not open decisions | T1 §2, T2 `/ui/*` | each prune mode and `ClearCounts`, with open-decision exclusion and `kept` | `/ui/*` without Origin or header → 403; delete with an open decision → 409 | dialog counts, confirm, rows with open decisions stay; delete dialog | — |
| 11 server stopped → exit 0 in 300 ms | T3 §5, T4 | exit-code mapping | exec `agentboard log` and `agentboard hook` (SessionStart, UserPromptSubmit) against a closed port and a never-responding listener: exit 0, wall time < 300 ms, stdout empty, one stderr line; 403 and 500 → 0 | — | devbox with the bridge down |
| 12 resource budget | all | — | `test/integration/budget_test.go` (`//go:build budget`), described below | — | Run on the Mac (darwin/arm64) and record the numbers in the PR |
| 13 no names or paths | all | — | `test/integration/hygiene_test.go`: `git ls-files` contains no IPv4 outside 127/8, 0.0.0.0, 192.168.64/24, 192.0.2/24 or 198.51.100/24, and no `/Users/…` or `/home/…` | — | the coordinator's local denylist grep before push |

**Budget test.** It builds with the release flags and asserts size < 20 MiB. It then starts serve on a temp DB with one SSE client attached. Ten goroutines exec the real `agentboard` binary for 60 s:
- SessionStart and UserPromptSubmit once
- then PreToolUse and PostToolUse every 1 s
- `agentboard log` every 10 s

It asserts per-call p95 < 50 ms and reports the max. After 30 s of idle it asserts:
- RSS (`ps -o rss=`) < 25 MiB
- CPU time over the next 60 s < 0.6 s, which is 1%

It runs with `go test -tags budget ./test/integration -run TestBudget -v`.

**Done means:**
- `go vet ./...` is clean and `gofmt -l .` is empty.
- `go test ./...` passes.
- `cd e2e && npx playwright test` passes. No screenshot baselines.

### 11. Budget per task (going past either limit: stop and ask the coordinator)

| Task | Files | Rough diff |
|---|---|---|
| T1 | go.mod, go.sum, wire.go, ≤ 5 store files | ~900 lines |
| T2 | ≤ 7 server files | ~800 |
| T3 | ≤ 2 client files, 1 cli file | ~550 |
| T4 | hookstate.go, hook.go, settings JSON | ~270 |
| T5 | 4 web files | ~1000 |
| T6 | main.go, build.sh, README.md, .gitignore | ~350 |
| T7 | tests listed in §10, ≤ 12 Go test files, ≤ 6 e2e files | ~2000 |

### 12. T8 interface needs (outside this repo)

- **Command:** `agentboard` only. No alias.
- **Environment:** `AB_URL`, `AB_MACHINE`, `AB_NAME`, `AB_RUN`.
- **Subcommands:** `ask` (prints the integer id alone), `answer`, `dismiss`, `log`, `item`, `went`, `run start|end`, `meter`.
- **Exit codes:** as in §5. Scripts must tolerate exit 0 with empty stdout from `ask` (fail-soft).
- **Mapping:** `status-log` → `log`/`item`/`meter`; `devlock` → `log`.

### 13. Decisions made for the owner (log these in the issue)

1. **`PostToolUseFailure` is hooked.** A failing Bash command fires it instead of `PostToolUse`. Without it, the "inside a tool" flag would stick after every red test run. This stays inside AC 7.
2. **PreToolUse runs sync; PostToolUse and PostToolUseFailure run async.** Post events clear the tool only when `tool_use_id` matches, so late or parallel Posts can't wipe the state.
3. **`agentboard hook` never writes stdout and always exits 0.** SessionStart and UserPromptSubmit stdout would cost model tokens.
4. **The client sends only derived labels.** No `cwd`, remote URL, prompt or tool input leaves the machine. It also ignores proxy environment variables.
5. **A gate refusal (403) and 5xx are fail-soft (exit 0).** Only 400, 404, 409 and 413 exit 1.
6. **Wildcard listeners are read-only.** An ingest listener must be bound to a loopback address or one inside `--ingest-from`.
7. **The board is unfiltered on the server; the page filters.** The meter follows the run chip only.
8. **SSE pushes a version, not data, and coalesces to at most one push per 250 ms per client.**
9. **The state machine is a pure package (T4) that the store applies through an injected function.** `main.go` sits with T6. This keeps T1–T4 compilable in isolation.
10. **"Delete this session" keeps the session's log lines, notes and items.** It removes the session, its history and its closed decisions.
11. **`run start` on an existing name reopens that run.**
12. **The elicitation notifications set waiting.** `elicitation_dialog` and `elicitation_url_dialog` wait on the user, just like `permission_prompt` and `agent_needs_input`.
13. **SessionStart starts a session as idle.** A session that is opened but never used never goes quiet. Rows created by the CLI also start idle.
14. **`UserPromptSubmit` is hooked (async, never stdout).** It sets active and clears any tool left stuck by an Esc interrupt, which fires neither Post nor Stop.
15. **The command is `agentboard` only.** The `ab` alias is dropped, because macOS ships ApacheBench as `/usr/sbin/ab`.

### 14. Resolved questions

1. **Meter boxes 3 and 4:** derived from the ledger (§8).
2. **Elicitation dialogs:** set waiting (§6, decision 12).
3. **SessionStart:** starts a session as idle (§6, decision 13).
4. **UserPromptSubmit:** hooked, async → active, clears a stuck tool (§6, decision 14). Rows from destroyed VMs are handled by the existing delete and clear; there is no new mode.
5. **Parallel-tool quiet:** accepted known limit (§6).
6. **`ab` alias:** dropped; `agentboard` only (decision 15).
7. **Unbounded log, notes and items:** accepted; they cost disk only, because board queries are capped.
8. **DNS rebinding:** accepted per the issue (§3.4).

**New, for the coordinator:** UserPromptSubmit is async. If its post lands after the turn's first PreToolUse, it clears that tool, and a long first tool could then show quiet. The model's own latency makes this unlikely. Either accept it as a known limit, or add a guard: forward `prompt_id`, store it with the tool, and have UserPromptSubmit clear only a tool from an older prompt (one column, one wire field).
