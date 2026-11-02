# Teamcp 路线图

状态：**草案 v0.1**
原则：**每个里程碑结束时，都能用真实 MCP 客户端跑通一条完整路径。** 不做"什么都写了一半"。

---

## 里程碑总览

| ID | 名称 | 规模 | 依赖 | 结束时能做什么 |
|----|------|------|------|--------------|
| M0 | 规格冻结 | S | — | 设计文档评审通过，可以动手 |
| M1 | 骨架与身份 | M | M0 | 服务器起得来，能连上，知道"我是谁" |
| M2 | 消息核心 | L | M1 | 两个 AI 能真的互聊（含长轮询） |
| M3 | 跨人协作与权限 | L | M2 | 不同人的 AI 能在房间里协作，注入被拦住 |
| M4 | 教学资产与上手 | M | M3 | 非技术用户能一条命令接入并学会 |
| M5 | 运维与发布 | M | M4 | 公网单实例可长期运行 |

```mermaid
graph LR
  M0[M0 规格] --> M1[M1 骨架]
  M1 --> M2[M2 消息核心]
  M2 --> M3[M3 权限]
  M3 --> M4[M4 教学]
  M4 --> M5[M5 发布]
```

---

## M0 · 规格冻结

**目标**：把设计定下来，避免边写边改数据模型。

**交付物**

- `README.md` / `DESIGN.md` / `docs/protocol.md` / `docs/tool.md` / `docs/ROADMAP.md`（本文件）
- 逐条确认 DESIGN §14 的开放问题（至少 Q1、Q2、Q7 要有结论）
- 14 条指令的语法、flag，以及 `team` 工具的 9 个参数，由**一张声明表**定稿（还没代码）

**验收标准**

- [ ] 三个场景各有 ≥1 条端到端用例（见 `docs/tool.md` §7 与 DESIGN §10）
- [ ] 权限矩阵每一格都有明确答案，没有"待定"
- [ ] 数据模型能表达全部用例，不需要加表
- [ ] 与至少一个真实宿主客户端（Claude Desktop / 自建 Go client）确认过协议兼容路径

**明确不做**：写任何业务代码。

---

## M1 · 骨架与身份

**目标**：一个能跑、能连、能认证的空壳。

**交付物**

| 模块 | 内容 |
|------|------|
| `cmd/teamcp` | 子命令：`serve`、`migrate`、`agent create`、`invite create`、`onboard` |
| `internal/config` | 环境变量 + 可选 YAML，启动即校验 |
| `internal/store` | SQLite（`modernc.org/sqlite`）+ `embed.FS` 迁移 + `schema_version` |
| `internal/auth` | 令牌签发/校验、principal 令牌、admin 令牌、`token_hint` |
| `internal/envelope` | `TEAM/1` 渲染器：状态码表、标头渲染顺序、`Text()` / `Structured()` 双投影 |
| `internal/directive` | DSL 解析器：用 `mvdan.cc/sh/v3/syntax` 做词法分析（与 Termcp 同一套），先只支持 `whoami` / `help` / 空 `cmd` |
| `internal/args` | **参数声明表**：`cmd` + 9 个参数的名称/类型/默认/说明，schema 与 `/api.md` 都从它渲染 |
| `internal/mcpserver` | `NewStreamableHTTPHandler`，`Stateless: true`，注册**一个**工具 `team` |
| `internal/httpx` | `/healthz` `/readyz` `/metrics` `/api/team` `/api.md`、request_id、panic recover、日志脱敏 |
| 集成测试 | 用 MCP client 连 `httptest.Server` 打通 `initialize → tools/list → tools/call` |

**验收标准**

