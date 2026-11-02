// The Matrix calls behind the verbs that manage rooms and state rather than read
// or write messages: join, ack, state, event, redact, invite, kick, create.
//
// They live in their own file because matrix.go is the read/write core that real
// machines have already exercised; adding nine methods there would bury it.
package matrix

import (
	"context"
	"encoding/json"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/open-mcp-ai/teamcp/internal/store"
)

// Join joins a room by id or alias and returns the room id.
//
// An alias is passed through untouched rather than resolved first: the homeserver
// resolves and joins in one call, and doing it ourselves would turn "not a member
// yet" into an error.
func (s *Store) Join(ctx context.Context, roomOrAlias string) (string, error) {
	if roomOrAlias == "" {
		return "", fmt.Errorf("join 需要一个房间地址")
	}
	resp, err := s.cli.JoinRoom(ctx, roomOrAlias, nil)
	if err != nil {
		return "", fmt.Errorf("加入失败：%w", err)
	}
	return string(resp.RoomID), nil
}

// Ack marks a position as read for THIS identity.
//
// Read state is per Matrix account, which is the whole reason one instance can
// hold several agents without their cursors eating each other.
func (s *Store) Ack(ctx context.Context, room, eventID string) (string, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return "", err
	}
	if eventID == "" {
		// `ack <room>` with no id means "up to the newest message I can see".
		latest, err := s.Messages(ctx, room, "", 1)
		if err != nil {
			return "", err
		}
		if len(latest.Messages) == 0 {
			return "", fmt.Errorf("房间里还没有消息可以回执")
		}
		eventID = latest.Messages[len(latest.Messages)-1].ID
	}
	if err := s.cli.MarkRead(ctx, rid, id.EventID(eventID)); err != nil {
		return "", fmt.Errorf("回执失败：%w", err)
	}
	return eventID, nil
}

// State reads one room state event. An empty type lists every state key of the
// room instead, which is what `state <room>` with no type asks for.
func (s *Store) State(ctx context.Context, room, eventType, stateKey string) (string, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return "", err
	}

	if eventType == "" {
		stateMap, err := s.cli.State(ctx, rid)
		if err != nil {
			return "", fmt.Errorf("读房间 state 失败：%w", err)
		}
		var out []string
		for evType, byKey := range stateMap {
			for key := range byKey {
				if key == "" {
					out = append(out, evType.Type)
				} else {
					out = append(out, evType.Type+"["+key+"]")
				}
			}
		}
		sortStrings(out)
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	var raw json.RawMessage
	if err := s.cli.StateEvent(ctx, rid, event.NewEventType(eventType), stateKey, &raw); err != nil {
		return "", fmt.Errorf("读 %s 失败：%w", eventType, err)
	}
	return string(raw), nil
}

// Event returns one message by id.
func (s *Store) Event(ctx context.Context, room, eventID string) (store.Message, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return store.Message{}, err
	}
	evt, err := s.cli.GetEvent(ctx, rid, id.EventID(eventID))
	if err != nil {
		return store.Message{}, fmt.Errorf("取事件失败：%w", err)
	}
	m, ok := toMessage(string(rid), evt)
	if !ok {
		return store.Message{}, fmt.Errorf("%s 不是一条消息（是 %s）", eventID, evt.Type.Type)
	}
	return m, nil
}

// Redact tombstones an event. It never erases: Matrix keeps the fact that
// something was redacted, which is what "只增不改" means in practice.
func (s *Store) Redact(ctx context.Context, room, eventID, reason string) error {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	var extra []mautrix.ReqRedact
	if reason != "" {
		extra = append(extra, mautrix.ReqRedact{Reason: reason})
	}
	if _, err := s.cli.RedactEvent(ctx, rid, id.EventID(eventID), extra...); err != nil {
		return fmt.Errorf("撤回失败：%w", err)
	}
	return nil
}

// Invite invites one MXID into a room.
func (s *Store) Invite(ctx context.Context, room, mxid string) error {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if _, err := s.cli.InviteUser(ctx, rid, &mautrix.ReqInviteUser{UserID: id.UserID(mxid)}); err != nil {
		return fmt.Errorf("邀请失败：%w", err)
	}
	return nil
}

// Kick removes one MXID from a room.
func (s *Store) Kick(ctx context.Context, room, mxid, reason string) error {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if _, err := s.cli.KickUser(ctx, rid, &mautrix.ReqKickUser{UserID: id.UserID(mxid), Reason: reason}); err != nil {
		return fmt.Errorf("移出失败：%w", err)
	}
	return nil
}

// CreateRoom creates a private room and returns its id.
//
// RoomAliasName makes the homeserver set m.room.canonical_alias, which is what
// lets `#dev` resolve later. Publishing an alias separately (as an earlier dev
// script did) sets a directory entry but NOT the canonical alias, so the room
// showed up as `!abc:host` and `#dev` did not resolve at all.
func (s *Store) CreateRoom(ctx context.Context, name, topic, aliasLocalpart string, invite []string) (string, error) {
	req := &mautrix.ReqCreateRoom{
		Visibility:    "private",
		Name:          name,
		Topic:         topic,
		RoomAliasName: aliasLocalpart,
		Preset:        "private_chat",
	}
	for _, u := range invite {
		req.Invite = append(req.Invite, id.UserID(u))
	}
	resp, err := s.cli.CreateRoom(ctx, req)
	if err != nil {
		return "", fmt.Errorf("建房间失败：%w", err)
	}
	return string(resp.RoomID), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
