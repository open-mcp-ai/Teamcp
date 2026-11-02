// The verbs that pass almost straight through to a Matrix call, plus join/ack/
// get/rm which are thin policy around one.
//
// They are here rather than in tool.go's switch because that switch is the
// high-level semantic core; this file is the translation layer, and the two have
// different reasons to change.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/teamcp/internal/store"
	"github.com/open-mcp-ai/teamcp/pkg/envelope"
)

// executeExtra handles every verb that is not in the high-level core.
//
// Returning ok=false lets the caller fall through to the honest 501, so a verb
// that is listed but not wired still says so instead of vanishing.
func executeExtra(ctx context.Context, d Deps, a *Args, st Statement) (envelope.Slot, bool) {
	need := func(n int, usage string) (envelope.Slot, bool) {
		if len(st.Args) >= n {
			return envelope.Slot{}, true
		}
		return withHint(failure(400, "E_MISSING_TARGET",
			fmt.Sprintf("%s 至少需要 %d 个位置参数，收到 %d 个。", st.Verb, n, len(st.Args))), usage), false
	}

	storeOrFail := func() (store.Store, string, *envelope.Slot) { return storeFor(ctx, d, a) }

	switch st.Verb {
	case "join":
		if s, ok := need(1, "join <room-or-alias>"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		rid, err := sto.Join(ctx, st.Args[0])
		if err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Room": rid, "Next": "read " + rid},
			Body:    "已加入 " + rid + "。",
		}, true

	case "ack", "receipt":
		if s, ok := need(1, "ack <room> [<event-id>]"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		eventID := ""
		if len(st.Args) > 1 {
			eventID = st.Args[1]
		}
		acked, err := sto.Ack(ctx, st.Args[0], eventID)
		if err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Room": st.Args[0], "Acked": acked, "Next-Seq": "msg:" + acked},
			Body:    "已回执到 " + acked + "（只影响当前身份）。",
		}, true

	case "get":
		if s, ok := need(1, "get <room> brief | get <room> message <id> | get art <slug>"); !ok {
			return s, true
		}
		if st.Args[0] == "art" {
			sto, _, bad := storeOrFail()
			if bad != nil {
				return *bad, true
			}
			if len(st.Args) < 2 {
				out, err := sto.ListArtifacts(ctx, st.Flag("room"))
				if err != nil {
					return backendFailure(err), true
				}
				return envelope.Slot{Code: 200, Body: out}, true
			}
			out, err := sto.GetArtifact(ctx, st.Flag("room"), st.Args[1])
			if err != nil {
				return backendFailure(err), true
			}
			return envelope.Slot{Code: 200, Headers: map[string]string{"Slug": st.Args[1]}, Body: out}, true
		}
		if len(st.Args) < 2 {
			return withHint(failure(400, "E_MISSING_TARGET", "get 需要说明读什么。"),
				"get <room> brief | get <room> message <id>"), true
		}
		switch st.Args[1] {
		case "brief":
			return catchupSlot(ctx, d, a, st), true
		case "message":
			if len(st.Args) < 3 {
				return withHint(failure(400, "E_MISSING_TARGET", "get message 需要事件 id。"),
					"get <room> message <id>"), true
			}
			st2, _, bad := storeOrFail()
			if bad != nil {
				return *bad, true
			}
			m, err := st2.Event(ctx, st.Args[0], st.Args[2])
			if err != nil {
				return backendFailure(err), true
			}
			return envelope.Slot{
				Code:    200,
				Headers: map[string]string{"Room": st.Args[0], "Event": m.ID},
				Body:    renderMessages([]store.Message{m}, actingPrincipal(d, a)),
			}, true
		default:
			return withHint(failure(400, "E_UNKNOWN_TARGET",
				fmt.Sprintf("get 不认识 %q。", st.Args[1])), "可选：brief / message / art"), true
		}

	case "state":
		if s, ok := need(1, "state <room> [<type> [<state-key>]]"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		typ, key := "", ""
		if len(st.Args) > 1 {
			typ = st.Args[1]
		}
		if len(st.Args) > 2 {
			key = st.Args[2]
		}
		out, err := sto.State(ctx, st.Args[0], typ, key)
		if err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{"Room": st.Args[0]}, Body: out}, true

	case "redact":
		if s, ok := need(2, "redact <room> <event-id> [--reason $1]"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		if err := sto.Redact(ctx, st.Args[0], st.Args[1], st.Flag("reason")); err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{Code: 200, Body: "已撤回 " + st.Args[1] + "（墓碑，不抹除）。"}, true

	case "invite":
		if s, ok := need(2, "invite <room> <mxid>"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		if err := sto.Invite(ctx, st.Args[0], st.Args[1]); err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{Code: 200, Body: "已邀请 " + st.Args[1] + "。"}, true

	case "kick":
		if s, ok := need(2, "kick <room> <mxid> [--reason $1]"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		if err := sto.Kick(ctx, st.Args[0], st.Args[1], st.Flag("reason")); err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{Code: 200, Body: "已把 " + st.Args[1] + " 移出。"}, true

	case "rm":
		// rm <room> member <mxid> —— 与 kick 同义，但按 DSL 的地址形状书写。
		if len(st.Args) >= 3 && st.Args[1] == "member" {
			sto, _, bad := storeOrFail()
			if bad != nil {
				return *bad, true
			}
			if err := sto.Kick(ctx, st.Args[0], st.Args[2], st.Flag("reason")); err != nil {
				return backendFailure(err), true
			}
			return envelope.Slot{Code: 200, Body: "已把 " + st.Args[2] + " 移出 " + st.Args[0] + "。"}, true
		}
		return withHint(failure(501, "E_NOT_IMPLEMENTED", "rm 目前只实现了 rm <room> member <mxid>。"),
			"产物删除需要产物存储，尚未设计。"), true

	case "create", "new":
		// create room … 与 new room … 是同一个动作的两种拼法：前者是 Matrix 的
		// 原生词汇，后者是 DSL 的高层词汇。
		args := st.Args
		if len(args) == 0 {
			return withHint(failure(400, "E_MISSING_TARGET", "create/new 需要说明建什么。"),
				"new room <slug> --purpose $1  |  create room --alias <localpart> --topic $1"), true
		}
		if args[0] == "invite" {
			// Matrix 没有邀请码：邀请是定向的（邀请一个 MXID）。所以不发明短码，
			// 而是要求 --to，把差异说清楚。
			sto, _, bad := storeOrFail()
			if bad != nil {
				return *bad, true
			}
			to := st.Flag("to")
			if to == "" {
				return withHint(failure(400, "E_MISSING_TARGET",
					"Matrix 的邀请是定向的：要指定被邀请的 MXID，不存在可分享的邀请码。"),
					"new invite <room> --to @teamcp_bob_a_reviewer:host"), true
			}
			if len(args) < 2 {
				return withHint(failure(400, "E_MISSING_TARGET", "new invite 需要房间。"),
					"new invite <room> --to <mxid>"), true
			}
			if err := sto.Invite(ctx, args[1], to); err != nil {
				return backendFailure(err), true
			}
			return envelope.Slot{Code: 200, Body: "已邀请 " + to + " 进 " + args[1] + "。"}, true
		}
		if args[0] != "room" {
			return withHint(failure(501, "E_NOT_IMPLEMENTED",
				fmt.Sprintf("%s %s 还没有实现。", st.Verb, args[0])),
				"本版本只实现了建房间。"), true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		slug := ""
		if len(args) > 1 {
			slug = args[1]
		}
		if slug == "" {
			slug = st.Flag("alias")
		}
		name := st.Flag("title")
		if name == "" {
			name = slug
		}
		rid, err := sto.CreateRoom(ctx, name, st.Flag("purpose")+st.Flag("topic"), slug, nil)
		if err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Room": rid, "Next": "read " + rid},
			Body:    "已建立房间 " + rid + "。",
		}, true

	case "messages":
		// read 的底版：同一个窗口，但不做渲染，直接给结构化投影。
		if s, ok := need(1, "messages <room> [--since <cursor>] [--limit <n>]"); !ok {
			return s, true
		}
		st2, _, bad := storeFor(ctx, d, a)
		if bad != nil {
			return *bad, true
		}
		page, err := st2.Messages(ctx, st.Args[0], strings.TrimPrefix(st.Flag("since"), "msg:"), 20)
		if err != nil {
			return backendFailure(err), true
		}
		b, _ := json.MarshalIndent(page, "", "  ")
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Room": st.Args[0], "Next-Seq": "msg:" + page.Next},
			Body:    string(b),
		}, true

	case "put":
		// put art <slug> --kind <k> $1 [--room <room>] [--title $2]
		if len(st.Args) >= 2 && st.Args[0] == "art" {
			sto, _, bad := storeOrFail()
			if bad != nil {
				return *bad, true
			}
			body := ""
			if len(st.Args) > 2 {
				body = st.Args[2]
			}
			if body == "" {
				return withHint(failure(400, "E_MISSING_BODY", "产物正文必须作为值传入。"),
					"put art <slug> --kind <k> $1 --room <room>"), true
			}
			// 1 MiB：协议里的产物上限。之前文档写了、代码没检查 —— 那是最糟的组合，
			// 因为调用方会以为有保护。产物是 state event，超大正文会把**每次**
			// state 读取一起拖慢，不只是这一次写入。
			const maxArtifact = 1 << 20
			if len(body) > maxArtifact {
				return withHint(failure(413, "E_TOO_LARGE",
					fmt.Sprintf("产物正文 %d 字节，超过上限 %d。", len(body), maxArtifact)),
					"把交接包拆成几份产物，或用 GitHub Gist 之类的外部存储并在正文里引用链接。"), true
			}

			kind := st.Flag("kind")
			if kind == "" {
				kind = "doc"
			}
			title := st.Flag("title")
			if title == "" {
				title = st.Args[1]
			}
			ver, err := sto.PutArtifact(ctx, st.Flag("room"), st.Args[1], kind, title, body)
			if err != nil {
				return backendFailure(err), true
			}
			return envelope.Slot{
				Code:    200,
				Headers: map[string]string{"Slug": st.Args[1], "Version": fmt.Sprint(ver)},
				Body:    fmt.Sprintf("已写入产物 %s v%d。", st.Args[1], ver),
			}, true
		}
		return withHint(failure(501, "E_NOT_IMPLEMENTED", "put 目前只实现了 put art。"),
			"put art <slug> --kind <k> $1 --room <room>"), true

	case "close":
		if s, ok := need(1, "close <room>"); !ok {
			return s, true
		}
		sto, _, bad := storeOrFail()
		if bad != nil {
			return *bad, true
		}
		if err := sto.Leave(ctx, st.Args[0]); err != nil {
			return backendFailure(err), true
		}
		return envelope.Slot{Code: 200, Body: "已离开 " + st.Args[0] + "；历史消息仍在。"}, true

	case "sync":
		// wait 的底版：返回同步窗口与新的同步游标。
		if s, ok := need(1, "sync <room> [--since <sync-cursor>]"); !ok {
			return s, true
		}
		st2, _, bad := storeFor(ctx, d, a)
		if bad != nil {
			return *bad, true
		}
		page, err := st2.Sync(ctx, st.Args[0], strings.TrimPrefix(st.Flag("since"), "sync:"), int(a.Timeout))
		if err != nil {
			return backendFailure(err), true
		}
		b, _ := json.MarshalIndent(page, "", "  ")
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Room": st.Args[0], "Next-Seq": "sync:" + page.Next},
			Body:    string(b),
		}, true
	}

	return envelope.Slot{}, false
}