- [ ] `go build ./...` 产出**单文件静态二进制**（`CGO_ENABLED=0`），注入 `docker scratch` 能跑
- [ ] 空 DB 启动自动建表；重复启动幂等
- [ ] `teamcp agent create --name planner` 打印一次令牌；DB 里只有 sha256
- [ ] 用该令牌调 `team(cmd="whoami")` 返回正确 principal/agent
- [ ] **`tools/list` 只有一个工具 `team`**，schema 字节数等于一个常量
- [ ] 空 `cmd` 返回引导且**永不报错**；`help` 与 `help whoami` 各返回一条 `200` slot
- [ ] 一条带语法错误的 `cmd` 返回 `400` + `Error-Code` + `Usage`，且**没有任何语句被执行**
- [ ] `POST /api/team`（curl 便利入口）与 MCP `team(cmd=…)` 对同一条指令返回同一个信封（一个解析器，两个入口）
- [ ] 用无效令牌 → `403` + `E_UNAUTHENTICATED`；吊销后同样
- [ ] **协议兼容实测**：`2026-07-28` 与 `2025-06-18` 两种客户端都能完成 `tools/list` + `tools/call`（这是 DESIGN §9.2 的关键假设，必须在此刻验证，失败则回头改传输层方案）
- [ ] 日志里搜不到任何明文令牌（测试断言）
- [ ] `/metrics` 有内容，`/healthz` 在 DB 损坏时仍 200，`/readyz` 则 503

**风险**

- 🔴 **DSL 解析器是新的最大单体**。Termcp 的等价物是 61 KB 的 `parse.go` 加 25 KB 的 `directive.go`，而且是被几十个测试一轮轮打出来的。M1 只做三条指令就是为了把语法骨架（target 位置规则、flag 只在 target 之后、值引用、`;`/`&&` 组合）先定下来，**不要**在 M1 就铺满 14 条。
- ✅ **`Stateless: true` 下旧版客户端能工作——已实测，不再是风险。** 同一个 `Stateless: true` 的
  handler 同时服务 `2025-06-18` 与 `2026-07-28`：前者只带 `MCP-Protocol-Version` 头就能调通，
  后者按 SEP-2575 要求每个 POST 自带 `Mcp-Method` / `Mcp-Name` 头与完整 `_meta`
  （`protocolVersion` + `clientCapabilities` + `clientInfo`），三条 RPC（`server/discover`、
  `tools/list`、`tools/call`）全部 200。`server/discover` 自报支持
  `2026-07-28 / 2025-11-25 / 2025-06-18 / 2025-03-26 / 2024-11-05`。
  原来的退路（`Stateless: false` + 进程内会话）不需要了。
- 🟡 `modernc.org/sqlite` 的并发写行为与 `database/sql` 连接池配置——需要在 M1 就压测一次（`_txlock=immediate`）。
- 🟡 **参数表和 schema 的漂移**：三处读方（schema、`/api.md`、解析器默认值）必须从同一张表渲染，并有一条测试比对。Termcp 的 `term_args.go` 注释记录了漂移真的发生过。

---

## M2 · 消息核心

**目标**：两个 AI 能真的互聊。这是整个项目的"心跳"。

**交付物**

| 模块 | 内容 |
|------|------|
| `internal/domain` | `Room` / `Message` / 不变量 I1–I7 |
| `internal/service/room` | 建房间、`last_seq` 原子递增（事务 + `UPDATE ... RETURNING`） |
| `internal/service/message` | `send` / `read` / `catchup` / `ack` |
| `internal/wait` | **waiter 注册表**：`Register(roomID) → chan`，`Broadcast(roomID)`，`Unregister`；`max_concurrent_waiters` 背压 |
| `internal/directive` | 铺满消息类指令：`ls` `get` `read` `wait` `catchup` `send` `ack` `new room` |
| 消息渲染 | `<teamcp-message>` XML 包裹 + `trust` 横幅 + 8 KiB 截断 |
| 引导与 help | 从指令表渲染空 `cmd` 引导、`help`、`help <verb>` |
| e2e 测试 | `docs/tool.md` §7 的完整往返，两个 client 并发 |

**验收标准**

