package tool

import (
	"errors"
	"fmt"
	"testing"

	"github.com/open-mcp-ai/teamcp/internal/store"
	"github.com/open-mcp-ai/teamcp/pkg/envelope"
)

// 后端错误的映射：能被区分的东西越多，能探明的东西就越多。
func TestBackendFailureMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		ec   string
	}{
		{"对象不存在", fmt.Errorf("%w：房间 X", store.ErrNotFound), 404, "E_NOT_FOUND"},
		{"身份不被信任", fmt.Errorf("%w：@x:host", store.ErrNotAnActor), 403, "E_NOT_AN_ACTOR"},
		{"上游故障", errors.New("connection refused"), 502, "E_BACKEND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := backendFailure(tc.err)
			if s.Code != tc.code {
				t.Errorf("code = %d, want %d", s.Code, tc.code)
			}
			if got := s.Headers["Error-Code"]; got != tc.ec {
				t.Errorf("Error-Code = %q, want %q", got, tc.ec)
			}
		})
	}
}

// 状态码词表必须覆盖代码真正会发出的每一个码，否则原因短语会渲染成 "Status"。
func TestEveryStatusWeEmitHasAPhrase(t *testing.T) {
	for _, code := range []int{200, 202, 204, 206, 208, 400, 403, 404, 409, 413, 429, 500, 501, 502} {
		line := (envelope.Slot{Code: code}).StatusLine()
		if line == fmt.Sprintf("TEAM/1 %d Status", code) {
			t.Errorf("code %d has no reason phrase: %q", code, line)
		}
	}
}
