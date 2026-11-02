// Package auth answers one question per request: WHICH PRINCIPAL is calling.
//
// It is deliberately small because ADR-012 puts authentication outside this
// binary: a reverse proxy authenticates (OIDC, GitHub, mTLS…) and asserts the
// identity in a header. This package only verifies that the assertion came from
// the proxy we expect, and carries the answer to the tool layer.
//
// THREE MODES, and the default is the safest one that still boots:
//
//	proxy  a trusted proxy asserted a principal (the cloud shape)
//	token  one operator token, one trust domain (single-user deployments)
//	open   no authentication at all; only legitimate bound to loopback
//
// THE POINT OF RESOLVING A PRINCIPAL AT ALL is that identity selection must be
// bound to it: without that, any caller who can reach /mcp could say `as` any
// configured identity and speak as someone else's AI.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Mode is how this instance decides who is calling.
type Mode string

const (
	ModeOpen  Mode = "open"
	ModeProxy Mode = "proxy"
	ModeToken Mode = "token"
)

// Config is the authentication configuration.
type Config struct {
	Mode Mode

	// UserHeader is the header the proxy sets to the authenticated principal.
	UserHeader string
	// SecretHeader carries the shared secret that proves the header came from
	// the proxy and not from the client.
	SecretHeader string
	// Secret must match SecretHeader. Empty in proxy mode is a startup error.
	Secret string
	// TrustedProxies is the set of source networks allowed to assert an
	// identity. Empty means "loopback only", which is the narrow default.
	TrustedProxies []netip.Prefix

	// Token is required in token mode.
	Token string
}

// Identity is what the request is allowed to act as.
type Identity struct {
	// Principal is the trust anchor. Empty means "unscoped": the caller may use
	// any configured identity, which is only acceptable in open/token mode.
	Principal string
	// Via names where the assertion came from, for the audit trail.
	Via string
}

type ctxKey struct{}

// WithIdentity attaches an identity to a context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom reads the identity the middleware resolved.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// ErrUnauthenticated is returned when no identity can be established.
var ErrUnauthenticated = errors.New("请求没有通过认证")

// Validate refuses a configuration that would silently run without protection.
//
// PROXY MODE WITHOUT A SECRET IS A STARTUP ERROR, on purpose: it would mean any
// client that can reach the port can claim any principal by setting one header.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeOpen:
		return nil
	case ModeProxy:
		if strings.TrimSpace(c.Secret) == "" {
			return fmt.Errorf("proxy 模式必须配置共享密钥（TEAMCP_PROXY_SECRET）；" +
				"否则任何能连到端口的人都能自称任何 principal")
		}
		if strings.TrimSpace(c.UserHeader) == "" {
			return fmt.Errorf("proxy 模式必须配置用户头（TEAMCP_PROXY_USER_HEADER）")
		}
		return nil
	case ModeToken:
		if strings.TrimSpace(c.Token) == "" {
			return fmt.Errorf("token 模式必须配置 TEAMCP_ADMIN_TOKEN")
		}
		return nil
	default:
		return fmt.Errorf("未知的认证模式 %q（可选 open / proxy / token）", c.Mode)
	}
}

// Middleware resolves the caller's identity before the request reaches a handler.
//
// Every rejection is 401/403 with a short reason and NO detail about what was
// expected — a wrong secret and a missing header must not be distinguishable to
// someone probing the endpoint.
func (c Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := c.resolve(r)
		if err != nil {
			code := http.StatusUnauthorized
			if errors.Is(err, errNotTrusted) {
				code = http.StatusForbidden
			}
			http.Error(w, "未认证", code)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

var errNotTrusted = errors.New("来源不受信任")

func (c Config) resolve(r *http.Request) (Identity, error) {
	switch c.Mode {
	case ModeOpen:
		return Identity{Via: "open"}, nil

	case ModeToken:
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.Token)) != 1 {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{Via: "token"}, nil

	case ModeProxy:
		if !c.trusted(r.RemoteAddr) {
			return Identity{}, errNotTrusted
		}
		secret := r.Header.Get(c.SecretHeader)
		if subtle.ConstantTimeCompare([]byte(secret), []byte(c.Secret)) != 1 {
			return Identity{}, ErrUnauthenticated
		}
		user := strings.TrimSpace(r.Header.Get(c.UserHeader))
		if user == "" {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{Principal: user, Via: "proxy"}, nil
	}
	return Identity{}, ErrUnauthenticated
}

// trusted reports whether a request came from a network allowed to assert an
// identity. An empty list means loopback only.
func (c Config) trusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()

	if len(c.TrustedProxies) == 0 {
		return addr.IsLoopback()
	}
	for _, p := range c.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ParsePrefixes turns "127.0.0.1/32,::1/128" into prefixes.
//
// A bare address is accepted and treated as a single host, because writing /32
// by hand is the kind of detail an operator forgets and then wonders why their
// proxy is refused.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q 既不是 CIDR 也不是地址", part)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}