- [ ] `team(cmd="wait acme/dev --since 41", timeout=25)` 在对方发消息后 **< 50ms** 返回 `200`；无事则准时 `204` + `Timed-Out: true` + `Next-Seq`
- [ ] 同一房间 100 并发 send，`seq` 严格连续无洞（`1..100`）
- [ ] `wait` 在客户端断开后 **1 秒内**释放 waiter（用 `goleak` 断言无泄漏）
- [ ] `timeout=0` 立即返回（有消息 `200`，没有 `204` + `Hint`）；`timeout=300` 不被拒绝，`Waited` 报告实际时长
- [ ] `catchup` 正确识别：未答 `ask`、未回执 `command`、新 `artifact`
- [ ] 单条 64 KiB 消息成功；64 KiB+1 返回 `413` + `E_TOO_LARGE` 并提示改用 `put art`
- [ ] 任意 slot 正文 ≤ 8 KiB，超出时 `Truncated` + `Content-Range`
- [ ] `wait` 重复调用同一 `--since` 不重复消费、不报错
- [ ] **正文永不进 DSL**：`send acme/dev --kind ask 你好` 返回 `400 E_AMBIGUOUS_PAYLOAD`；对端消息体里含 `; rm -rf /` 时只作为数据渲染，不产生第二个 slot
- [ ] 复合命令：`a ; b && c` 生成 3 个 slot；`a` 失败时 `c` 是 `208` 且 slot 下标仍对齐
- [ ] 压测：500 并发 waiter + 10 msg/s 持续 5 分钟，内存平稳、无 goroutine 增长

**风险**

- 🔴 **waiter 泄漏**：这是最容易出隐蔽 bug 的地方（取消、超时、panic 三条路径都要 `defer Unregister`）。用 `goleak` + 每路径单测硬顶。
- 🔴 **`send` 的 payload 校验松了**：一旦允许正文直接写进 `cmd`，ADR-011 就破了，而且是**不可逆地**破——因为协议一旦接受这个形状，就不能再收紧。这条要一个专门的负向测试。
- 🟡 **`seq` 空洞**：若在事务里先取 `last_seq` 再插入，并发下会跳号。必须用单条 `UPDATE room SET last_seq = last_seq + 1 WHERE id = ? RETURNING last_seq`。
- 🟡 长轮询与反向代理的 idle timeout：写进部署文档（Caddy `flush_interval -1`），M5 验证。

---

## M3 · 跨人协作与权限

**目标**：不同人的 AI 能安全地待在一个房间。这是"这个项目安全吗"的答案所在。

**交付物**

| 模块 | 内容 |
|------|------|
| `internal/domain` | `Principal` / `Team` / `TeamMember` / `RoomMember` / `Invite` |
| `internal/acl` | 唯一入口 `Authorize(actor, action, target)`；默认拒绝；越权读返回 NotFound |
| `internal/service/team` | team 创建、成员管理、角色变更 |
| `internal/service/invite` | 高熵短码、使用计数、过期、吊销、失败限速 |
| `internal/directive` | 铺满组织类指令：`join` `new agent` `new invite` `new team` `close` `rm` |
| 权限接线 | `authorize` 作为**每个指令 handler 的第一个非平凡语句**；`send --kind command` 接入 `command_policy` |
| 注入防御 | 跨 principal `command` 硬拒 + `audit_log` 记录 + `trust` 渲染 |
| **注入回归测试套件** | `testdata/injection/*.txt`，每条是一个攻击载荷，断言被拦截或降权 |
| `internal/audit` | 只追加审计日志 |

**验收标准**

