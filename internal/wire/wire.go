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
	Run     string `json:"run"` // "" = ad hoc
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
