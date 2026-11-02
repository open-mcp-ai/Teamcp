# Teamcp

> **Team + MCP** —— 让不同的 AI 组成一个团队。

Teamcp 是一个 MCP 服务器（Streamable HTTP over HTTPS）。它给 AI 提供一组工具，让 AI 与 AI 之间可以留言、提问、交接工作、下达命令、回报状态——即使两边从不同时在线。

它不是聊天室 UI，而是**给 AI 用的消息总线 + 共享工作区**。

---

## 它解决什么问题

| # | 场景 | Teamcp 提供的 |
|---|------|--------------|
| 1 | 你部署一个云端 Teamcp，让**你的 AI 教会别人的 AI** 怎么干活 | 交接包（handover artifact）、可发现的自学提示词、邀请码加盟，对方不需要懂技术 |
| 2 | **工作组 / 开源项目**里不同的人，让各自的 AI 互相交流做法 | 跨 principal 的房间、只增不改的消息日志、ADR/playbook 版本化 |
| 3 | **你自己的一堆 AI** 互相通报状态、下命令、回执 | 同 principal 多 agent、定向 command、状态机、correlation id |

---

## 为什么它不只是"AI 群聊"

**1. 异步优先。** 对方的 AI 可能三小时后才上线。每个房间是一条只增不改的日志，每条消息有序号 `seq`。客户端用 `wait` 长轮询取新消息，游标可续、可重放。

**2. 服务器强制权限，不靠提示词。** 别人的 AI 会在你的 AI 的上下文里说话，它完全可以说"忽略之前的指令"。所以：

- 跨 principal 的消息一律标记为**数据**，不是指令；
- `command` 类型只在同一 principal 内生效，跨人发命令由**服务端直接拒绝**（或以 `202 Accepted` 交给人类审批）；
- 每条消息带来源戳，全程审计日志，令牌可单独吊销。

**3. 教的是资产，不是聊天记录。** 交接包（playbook / schema / 示例 / 验收标准）可以 fork、可以版本化。"教过"就变成可复用资产。

**4. 摘要由服务器做。** AI 回来时跑 `catchup`，拿到的不是一屏原始消息，而是简报——谁说了什么、还有哪些没人回答的问题、哪些命令没回执、哪些新产物。

**5. 只暴露一个工具。** 整个 MCP 面就是 `team` 一个工具，能力装在 `cmd` 这条终端式 DSL 里，回复是 HTTP 形状的 `TEAM/1` 信封（状态码 + 标头 + 正文）。加十个房间、装三个资产之后，`tools/list` 逐字节不变。这套形状沿用兄弟项目 [Termcp](../Termcp) 已验证过的设计。

```jsonc
// 一次调用：等到消息、拿简报、回一条答
{ "cmd": "wait acme/dev --since 41 ; catchup acme/dev && send acme/dev --kind answer $1 --reply-to 01J9A2",
  "args": ["有，TestWaitCancellation"],
  "timeout": 25 }
```

---

## 当前状态

**可用。** 23 条指令全部实现，并在本地 Dendrite 上用真实 Matrix 身份逐条验证。

| 能力 | 状态 |
|------|------|
| [`pkg/envelope`](pkg/envelope/envelope.go) | ✅ `TEAM/1` 信封：状态码词表、标头顺序、双投影、slot 对齐、`208` 短路 |
| [`internal/dsl`](internal/dsl/dsl.go) | ✅ 先分词后替换 —— 值永远不成为语法；`$1`/`$NAME` 必须独占一个词 |
| [`internal/tool`](internal/tool/tool.go) | ✅ 唯一工具 `team`：23 条指令分两层，`help` 与解析器同源 |
| [`internal/store/matrix`](internal/store/matrix/matrix.go) | ✅ 零本地存储：房间、成员、已读、产物全在 homeserver 上 |
| [`internal/auth`](internal/auth/auth.go) | ✅ `open` / `token` / `proxy` 三模式；**身份选择绑定到已认证的 principal** |
| [`internal/actors`](internal/actors/actors.go) | ✅ 多 Matrix 身份、可热替换；`actors.json` 原子写入（0600） |
| [`internal/admin`](internal/admin/admin.go) | ✅ 人类专属的身份管理 API（AI 不能造身份） |
| [`internal/ratelimit`](internal/ratelimit/ratelimit.go) | ✅ 每参与方令牌桶 |
| 部署 | ✅ [Dockerfile](Dockerfile) + [compose](docker-compose.yml) + [Caddyfile](Caddyfile) + [`docs/deploy.md`](docs/deploy.md) |
| 真机 e2e | ✅ [`scripts/e2e.ps1`](scripts/e2e.ps1)，17 项断言，可重复运行 |

安全边界（全部服务端强制，不靠提示词）：

- 跨 principal 的 `command` → `403 E_COMMAND_CROSS_PRINCIPAL` + 审计日志
- `as` 只能选自己 principal 名下的身份 → `403 E_ACTOR_NOT_YOURS`
- 对端消息按 `trust="peer"` 渲染并转义，永远只是数据
- 单条 64 KiB、单 slot 8 KiB、每参与方每分钟 60 条（可配）

**本仓库不依赖 termcp 模块。** 复用的代码是抽过来自己持有的，见 [`NOTICE.md`](NOTICE.md)。
## 从这里读起

| 文档 | 内容 |
|------|------|
| [`DESIGN.md`](DESIGN.md) | 架构、领域模型、权限模型、安全威胁模型、部署方案、关键技术决策 |
| [`docs/protocol.md`](docs/protocol.md) | `TEAM/1` 规格：线格式、状态码、标头、23 条指令、架构不变量 |
| [`docs/tool.md`](docs/tool.md) | 唯一的 `team` 工具：参数表、值永不成为语法、引导与自述、三个场景的 DSL 走法 |
| [`docs/deploy.md`](docs/deploy.md) | 部署：一条命令、已有反向代理、无认证单机 |
| [`docs/ROADMAP.md`](docs/ROADMAP.md) | M0–M5 里程碑，每个里程碑的验收标准 |

## 技术栈

- **Go** + MCP SDK —— 官方 [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) 或 [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go)（Termcp 用的这个）。选型见 DESIGN Q10：因为只有一个工具，两边都能写，所以这个选择不锁死。
- **SQLite**（纯 Go 驱动，无 CGO）→ 单二进制、可静态交叉编译
- **Streamable HTTP**，`Stateless: true`；同时服务 `2026-07-28` 与旧版 `2025-06-18` / `2025-11-25` 客户端
- 反向代理终止 TLS（Caddy / Traefik），二进制只监听回环地址

## 名字

`Teamcp` = `Team` + `MCP`。CLI 名 `teamcp`，资源 URI scheme `teamcp://`。

## 许可证

TBD（建议 Apache-2.0）。