- [ ] 权限矩阵（DESIGN §7.3）**逐格**有测试，共 ≥ 7 动作 × 5 角色
- [ ] 跨 principal `command` → `403` + `E_COMMAND_CROSS_PRINCIPAL`，且 `audit_log` 有记录
- [ ] `command_policy=approval` 的房间：跨 principal `command` → `202` + `Pending-ID`，且消息**未**入库为已发送
- [ ] 非成员读房间 → `E_NOT_FOUND`（**不是** `E_FORBIDDEN`），且响应时间与"不存在"无显著差异
- [ ] 邀请码：用尽 → `E_INVITE_EXHAUSTED`；过期 → `E_INVITE_INVALID`；并发兑换同一码 `max_uses=1` 只成功一次
- [ ] 注入套件全绿，至少覆盖：伪造系统指令、伪造 `from` 字段、要求执行命令、伪造已读回执、试图让 AI 泄露其他房间内容
- [ ] `observer` 角色只能读，任何写操作 `E_FORBIDDEN`
- [ ] 吊销 agent 后其所有请求立刻 `E_UNAUTHENTICATED`（无缓存窗口）

**风险**

- 🔴 **把 ACL 判断写在指令 handler 里**：一旦分散就必然漏。强制约定：**每个指令 handler 的第一个非平凡语句必须是 `authorize`**，用一个 AST 检查测试来保证（这条比原方案更容易做到——指令 handler 是集中注册的）。
- 🟡 邀请码的时序侧信道——并发兑换用 `UPDATE ... WHERE used < max_uses` 的原子性解决，不先读后写。

---

## M4 · 教学资产与上手

**目标**：让"别人不怎么会用电脑"这件事真的成立。

**交付物**

| 模块 | 内容 |
|------|------|
| `internal/service/artifact` | 按 slug 版本化、`content_sha`、`supersedes`、1 MiB 上限 |
| `internal/directive` | 铺满资产类指令：`put art` `get art` `get <room> brief` `get <room> message` |
| 引导与 help | 引导里列出房间用途与 `Next:` 建议；`help <verb>` 覆盖全部 14 条 |
| `teamcp onboard` | 输出一段可直接粘贴的 MCP 客户端配置 JSON（含 URL + 令牌 + 一句起始指令） |
| `internal/console` | 极简网页：注册 agent、发邀请码、看房间列表、审批跨 principal 的 `command`。单个 `embed` 的 HTML，无前端构建链 |

**验收标准**

- [ ] `teamcp onboard --name bob-ai` 输出可粘贴配置；贴进真实客户端后 AI **无需人类再解释**就能完成 `whoami → catchup`（靠空 `cmd` 的引导，不是靠文档）
- [ ] 场景 A 的完整交接流程（DESIGN §10）在 e2e 测试里跑通
- [ ] artifact 版本化正确：同 slug 连续 `put art` 得到 v1/v2/v3；`get art` 省略 `--version` 返回最新；版本列表完整
- [ ] `handover` 类 artifact 缺少四节结构时给出**警告**（不拒绝）——引导而不强制
- [ ] console 页面能在手机浏览器里完成"发一个邀请码"（非技术用户真实场景）
- [ ] **人类审批闭环**：`202` → console 批准 → 裁决以 `system` 消息进房间 → 发送方 AI 的 `wait` 返回它
- [ ] `help` 里的每一条示例都能被解析器解析并执行到预期的状态码（`spec_drift_test` 式的断言）

**风险**

- 🟡 console 是范围内最容易长成"半个聊天 UI"的东西。**硬约束：console 只允许 4 个页面，不允许出现消息正文**（审批卡只显示元数据：谁、对谁、什么 kind、`correlation`）。
- 🟡 **审批裁决必须走消息通道**，不能只落在网页上。Termcp 的原则在这里同样成立：投递不是确认。只在网页上弹过的批准，AI 是不知道的。
- 🟡 引导是每轮都可能进上下文的东西，**必须有一屏预算的测试**（字节数断言），否则它会慢慢长成手册。

---

## M5 · 运维与发布

**目标**：能在公网跑三个月不炸。

**交付物**

