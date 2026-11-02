package matrix_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"

	"github.com/open-mcp-ai/teamcp/internal/store"
	"github.com/open-mcp-ai/teamcp/internal/store/matrix"
)

func idOf(s string) id.UserID { return id.UserID(s) }

// fakeHomeserver is a stub Client-Server API. It exists so the adapter's real
// code path (build request → parse JSON → map to store types) is exercised
// without a live homeserver, which would make every test a deployment problem.
func fakeHomeserver(t *testing.T, userID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			_, _ = w.Write([]byte(`{"user_id":"` + userID + `","device_id":"DEV"}`))
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			_, _ = w.Write([]byte(`{"joined_rooms":["!dev:host","!onb:host"]}`))
		case strings.Contains(r.URL.Path, "/state/m.room.name"):
			if strings.Contains(r.URL.Path, "%21dev") || strings.Contains(r.URL.Path, "!dev") {
				_, _ = w.Write([]byte(`{"name":"发布流程"}`))
			} else {
				_, _ = w.Write([]byte(`{"name":"交接"}`))
			}
		case strings.Contains(r.URL.Path, "/state/m.room.topic"):
			if strings.Contains(r.URL.Path, "%21dev") || strings.Contains(r.URL.Path, "!dev") {
				_, _ = w.Write([]byte(`{"topic":"发布与联调"}`))
			} else {
				_, _ = w.Write([]byte(`{"topic":"把发布流程交给新人"}`))
			}
		case strings.Contains(r.URL.Path, "/state/m.room.canonical_alias"):
			if strings.Contains(r.URL.Path, "%21dev") || strings.Contains(r.URL.Path, "!dev") {
				_, _ = w.Write([]byte(`{"alias":"#dev:host"}`))
			} else {
				_, _ = w.Write([]byte(`{"alias":"#onb:host"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"not stubbed"}`))
		}
	}))
}

// 命名空间是信任边界：解析必须严格，且失败要能被上层识别成"不被信任"。
func TestParseUserID(t *testing.T) {
	ok := []struct {
		mxid              string
		principal, agent  string
		kind              store.Kind
	}{
		{"@teamcp_acme_a_planner:host", "acme", "planner", store.KindAI},
		{"@teamcp_acme_h_me:host", "acme", "me", store.KindHuman},
		{"@teamcp_bob-x_a_coder-2:matrix.example.org", "bob-x", "coder-2", store.KindAI},
	}
	for _, tc := range ok {
		actor, err := matrix.ParseUserID(idOf(tc.mxid))
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.mxid, err)
			continue
		}
		if actor.Principal != tc.principal || actor.Agent != tc.agent || actor.Kind != tc.kind {
			t.Errorf("%s: got %+v", tc.mxid, actor)
		}
		if actor.UserID != tc.mxid {
			t.Errorf("%s: UserID round-trip = %q", tc.mxid, actor.UserID)
		}
	}

	bad := []string{
		"@alice:host",                    // 不在命名空间：同一个 homeserver 上任何人注册的账号
		"@teamcp_acme_x_planner:host",   // 种类位不是 a/h
		"@teamcp_acme_a:host",           // 少一段
		"@teamcp_acme_a_planner_x:host", // 多一段
		"@teamcp_Acme_a_planner:host",   // 大写不合法
		"@teamcp_acme_a_-bad:host",      // 以 - 开头
		"@teamcp__a_planner:host",       // principal 为空
		"not-an-mxid",
		"@teamcp_acme_a_planner", // 缺域名
	}
	for _, s := range bad {
		_, err := matrix.ParseUserID(idOf(s))
		if err == nil {
			t.Errorf("%s: expected an error", s)
			continue
		}
		if !errors.Is(err, store.ErrNotAnActor) {
			t.Errorf("%s: error must wrap store.ErrNotAnActor so the tool layer can 403 it; got %v", s, err)
		}
	}
}

func TestCapsAreHonest(t *testing.T) {
	st, err := matrix.New("http://127.0.0.1:1", "@teamcp_acme_a_planner:host", "tok")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := st.Caps()
	if caps.ExactTotals {
		t.Errorf("Matrix cannot report an exact total; claiming it would fabricate a Content-Range denominator")
	}
	if caps.CursorKind != "sync-token" {
		t.Errorf("CursorKind = %q, want sync-token", caps.CursorKind)
	}
}

func TestWhoamiAndRooms(t *testing.T) {
	const mxid = "@teamcp_acme_a_planner:host"
	hs := fakeHomeserver(t, mxid)
	defer hs.Close()

	st, err := matrix.New(hs.URL, mxid, "tok")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	actor, err := st.Whoami(ctx)
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if actor.Principal != "acme" || actor.Agent != "planner" || actor.Kind != store.KindAI {
		t.Errorf("actor = %+v", actor)
	}

	rooms, err := st.Rooms(ctx)
	if err != nil {
		t.Fatalf("Rooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("rooms = %d, want 2", len(rooms))
	}
	if rooms[0].Alias != "#dev:host" || rooms[0].Name != "发布流程" || rooms[0].Purpose != "发布与联调" {
		t.Errorf("room[0] = %+v", rooms[0])
	}
	if rooms[0].ID != "!dev:host" {
		t.Errorf("room[0].ID = %q", rooms[0].ID)
	}
}

// 令牌属于另一个账号时必须报错：否则这个 agent 的消息会被记到别人名下。
func TestWhoamiRejectsMismatchedToken(t *testing.T) {
	hs := fakeHomeserver(t, "@teamcp_other_a_someone:host")
	defer hs.Close()

	st, err := matrix.New(hs.URL, "@teamcp_acme_a_planner:host", "tok")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Whoami(context.Background()); err == nil {
		t.Fatalf("a token belonging to another account was accepted")
	}
}

// 启动即校验命名空间：配错了要在这里就失败，而不是等第一次调用。
func TestNewRejectsNonNamespacedUser(t *testing.T) {
	if _, err := matrix.New("http://127.0.0.1:1", "@random:host", "tok"); err == nil {
		t.Fatalf("a non-namespaced MXID was accepted")
	} else if !errors.Is(err, store.ErrNotAnActor) {
		t.Errorf("error must wrap store.ErrNotAnActor, got %v", err)
	}
}
