package tool

import (
	"context"
	"testing"

	"github.com/open-mcp-ai/teamcp/internal/actors"
	"github.com/open-mcp-ai/teamcp/internal/auth"
	"github.com/open-mcp-ai/teamcp/internal/store"
)

type stubStore struct {
	store.Store
	principal string
}

type stubActors struct{ byName map[string]string }

func (s stubActors) Get(name string) (store.Store, string, error) {
	return stubStore{principal: s.byName[name]}, name, nil
}
func (s stubActors) Describe() []actors.Info { return nil }
func (s stubActors) Default() string         { return "planner" }
func (s stubActors) PrincipalOf(nameOrMXID string) (string, bool) {
	p, ok := s.byName[nameOrMXID]
	return p, ok
}

// ADR-012 的授权边界：as 只能选自己 principal 名下的身份。
//
// 没有这条，认证只是装饰 —— 任何通过认证的调用方都能用 as 选走别人配置的
// 身份，以那个 AI 的身份说话。
func TestActorSelectionIsBoundToCallerPrincipal(t *testing.T) {
	deps := Deps{Actors: stubActors{byName: map[string]string{
		"planner": "acme", "reviewer": "bob",
	}}}

	caller := auth.WithIdentity(context.Background(), auth.Identity{Principal: "acme"})

	// 自己的身份：放行
	if _, name, bad := storeFor(caller, deps, &Args{As: "planner"}); bad != nil {
		t.Fatalf("own identity refused: %s", bad.Body)
	} else if name != "planner" {
		t.Errorf("resolved %q", name)
	}

	// 别人的身份：拒绝，而且要说清是谁的
	_, _, bad := storeFor(caller, deps, &Args{As: "reviewer"})
	if bad == nil {
		t.Fatal("another principal's identity was accepted")
	}
	if bad.Code != 403 {
		t.Errorf("code = %d, want 403", bad.Code)
	}
	if got := bad.Headers["Error-Code"]; got != "E_ACTOR_NOT_YOURS" {
		t.Errorf("Error-Code = %q", got)
	}

	// 没有人断言 principal 时（open/token 模式）不设限
	if _, _, bad := storeFor(context.Background(), deps, &Args{As: "reviewer"}); bad != nil {
		t.Errorf("unscoped caller refused: %s", bad.Body)
	}
}