| 模块 | 内容 |
|------|------|
| 容器 | 多阶段构建 → `scratch`/`distroless`；`docker compose` 含 Caddy；非 root 运行 |
| 备份 | Litestream 默认开启，文档写清恢复演练步骤 |
| 观测 | Prometheus 指标齐全；OTel trace 透传（`_meta` 的 `traceparent`）；`slog` JSON |
| 限速 | 每 agent 令牌桶；每 IP 粗限速在 Caddy 层 |
| 保留策略 | `--retention-days`（默认 0 = 永久） |
| 文档 | 部署指南、故障排查、升级与回滚、威胁模型摘要（给非技术读者的一页版） |
| 压测报告 | 数据写进 `docs/`，作为容量基线 |

**验收标准**

- [ ] 全新机器到可用服务 ≤ 5 分钟，只需 `docker compose up -d` + 一个环境变量文件
- [ ] 进程被 `kill -9` 后重启，数据完整，`seq` 无异常
- [ ] Litestream 恢复演练：从对象存储恢复出一个可工作的实例
- [ ] 压测：500 并发 waiter + 1000 msg/s 写入 10 分钟，P99 `wait` < 100ms
- [ ] 长轮询穿过 Caddy 不被断开（需 `flush_interval -1` 或等价配置，写进默认配置）
- [ ] 所有日志中的令牌、消息正文可脱敏开关
- [ ] 安全自检清单（OWASP 风格）逐项打勾

---

## 测试策略

| 层 | 覆盖对象 | 手段 |
|----|---------|------|
| 单元 | ACL 矩阵、不变量、游标、摘要算法 | 表驱动测试，一个格子一个用例 |
| 集成 | store ↔ service ↔ API | 真 SQLite（临时文件），不用 mock |
| **e2e** | 完整 MCP 往返 | 官方 `mcp.Client` × N，跑真实 `httptest` 服务器 |
| **注入回归** | 提示词攻击载荷 | `testdata/injection/`，每次改动全部重跑 |
| 并发 | waiter、`seq`、邀请兑换 | `-race` + `goleak` + 500 并发 |
| 模糊 | 工具参数、消息渲染 | `go test -fuzz` 于 JSON 解码与 XML 转义（**必须防 XML 注入**） |
| 压测 | 容量基线 | `k6` 或自带 Go benchmark |

**全局 DoD（Definition of Done）**：每个里程碑合并前必须满足

1. `go vet` + `golangci-lint` 干净
2. `go test -race ./...` 全绿
3. 新增的每个工具都有：单元测试 + e2e 用例 + 错误路径测试
4. 相关文档同步更新（`help` 表、`docs/protocol.md`、`docs/tool.md`）
5. 没有 `TODO` 遗留在权限和存储代码里

---

## 刻意推迟的东西

按"什么时候才值得做"排序，避免过度设计：

| 功能 | 推迟到 | 触发条件 |
|------|--------|---------|
| OAuth 2.0 资源服务器（`oauth-protected-resource`） | 决定做 SaaS 时 | 出现"不能给用户手发令牌"的场景 |
| Postgres + `LISTEN/NOTIFY` | 单实例撑不住时 | >1000 日活 agent 或需要多副本 |
| `tasks` 扩展（超长等待） | v0.3 | 客户端支持度确认后 |
| 跨房间内容相似度检测（防搬运） | v0.3 | 出现真实违规案例 |
| 小模型摘要 | v0.4 | 规则式简报被反馈"不够用" |
| 人类参与桥（IM / 邮件） | v0.4 | 场景 A 里人类想旁观 |
| 端到端加密 | 不做 | 与服务器摘要根本冲突 |
| 消息全文搜索 | v0.3 | SQLite FTS5，低成本高收益 |

---

## 开放问题（阻塞 M0 收尾）

- **Q1 部署形态**：自部署为主还是公网 SaaS？→ 决定认证路线（邀请码 vs OAuth）与滥用防护投入。
- **Q2 `tasks` 扩展**：M1 做完协议实测后，评估客户端支持度再定。
- **Q7 许可证**：Apache-2.0 与 MCP 生态一致，若无异议就定这个。
- **Q9（新增）** 是否需要"房间只读镜像"（`share_policy=public` 时给外部只读访问）？影响是否要做第二套公开端点。
