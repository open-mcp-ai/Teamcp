// Package admin is the HUMAN surface for identity configuration.
//
// It exists because of one boundary: adding a Matrix account grows the
// capability surface — it lets this instance speak as another participant — so it
// is a decision a person makes, never one an AI makes through the DSL. The DSL
// can only NAME an identity (ls agents / as <name>); creating one lives here.
//
// The same split as Termcp's hosts: saved configuration outlives sessions and is
// managed by a human; the AI just uses it.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/teamcp/internal/actors"
	"github.com/open-mcp-ai/teamcp/internal/auth"
)

// Server mounts the identity management API.
//
// Auth is deliberately blunt and minimal until the reverse proxy lands (ADR-012):
// a bearer admin token if one is configured, otherwise loopback only. That is
// enough to keep the door shut on an accidentally exposed instance, and it is
// honest about what it is — not a user login system.
type Server struct {
	Actors *actors.Manager

	// AdminToken, when non-empty, is required on every request.
	AdminToken string

	// AllowLoopback lets an unauthenticated instance be administered from the
	// machine it runs on. With no token and no loopback, every request is
	// refused rather than silently allowed.
	AllowLoopback bool

	// AllowedPrincipals, when non-empty, is the ONLY thing that grants admin
	// rights to a proxy-authenticated caller.
	//
	// Adding an identity grows the capability surface — it lets this instance
	// speak as another participant — so being authenticated is not enough; you
	// have to be an operator. An empty list keeps the old token/loopback rule.
	AllowedPrincipals []string
}

// Handler returns the mux for /admin/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /admin/api/actors", s.guard(s.list))
	mux.HandleFunc("POST /admin/api/actors", s.guard(s.add))
	mux.HandleFunc("DELETE /admin/api/actors/{name}", s.guard(s.remove))
	mux.HandleFunc("POST /admin/api/actors/default", s.guard(s.setDefault))
	mux.HandleFunc("POST /admin/api/actors/{name}/test", s.guard(s.test))

	return mux
}

// actorJSON is the wire shape. It NEVER carries access_token: the console may
// show that an identity exists and which MXID it is, never the secret.
type actorJSON struct {
	Name       string `json:"name"`
	UserID     string `json:"user_id"`
	Homeserver string `json:"homeserver"`
	Default    bool   `json:"default"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	infos := s.Actors.Describe()
	out := make([]actorJSON, 0, len(infos))
	for _, in := range infos {
		out = append(out, actorJSON{Name: in.Name, UserID: in.UserID, Homeserver: in.Homeserver, Default: in.Default})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default": s.Actors.Default(),
		"actors":  out,
	})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Homeserver  string `json:"homeserver"`
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		Default     bool   `json:"default"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	spec := actors.Spec{
		Name:        strings.TrimSpace(body.Name),
		Homeserver:  strings.TrimSpace(body.Homeserver),
		UserID:      strings.TrimSpace(body.UserID),
		AccessToken: strings.TrimSpace(body.AccessToken),
	}
	if err := s.Actors.Add(spec, body.Default); err != nil {
		// Refusals here are usually the namespace rule, so the message is worth
		// showing verbatim in a form.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": spec.Name, "default": s.Actors.Default()})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Actors.Remove(name); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, actors.ErrNoIdentities) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default": s.Actors.Default()})
}

func (s *Server) setDefault(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if err := s.Actors.SetDefault(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default": s.Actors.Default()})
}

// test calls the homeserver's whoami for one identity.
//
// It is what a "测试" button calls, so a bad token is discovered in the form
// rather than at the AI's first message. A test never mutates anything.
func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	actor, err := s.Actors.Test(ctx, name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"principal": actor.Principal,
		"agent":     actor.Agent,
		"kind":      string(actor.Kind),
		"user_id":   actor.UserID,
	})
}

// guard enforces the blunt auth described on Server.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 认证层已经解出 principal 时，管理员权限必须再按名单确认一次。
		if id, ok := auth.IdentityFrom(r.Context()); ok && id.Principal != "" {
			for _, p := range s.AllowedPrincipals {
				if subtle.ConstantTimeCompare([]byte(p), []byte(id.Principal)) == 1 {
					next(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden,
				"这个 principal 没有管理权限；需要 --admin-principals 明确授权。")
			return
		}
		if s.AdminToken != "" {
			h := r.Header.Get("Authorization")
			got := strings.TrimPrefix(h, "Bearer ")
			// Constant time: a token compared with == leaks its prefix by timing.
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.AdminToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "需要管理员令牌")
				return
			}
			next(w, r)
			return
		}

		if s.AllowLoopback && isLoopback(r.RemoteAddr) {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden,
			"未配置管理员令牌，且请求不是来自本机。用 --admin-token 配一个，或从本机操作。")
	}
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
