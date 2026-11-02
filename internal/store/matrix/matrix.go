// Package matrix backs Teamcp with a Matrix homeserver.
//
// Nothing is stored here: the homeserver owns rooms, the event DAG, membership,
// room state, read receipts and media. This package is a translation layer
// between Teamcp's vocabulary and the Client-Server API, which is why it is
// small — that is the whole point of choosing Matrix as the cloud backend.
//
// The one thing Matrix does NOT give us is the right to trust a user ID. A
// homeserver account can be registered by anyone who can reach it, so an MXID is
// only an identity when it is inside OUR namespace. See ParseUserID.
package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/open-mcp-ai/teamcp/internal/store"
)

// NamespacePrefix is the localpart prefix Teamcp owns. An appservice can puppet
// any user under its namespace, so this prefix is how a principal gets a stable
// identity without Teamcp storing a user table.
//
// NO LEADING UNDERSCORE, and that is not cosmetic: Dendrite rejects any localpart
// starting with `_` ("username cannot start with a '_'"). The first draft used
// `teamcp_` and worked in unit tests — which never create a real account — and
// failed at the first real registration. A namespace has to be a legal localpart
// on the homeservers this runs against.
const NamespacePrefix = "teamcp_"

// ErrNotNamespaced is returned for an MXID outside the namespace.
//
// IT IS A SECURITY BOUNDARY, NOT A FORMAT ERROR. The same homeserver hosts
// accounts belonging to nobody in particular; if such an account were accepted as
// an actor, anyone who can register could claim to be a principal and every
// cross-principal rule would fall. The tool layer maps this to 403.
//
// It wraps store.ErrNotAnActor so the tool layer can recognise it without
// importing this package.
var ErrNotNamespaced = fmt.Errorf("%w：MXID 不在 Teamcp 命名空间内", store.ErrNotAnActor)

// Store is a Teamcp backend over one Matrix account.
type Store struct {
	cli   *mautrix.Client
	actor store.Actor
}

// New connects to a homeserver as userID.
//
// The MXID is parsed BEFORE the client is built, so a misconfigured deployment
// fails at startup rather than at the first call — and fails with the namespace
// error, which says what to fix.
func New(homeserverURL, userID, accessToken string) (*Store, error) {
	actor, err := ParseUserID(id.UserID(userID))
	if err != nil {
		return nil, err
	}
	cli, err := mautrix.NewClient(homeserverURL, id.UserID(userID), accessToken)
	if err != nil {
		return nil, fmt.Errorf("连接 homeserver 失败：%w", err)
	}
	return &Store{cli: cli, actor: actor}, nil
}

// Caps reports what a homeserver can promise.
//
// ExactTotals is false: Matrix has no cheap "how many events in this room".
// /messages pages by token and the sync stream has no total. Claiming a number
// here would put a fabricated denominator in a Content-Range header.
func (s *Store) Caps() store.Caps {
	return store.Caps{ExactTotals: false, CursorKind: "sync-token"}
}

// Whoami asks the homeserver who the token actually belongs to.
//
// The local answer is not enough: a configured token may belong to a different
// account than the configured MXID, and that mismatch would silently attribute
// this agent's messages to someone else. So the server is asked, every time the
// caller asks.
func (s *Store) Whoami(ctx context.Context) (store.Actor, error) {
	resp, err := s.cli.Whoami(ctx)
	if err != nil {
		return store.Actor{}, fmt.Errorf("homeserver 不认这个令牌：%w", err)
	}
	if string(resp.UserID) != s.actor.UserID {
		return store.Actor{}, fmt.Errorf("令牌属于 %s，配置的 UserID 却是 %s：两者必须一致",
			resp.UserID, s.actor.UserID)
	}
	return s.actor, nil
}

// Rooms lists the rooms this account has joined, with the two state fields the
// guide and `ls` actually show.
//
// Each field is fetched separately and a missing one is not an error: a Matrix
// room is legal with no name, no topic and no canonical alias, and refusing to
// list it would hide a room the actor can perfectly well read.
func (s *Store) Rooms(ctx context.Context) ([]store.Room, error) {
	resp, err := s.cli.JoinedRooms(ctx)
	if err != nil {
		return nil, fmt.Errorf("拉取已加入房间失败：%w", err)
	}

	rooms := make([]store.Room, 0, len(resp.JoinedRooms))
	for _, rid := range resp.JoinedRooms {
		r := store.Room{ID: string(rid)}

		var name struct {
			Name string `json:"name"`
		}
		if err := s.cli.StateEvent(ctx, rid, event.NewEventType("m.room.name"), "", &name); err == nil {
			r.Name = name.Name
		}

		var topic struct {
			Topic string `json:"topic"`
		}
		if err := s.cli.StateEvent(ctx, rid, event.NewEventType("m.room.topic"), "", &topic); err == nil {
			r.Purpose = topic.Topic
		}

		var alias struct {
			Alias string `json:"alias"`
		}
		if err := s.cli.StateEvent(ctx, rid, event.NewEventType("m.room.canonical_alias"), "", &alias); err == nil {
			r.Alias = alias.Alias
		}

		rooms = append(rooms, r)
	}
	return rooms, nil
}

