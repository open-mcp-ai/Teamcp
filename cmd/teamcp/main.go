// Command teamcp serves the single `team` tool over Streamable HTTP, plus the
// human surface where identities are configured.
//
// TWO AUDIENCES, TWO DOORS:
//
//	/mcp    the AI. It can NAME an identity and use it; it cannot create one.
//	/admin  the human. Accounts, tokens and the default identity live here.
//
// The split is the whole security model of identity management: creating an
// account grows the capability surface (this instance may then speak as that
// participant), so it is a human decision — the same boundary Termcp draws with
// its review gate.
//
// STORAGE IS NOT HERE. The Matrix homeserver owns rooms, the event DAG,
// membership, read receipts and room state. The only local file is actors.json:
// saved configuration, not data.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"fmt"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/open-mcp-ai/teamcp/internal/actors"
	"github.com/open-mcp-ai/teamcp/internal/admin"
	"github.com/open-mcp-ai/teamcp/internal/auth"
	"github.com/open-mcp-ai/teamcp/internal/metrics"
	"github.com/open-mcp-ai/teamcp/internal/ratelimit"
	"github.com/open-mcp-ai/teamcp/internal/tool"
)

// version is stamped at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "监听地址（TLS 由前面的反向代理终止）")
	logLevel := flag.String("log-level", "info", "日志级别：debug / info / warn / error")
	dataDir := flag.String("data-dir", env("TEAMCP_DATA_DIR", "./data"), "配置目录，只存 actors.json，不存消息")
	adminToken := flag.String("admin-token", env("TEAMCP_ADMIN_TOKEN", ""), "管理接口令牌；不配则只允许本机访问 /admin")
	authMode := flag.String("auth-mode", env("TEAMCP_AUTH_MODE", "open"), "认证模式：open / proxy / token")
	userHeader := flag.String("proxy-user-header", env("TEAMCP_PROXY_USER_HEADER", "X-Teamcp-User"), "代理注入的 principal 头")
	secretHeader := flag.String("proxy-secret-header", env("TEAMCP_PROXY_SECRET_HEADER", "X-Teamcp-Secret"), "代理注入的共享密钥头")
	proxySecret := flag.String("proxy-secret", env("TEAMCP_PROXY_SECRET", ""), "共享密钥；proxy 模式下为空则拒绝启动")
	trusted := flag.String("trusted-proxies", env("TEAMCP_TRUSTED_PROXIES", ""), "允许断言身份的来源网段，逗号分隔；空 = 仅回环")
	adminPrincipals := flag.String("admin-principals", env("TEAMCP_ADMIN_PRINCIPALS", ""), "允许使用 /admin 的 principal，逗号分隔；空 = 沿用令牌/本机规则")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	ratePerMin := flag.Int("rate-msg-per-min", envInt("TEAMCP_RATE_MSG_PER_MIN", 60), "每个参与方每分钟最多发多少条；0 表示不限")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	actorsPath := filepath.Join(*dataDir, "actors.json")
	mgr, err := actors.Open(actorsPath)
	if err != nil {
		// A broken actor file must not boot: serving requests that can only fail
		// is worse than refusing, because the failure would surface later as
		// "the AI can't do anything" instead of as a startup error.
		logger.Error("身份配置无效", "path", actorsPath, "err", err)
		os.Exit(1)
	}
	if len(mgr.Describe()) == 0 {
		logger.Warn("还没有配置任何身份：引导与 help 可用，其余指令会回 E_NO_BACKEND",
			"hint", "在 /admin 或 POST /admin/api/actors 添加一个 Matrix 账号")
	}

	prefixes, err := auth.ParsePrefixes(*trusted)
	if err != nil {
		logger.Error("TRUSTED_PROXIES 无法解析", "err", err)
		os.Exit(1)
	}
	authCfg := auth.Config{
		Mode:           auth.Mode(*authMode),
		UserHeader:     *userHeader,
		SecretHeader:   *secretHeader,
		Secret:         *proxySecret,
		TrustedProxies: prefixes,
		Token:          *adminToken,
	}
	if err := authCfg.Validate(); err != nil {
		// 认证配错必须启动失败：带着一个形同虚设的认证跑起来，
		// 比直接不启动危险得多。
		logger.Error("认证配置无效", "err", err)
		os.Exit(1)
	}

	metricsReg := metrics.New()

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "teamcp", Version: version},
		&mcp.ServerOptions{Instructions: tool.Instructions(), Logger: logger},
	)
	tool.Register(srv, tool.Deps{
		Actors: mgr,
		Rate:   ratelimit.New(*ratePerMin),
		// 审计走结构化日志：一条 JSON 一行，能被任何日志系统直接消费。
		Metrics: metricsReg,
		Audit: func(event string, fields map[string]string) {
			args := make([]any, 0, len(fields)*2+2)
			args = append(args, "event", event)
			for k, v := range fields {
				args = append(args, k, v)
			}
			logger.Warn("audit", args...)
		},
	})

	// Stateless: each request is served on its own, with no session to keep
	// alive. A caller that goes away mid-call loses nothing — the state that
	// matters lives in the homeserver.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: logger},
	)

	adm := &admin.Server{
		Actors: mgr, AdminToken: *adminToken, AllowLoopback: true,
		AllowedPrincipals: splitList(*adminPrincipals),
	}

	mux := http.NewServeMux()
	// /mcp 与 /admin 走同一套认证：principal 从代理断言里来，工具层用它约束 as。
	mux.Handle("/mcp", authCfg.Middleware(mcpHandler))
	mux.Handle("/admin/", authCfg.Middleware(adm.Handler()))
	mux.Handle("/metrics", metricsReg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Liveness only: it must not call the homeserver, or a slow Matrix would
		// get a healthy process restarted.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	logger.Info("teamcp listening", "addr", *listen, "version", version, "tools", 1,
		"identities", len(mgr.Describe()), "default", mgr.Default())
	if err := http.ListenAndServe(*listen, mux); err != nil {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
