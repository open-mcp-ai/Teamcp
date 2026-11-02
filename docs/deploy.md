# 部署

三种形态，按你手上的东西选。

---

## 形态一：一条命令（推荐给自用 / 小团队）

```bash
git clone https://github.com/open-mcp-ai/Teamcp && cd Teamcp
cp .env.example .env      # 填 TEAMCP_DOMAIN、TEAMCP_PROXY_SECRET、TEAMCP_BASIC_HASH
docker compose up -d
```

它起两个容器：`teamcp`（不对外暴露端口）和 `caddy`（TLS + 认证）。Caddy 验证用户后
把 `X-Teamcp-User` 和共享密钥转发给 Teamcp——这就是 ADR-012 的"认证在外面"。

生成凭据：

```bash
# 密码哈希（写进 .env 的 TEAMCP_BASIC_HASH）
docker run --rm caddy:2-alpine caddy hash-password --plaintext 'your-password'

# 共享密钥（写进 .env 的 TEAMCP_PROXY_SECRET）
openssl rand -base64 32
```

然后配身份（见下文"配身份"）。

---

## 形态二：已经有反向代理

一个二进制就够：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags)" \
  -o teamcp ./cmd/teamcp
./teamcp -listen 127.0.0.1:8080 -data-dir /var/lib/teamcp \
         -auth-mode proxy -proxy-secret "$TEAMCP_PROXY_SECRET" \
         -trusted-proxies 127.0.0.1/32
```

代理侧必须做两件事，缺一不可：

1. **先清空**客户端可能自带的 `X-Teamcp-User` / `X-Teamcp-Secret`，再设成你的值；
2. 认证通过后把登录名写进 `X-Teamcp-User`。

nginx 的样子：

```nginx
location /mcp {
    proxy_set_header X-Teamcp-User   "";      # 清空入站
    proxy_set_header X-Teamcp-Secret "";
    proxy_set_header X-Teamcp-User   $remote_user;   # 或 $auth_user
    proxy_set_header X-Teamcp-Secret "REPLACE_ME";
    proxy_pass http://127.0.0.1:8080;
    proxy_buffering off;                      # wait 是长轮询，别缓冲
    proxy_read_timeout 300s;
}
```

> `proxy_buffering off` 和放宽 `read_timeout` 不是可选优化：`wait` 默认挂 25 秒，
> 缓冲或默认 60 秒超时会让它看起来"没有反应"。

---

## 形态三：单机、无认证

只在自己的机器上跑、只连回环：

```bash
./teamcp -listen 127.0.0.1:8080 -data-dir ./data -auth-mode open
```

`open` 模式下**任何**能连到端口的进程都能用任意已配置身份。仅在端口不对外时使用。

---

## 配身份

身份**只能由人配置**，DSL 里没有对应动词——加一个身份等于扩大能力面（ADR-013 的推论）。

```bash
curl -X POST https://your-domain/admin/api/actors \
  -u team:your-password \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "planner",
    "homeserver": "https://matrix.example.org",
    "user_id": "@teamcp_acme_a_planner:matrix.example.org",
    "access_token": "syt_...",
    "default": true
  }'
```

MXID **必须**是 `@teamcp_<principal>_<a|h>_<agent>:<host>`：

- `a` = AI，`h` = 人；
- 前导下划线不行（Dendrite 会拒），所以是 `teamcp_` 而不是 `_teamcp_`；
- principal 与 agent 名只允许 `a-z0-9-`，因为 `_` 是分隔符。

不在这个命名空间里的账号会被拒绝——那是信任边界，不是格式要求。

配完验收：

```bash
curl -X POST https://your-domain/admin/api/actors/planner/test -u team:your-password
# {"ok":true,"principal":"acme","agent":"planner","kind":"ai","user_id":"@teamcp_acme_a_planner:..."}
```

---

## 运维

| | |
|---|---|
| 健康检查 | `GET /healthz`——只回进程存活，**不查 homeserver**（否则 Matrix 一慢就被重启） |
| 指标 | `GET /metrics`（Prometheus 文本格式） |
| 备份 | `data/actors.json` 就够了。**消息不在本地**，在 homeserver 上，按 homeserver 的方式备份 |
| 排障 | 日志是 JSON，一行一条。ACL 拒绝会带 `"msg":"audit"` |
| 代理链 | 只监听回环或私网；`--trusted-proxies` 必须覆盖代理的源地址，否则一律 403 |

### 身份令牌泄漏怎么办

删掉那个身份再重建：

```bash
curl -X DELETE https://your-domain/admin/api/actors/planner -u team:your-password
```

homeserver 侧请同时登出那个 access token。Teamcp 不存消息，所以**没有需要清理的本地副本**。