// ErrRoomNotFound is returned when a room cannot be addressed.
//
// Resolution is done against the JOINED room list rather than by asking the
// homeserver to resolve an alias. Two things fall out of that, both good: no
// extra API surface to get wrong, and "you can only address a room you are in"
// becomes a property of parsing instead of a check somebody has to remember.
var ErrRoomNotFound = errors.New("房间不存在或你不是成员")

// resolveRoom turns what a caller typed into a room id.
//
// Accepted spellings: the canonical alias (`#dev:host`), the room id (`!abc:host`),
// or the alias localpart (`#dev` / `dev`), which is what a model tends to type.
func (s *Store) resolveRoom(ctx context.Context, room string) (id.RoomID, error) {
	if room == "" {
		return "", fmt.Errorf("%w：（没有给房间）", ErrRoomNotFound)
	}
	rooms, err := s.Rooms(ctx)
	if err != nil {
		return "", err
	}

	want := strings.TrimPrefix(strings.TrimSpace(room), "#")
	for _, r := range rooms {
		if r.ID == room {
			return id.RoomID(r.ID), nil
		}
		alias := strings.TrimPrefix(r.Alias, "#")
		if r.Alias == room || alias == want || alias == room {
			return id.RoomID(r.ID), nil
		}
		// 只给了 localpart：`dev` 匹配 `#dev:host`
		if i := strings.IndexByte(alias, ':'); i > 0 && alias[:i] == want {
			return id.RoomID(r.ID), nil
		}
	}
	return "", fmt.Errorf("%w：%q", ErrRoomNotFound, room)
}

// Messages reads one window of a room's history.
//
// `since` is an opaque token the backend handed out earlier; empty means "start
// from the newest". The window is read BACKWARD (newest first) because that is
// what a returning caller wants, and the reverse order is put back before
// returning so a model reads the conversation in the order it happened.
func (s *Store) Messages(ctx context.Context, room, since string, limit int) (store.Page, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return store.Page{}, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	resp, err := s.cli.Messages(ctx, rid, since, "", mautrix.DirectionBackward, nil, limit)
	if err != nil {
		return store.Page{}, fmt.Errorf("拉取消息失败：%w", err)
	}

	page := store.Page{Next: resp.End, Head: resp.Start, More: len(resp.Chunk) >= limit}
	for i := len(resp.Chunk) - 1; i >= 0; i-- {
		if m, ok := toMessage(string(rid), resp.Chunk[i]); ok {
			page.Messages = append(page.Messages, m)
		}
	}
	return page, nil
}

// Sync reads what arrived after `since` and returns the next cursor.
//
// THIS REPLACED A FORWARD `/messages` READ, and the replacement was forced by a
// real homeserver: a backward `/messages` read returns Start = the infinity
// sentinel (t9223372036854775807_...), which `/messages?dir=f` does not accept as
// a starting point. The unit-test stub happily agreed with the wrong assumption
// because it invented its own tokens.
//
// `timeoutSeconds` is a real long-poll: the homeserver holds the request open
// while the room is quiet, so one call covers the whole wait window.
func (s *Store) Sync(ctx context.Context, room, since string, timeoutSeconds int) (store.Page, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return store.Page{}, err
	}
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}

	// MATRIX'S `timeout` IS IN MILLISECONDS. Passing the seconds straight through
	// made every wait a 12-MILLISECOND long-poll: it returned at once, said
	// "nothing new", and the caller believed the room was quiet.
	timeoutMillis := timeoutSeconds * 1000
	resp, err := s.cli.SyncRequest(ctx, timeoutMillis, since, "", false, event.PresenceOffline)
	if err != nil {
		return store.Page{}, fmt.Errorf("同步失败：%w", err)
	}

	page := store.Page{Next: resp.NextBatch, Head: resp.NextBatch}
	if joined, ok := resp.Rooms.Join[rid]; ok && joined != nil {
		for _, evt := range joined.Timeline.Events {
			if m, ok := toMessage(string(rid), evt); ok {
				page.Messages = append(page.Messages, m)
			}
		}
	}
	return page, nil
}
// Send posts one message.
//
// The event type is `m.room.message` with a `net.teamcp` sub-object, not a custom
// event type, and that choice is load-bearing: Matrix clients MUST ignore
// unknown event types, so a custom type would be invisible to a human sitting in
// Element. With a standard type they read the text; we read the metadata.
func (s *Store) Send(ctx context.Context, room string, msg store.Outgoing) (store.Sent, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return store.Sent{}, err
	}

	meta := map[string]any{}
	if msg.Kind != "" {
		meta["kind"] = msg.Kind
	}
	if msg.Correlation != "" {
		meta["correlation"] = msg.Correlation
	}
	if msg.ReplyTo != "" {
		meta["reply_to"] = msg.ReplyTo
	}

	content := map[string]any{
		"msgtype":    "m.text",
		"body":       msg.Body,
		"net.teamcp": meta,
	}

	resp, err := s.cli.SendMessageEvent(ctx, rid, event.NewEventType("m.room.message"), content)
	if err != nil {
		return store.Sent{}, fmt.Errorf("发送失败：%w", err)
	}
	return store.Sent{EventID: string(resp.EventID)}, nil
}

