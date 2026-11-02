// Package store is the seam between the `team` tool and whatever actually holds
// the rooms.
//
// It exists because there are two real backends, not because abstraction is
// nice: a Matrix homeserver (the cloud shape) and, if anyone ever needs it, a
// local file. They differ in more than implementation — they differ in what they
// can GUARANTEE — so the seam carries capabilities rather than pretending they
// are interchangeable.
package store

import (
	"context"
	"errors"
)

// ErrNotAnActor 表示一个后端身份不在本服务的命名空间内。
//
// 它必须是包级哨兵，因为工具层要把它映射成 403 —— 而工具层不应该认识任何
// 具体后端（它不 import matrix）。后端用 %w 把它包进自己的错误里。
var ErrNotAnActor = errors.New("身份不在本服务命名空间内")

// Caps reports what a backend can promise. The TEAM/1 envelope reads this to
// decide which headers it is allowed to write.
//
// WITHOUT THIS, A BACKEND SILENTLY LIES. A message window's header says
// `messages 1-20/57`; a backend that cannot count cheaply would either invent a
// total or omit it, and the caller cannot tell which happened. A wrong total is
// worse than no total, because the caller resumes from a window it never had.
type Caps struct {
	// ExactTotals is false when the backend cannot cheaply say how many items
	// the whole stream holds. The envelope then renders `messages a-b/*`.
	ExactTotals bool

	// CursorKind names the opaque cursor's origin: "seq" for a local integer
	// position, "sync-token" for a Matrix /sync batch token.
	//
	// The cursor is opaque on the wire ON PURPOSE. `--since` accepts the token
	// this backend handed out and nothing else; a caller that tries to do
	// arithmetic on it gets a 400 rather than a wrong window.
	CursorKind string
}

// Kind says whether an actor is a person at a keyboard or an AI instance.
//
// This is NOT "two systems": rooms, membership, cursors and audit treat them
// identically. It only matters where authority differs — a human's `command` is
// an instruction, a sibling AI's is a suggestion.
type Kind string

const (
	KindAI    Kind = "ai"
	KindHuman Kind = "human"
)

// Actor is one participant: a principal (the trust anchor) plus which of that
// principal's participants is speaking.
type Actor struct {
	Principal string
	Agent     string
	Kind      Kind
	// UserID is the backend's native identity (a Matrix MXID, a local ULID, …).
	// It is what a message's `from` ultimately points at.
	UserID string
}

// Room is one channel, as the backend sees it.
type Room struct {
	// ID is the backend's native identifier. Teamcp addresses rooms by alias in
	// the DSL because a model has to type it; ID is what the API needs.
	ID string
	// Alias is the human/model-readable address, e.g. `#dev:matrix.local`.
	Alias string
	// Slug is the short form used in `cmd`: `acme/dev`. Empty when the backend
	// has no team/slug split (Matrix rooms are addressed by alias).
	Slug string
	Name string
	// Purpose is written for the AIs, not for humans: what this room is for.
	Purpose string
}

// Message is one thing said in a room.
type Message struct {
	// ID is the backend's event id, and what `--reply-to` points at.
	ID string
	// RoomID is the backend's native room identifier.
	RoomID string
	// Sender is the raw backend identity (an MXID).
	Sender string
	// From is the parsed actor, when the sender is inside our namespace. An
	// empty From means "someone we do not model" — a stranger on the homeserver,
	// which is exactly what `trust` has to reflect.
	From Actor
	// Kind is the Teamcp message kind (tell/ask/answer/…). Messages from a human
	// typing in Element have no kind field, so they default to `tell`.
	Kind string
	// ReplyTo is the id of the message this one answers or acknowledges.
	//
	// It is what makes the obligation list possible: "which asks are still
	// unanswered" is a set difference over this field, and it is the reason
	// `catchup` can say something the raw log cannot.
	ReplyTo string
	Body    string
	// TS is the backend timestamp in milliseconds.
	TS int64
}

// Page is one window of a room's history.
type Page struct {
	Messages []Message
	// Next is the opaque cursor after this window. It is what `--since` accepts;
	// callers must not do arithmetic on it.
	Next string
	// Head is the backend's position for "now", as of this read.
	//
	// `wait` needs it, and this is the bug it fixes: with no `--since`, a
	// backward read returns the tail of the history and everything in it looks
	// new, so the first wait answered instantly with messages from before it was
	// called. Head is the baseline a wait starts from.
	Head string
	// More is true when the backend withheld messages it has.
	More bool
}

// Outgoing is one message to send.
type Outgoing struct {
	Kind        string
	Body        string
	Correlation string
	ReplyTo     string
}

// Sent is what a backend reports after accepting a message.
type Sent struct {
	EventID string
	// Cursor is the position this message occupies, when the backend can say.
	// It is opaque for the same reason Page.Next is.
	Cursor string
}

// Store is what the tool layer needs from a backend. It is deliberately small —
// every method here is one the tool layer actually calls.
//
// THE CONSOLE USES THE SAME METHODS. A human-readable timeline is a second view
// over Messages, not a second client: one implementation, two audiences.
type Store interface {
	Caps() Caps
	Whoami(ctx context.Context) (Actor, error)
	Rooms(ctx context.Context) ([]Room, error)
	// Messages reads BACKWARD from the newest (or from `since`) — a history read.
	Messages(ctx context.Context, room, since string, limit int) (Page, error)
	// Sync reads what arrived AFTER a sync cursor, and returns the next one.
	//
	// This is `/sync`, not `/messages`, and the reason is a real-machine finding:
	// a backward `/messages` read hands back Start = the "infinity" sentinel
	// (t9223372036854775807_...) and that token is NOT a valid forward position.
	// Forward scanning needs the sync token space. The timeout argument is a
	// long-poll: the homeserver holds the request open, so one call waits instead
	// of N polls.
	Sync(ctx context.Context, room, since string, timeoutSeconds int) (Page, error)
	Send(ctx context.Context, room string, msg Outgoing) (Sent, error)

	// 下面这些是"管理房间与状态"的原语，对应 DSL 的原生层与 join/new/ack。
	Join(ctx context.Context, roomOrAlias string) (string, error)
	Ack(ctx context.Context, room, eventID string) (string, error)
	State(ctx context.Context, room, eventType, stateKey string) (string, error)
	Event(ctx context.Context, room, eventID string) (Message, error)
	Redact(ctx context.Context, room, eventID, reason string) error
	Invite(ctx context.Context, room, mxid string) error
	Kick(ctx context.Context, room, mxid, reason string) error
	CreateRoom(ctx context.Context, name, topic, aliasLocalpart string, invite []string) (string, error)

	// 产物是房间 state event，不是本地文件（见 internal/store/matrix/artifacts.go）。
	PutArtifact(ctx context.Context, room, slug, kind, title, body string) (int, error)
	GetArtifact(ctx context.Context, room, slug string) (string, error)
	ListArtifacts(ctx context.Context, room string) (string, error)
	Members(ctx context.Context, room string) (string, error)
	Leave(ctx context.Context, room string) error
}
