// 产物（artifact）与离开房间。
//
// 产物没有单独的存储：它是一条房间 state event，事件类型由 slug 决定。这样
// 持久化、版本、权限全部落在 homeserver 上，Teamcp 依旧一个字节都不存。
package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix/event"
)

// artifactType 是产物 state event 的类型名。
//
// state event 的 state_key 才是 slug，类型名保持一个常量：这样 Matrix 侧能按
// 类型批量列出所有产物，而不需要知道哪一个 slug 存在。
const artifactType = "net.teamcp.artifact"

// 类别必须显式写出。event.NewEventType 会按名字"猜"类别，而 `net.teamcp.artifact`
// 猜出来是消息类；state map 的键用的是 state 类，于是按类型查产物永远查不到，
// 列表返回 null —— 一个不会报错、只是安静地空掉的 bug。
var artifactEvType = event.Type{Type: artifactType, Class: event.StateEventType}

// artifactContent 是 state event 的内容。
type artifactContent struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Version int    `json:"version"`
	Updated string `json:"updated_by"`
}

// PutArtifact 写入或更新一份产物，并自增版本号。
//
// 版本号由现有的 state 决定而不是由调用方给：让调用方报版本，两个 AI 同时写
// 就会撞出同一个版本号，而 state event 是单值的——后写的会静默覆盖先写的，
// 谁都看不出来。
func (s *Store) PutArtifact(ctx context.Context, room, slug, kind, title, body string) (int, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return 0, err
	}
	if !validSlug(slug) {
		return 0, fmt.Errorf("slug 只能用 a-z0-9-，且不超过 64 字符：%q", slug)
	}

	version := 1
	var prev artifactContent
	if err := s.cli.StateEvent(ctx, rid, artifactEvType, slug, &prev); err == nil {
		version = prev.Version + 1
	}

	content := artifactContent{Kind: kind, Title: title, Body: body, Version: version}
	if _, err := s.cli.SendStateEvent(ctx, rid, artifactEvType, slug, content); err != nil {
		return 0, fmt.Errorf("写产物失败：%w", err)
	}
	return version, nil
}

// GetArtifact 读一份产物。空 version 表示要最新（state event 只有一个当前值，
// 所以"最新"就是它本身）。
func (s *Store) GetArtifact(ctx context.Context, room, slug string) (string, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return "", err
	}
	var c artifactContent
	if err := s.cli.StateEvent(ctx, rid, artifactEvType, slug, &c); err != nil {
		return "", fmt.Errorf("没有 slug 为 %q 的产物（或你不是房间成员）：%w", slug, err)
	}
	b, _ := json.Marshal(map[string]any{
		"slug":    slug,
		"kind":    c.Kind,
		"title":   c.Title,
		"version": c.Version,
		"body":    c.Body,
	})
	return string(b), nil
}

// ListArtifacts 列出房间里所有产物的 slug 与版本。
func (s *Store) ListArtifacts(ctx context.Context, room string) (string, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return "", err
	}
	stateMap, err := s.cli.State(ctx, rid)
	if err != nil {
		return "", fmt.Errorf("读房间 state 失败：%w", err)
	}

	type row struct {
		Slug    string `json:"slug"`
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Version int    `json:"version"`
	}
	var out []row
	byKey := stateMap[artifactEvType]
	for slug, evt := range byKey {
		if evt == nil || len(evt.Content.VeryRaw) == 0 {
			continue
		}
		var c artifactContent
		if json.Unmarshal(evt.Content.VeryRaw, &c) != nil {
			continue
		}
		out = append(out, row{Slug: slug, Kind: c.Kind, Title: c.Title, Version: c.Version})
	}
	if out == nil {
		out = []row{} // 空列表要序列化成 []，不是 null：null 看起来像"查询失败"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// Leave 让自己离开房间。留下的消息不会消失——这正是"只增不改"。
func (s *Store) Leave(ctx context.Context, room string) error {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if _, err := s.cli.LeaveRoom(ctx, rid); err != nil {
		return fmt.Errorf("离开失败：%w", err)
	}
	return nil
}

func validSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "-")
}

// Members 列出房间当前成员（joined），按 MXID 排序。
//
// 走 state 而不是 /joined_members：state 里已经带了每个成员的 membership，
// 一次读就够，而且能顺带看出谁在房间里但没 join。
func (s *Store) Members(ctx context.Context, room string) (string, error) {
	rid, err := s.resolveRoom(ctx, room)
	if err != nil {
		return "", err
	}
	stateMap, err := s.cli.State(ctx, rid)
	if err != nil {
		return "", fmt.Errorf("读成员失败：%w", err)
	}

	type row struct {
		UserID     string `json:"user_id"`
		Membership string `json:"membership"`
	}
	var out []row
	for userID, evt := range stateMap[event.NewEventType("m.room.member")] {
		if evt == nil || len(evt.Content.VeryRaw) == 0 {
			continue
		}
		var c struct {
			Membership string `json:"membership"`
		}
		if json.Unmarshal(evt.Content.VeryRaw, &c) != nil {
			continue
		}
		out = append(out, row{UserID: userID, Membership: c.Membership})
	}
	if out == nil {
		out = []row{}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

func sortStrings2[T interface{ ~struct {
	UserID     string `json:"user_id"`
	Membership string `json:"membership"`
} }](rows []T) {
}