// toMessage maps one Matrix event to a Message, reporting false for events that
// are not messages at all (membership, redactions, state).
func toMessage(roomID string, evt *event.Event) (store.Message, bool) {
	if evt == nil || evt.Type.Type != "m.room.message" {
		return store.Message{}, false
	}

	// Read the event's OWN fields and the content's RAW bytes.
	//
	// Round-tripping the whole event through encoding/json looked equivalent and
	// was not: the library's MarshalJSON produces a different shape, so every
	// message decoded to nothing and `read` answered 204 on a room that plainly
	// had messages in it. Only a real homeserver showed this — the unit-test stub
	// built its events from our own struct, so both sides agreed on a shape that
	// the real API does not use.
	var wire struct {
		Body      string `json:"body"`
		NetTeamcp struct {
			Kind        string `json:"kind"`
			Correlation string `json:"correlation"`
			ReplyTo     string `json:"reply_to"`
		} `json:"net.teamcp"`
	}
	if len(evt.Content.VeryRaw) > 0 {
		if err := json.Unmarshal(evt.Content.VeryRaw, &wire); err != nil {
			return store.Message{}, false
		}
	}

	m := store.Message{
		ID:      string(evt.ID),
		RoomID:  roomID,
		Sender:  string(evt.Sender),
		Kind:    wire.NetTeamcp.Kind,
		ReplyTo: wire.NetTeamcp.ReplyTo,
		Body:    wire.Body,
		TS:      evt.Timestamp,
	}
	if m.Kind == "" {
		// 人在 Element 里手打的消息没有这个字段 —— 它们就是陈述。
		m.Kind = "tell"
	}
	if actor, err := ParseUserID(evt.Sender); err == nil {
		m.From = actor
	}
	return m, true
}
// ParseUserID splits an MXID into (principal, kind, agent).
//
// The shape is `@teamcp_<principal>_<k>_<agent>:<host>` with k = `a` (AI) or
// `h` (human). Encoding the principal and the kind IN THE ID is what lets
// Teamcp derive `from`, `trust` and the cross-principal `command` rule with no
// local table at all — the identity is a pure function of the MXID.
//
// Names are `[a-z0-9-]` and may not contain `_`, because `_` is the separator.
// That restriction buys an unambiguous parse; it costs the ability to put an
// underscore in a principal's name, which no one needs.
func ParseUserID(u id.UserID) (store.Actor, error) {
	s := string(u)
	if !strings.HasPrefix(s, "@") {
		return store.Actor{}, fmt.Errorf("%w：%q 不是 MXID", ErrNotNamespaced, s)
	}
	local, host, ok := strings.Cut(s[1:], ":")
	if !ok || host == "" {
		return store.Actor{}, fmt.Errorf("%w：%q 缺少 homeserver 域名", ErrNotNamespaced, s)
	}
	if !strings.HasPrefix(local, NamespacePrefix) {
		return store.Actor{}, fmt.Errorf("%w：%q 应形如 @%s<principal>_<a|h>_<agent>:%s",
			ErrNotNamespaced, s, NamespacePrefix, host)
	}

	parts := strings.Split(strings.TrimPrefix(local, NamespacePrefix), "_")
	if len(parts) != 3 {
		return store.Actor{}, fmt.Errorf("%w：%q 应形如 @%s<principal>_<a|h>_<agent>:%s",
			ErrNotNamespaced, s, NamespacePrefix, host)
	}

	principal, kindLetter, agent := parts[0], parts[1], parts[2]

	var kind store.Kind
	switch kindLetter {
	case "a":
		kind = store.KindAI
	case "h":
		kind = store.KindHuman
	default:
		return store.Actor{}, fmt.Errorf("%w：%q 的种类位是 %q，只能是 a（AI）或 h（人）",
			ErrNotNamespaced, s, kindLetter)
	}

	for _, name := range []string{principal, agent} {
		if !validName(name) {
			return store.Actor{}, fmt.Errorf("%w：%q 不是合法名字（只允许 a-z0-9-，不以 - 开头结尾）",
				ErrNotNamespaced, name)
		}
	}

	return store.Actor{Principal: principal, Agent: agent, Kind: kind, UserID: s}, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 32 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}
