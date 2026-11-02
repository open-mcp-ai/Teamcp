package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/teamcp/internal/auth"
)

func serve(cfg auth.Config, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	cfg.Middleware(h).ServeHTTP(w, req)
	return w
}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.IdentityFrom(r.Context())
		_, _ = w.Write([]byte(id.Principal))
	})
}

func TestProxyModeRequiresSecretAndIdentity(t *testing.T) {
	cfg := auth.Config{
		Mode: auth.ModeProxy, UserHeader: "X-Teamcp-User",
		SecretHeader: "X-Teamcp-Secret", Secret: "s3cret",
	}
	newReq := func(user, secret, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.RemoteAddr = remote
		if user != "" {
			r.Header.Set("X-Teamcp-User", user)
		}
		if secret != "" {
			r.Header.Set("X-Teamcp-Secret", secret)
		}
		return r
	}

	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"正确断言", newReq("alice", "s3cret", "127.0.0.1:1234"), 200},
		{"缺用户头", newReq("", "s3cret", "127.0.0.1:1234"), 401},
		{"密钥错", newReq("alice", "wrong", "127.0.0.1:1234"), 401},
		{"来源不受信任", newReq("alice", "s3cret", "10.9.9.9:1234"), 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(cfg, ok(), tc.req)
			if w.Code != tc.status {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 200 && w.Body.String() != "alice" {
				t.Errorf("principal = %q, want alice", w.Body.String())
			}
		})
	}
}

// 一个没有密钥的 proxy 配置必须拒绝启动：否则任何能连端口的人都能自称任何 principal。
func TestProxyModeWithoutSecretRefusesToStart(t *testing.T) {
	cfg := auth.Config{Mode: auth.ModeProxy, UserHeader: "X-Teamcp-User"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty secret in proxy mode was accepted")
	}
}

func TestTokenMode(t *testing.T) {
	cfg := auth.Config{Mode: auth.ModeToken, Token: "tok"}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if w := serve(cfg, ok(), r); w.Code != 401 {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	r.Header.Set("Authorization", "Bearer tok")
	if w := serve(cfg, ok(), r); w.Code != 200 {
		t.Errorf("with token: code = %d, want 200", w.Code)
	}
}

func TestParsePrefixes(t *testing.T) {
	ps, err := auth.ParsePrefixes("127.0.0.1/32, ::1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("got %d prefixes", len(ps))
	}
	if _, err := auth.ParsePrefixes("not-an-addr"); err == nil {
		t.Error("garbage was accepted")
	}
}
