# OAIprism

OpenAI Prism 的高性能反向代理。把 `prism.openai.com` 的内部 start + poll 协议
包装成标准的 **OpenAI Chat Completions / Responses** 与 **Anthropic Messages** 接口，
同时保留一条完全不懂协议也能用的原样反代通道。

Go 单二进制，零 CGO，无运行时依赖。

> 本机部署、Codex 接入与 SUB2API 中转见 [`docs/deploy.md`](docs/deploy.md)。

---

## 为什么是这个技术栈

| 需求 | 选择 | 理由 |
|---|---|---|
| 语言 | Go 1.25 | 流式反代的瓶颈是"连接多、分配少、延迟低"，Go 的 net/http + goroutine 模型恰好命中；单二进制部署，无运行时依赖 |
| 上游连接 | 标准库 `http.Transport` | 原生支持 HTTP/2 多路复用；`fasthttp` 不支持 HTTP/2 上游，反而更慢 |
| 缓冲 | `sync.Pool` + `bufio` | 稳态零分配；SSE 逐帧 `Flush` |
| JSON | 手写编码器 | 热路径上避开反射与 map 分配，单 chunk 编码开销下降一个数量级 |
| 指标 | 自研 Prometheus 文本输出 | 只用了十几个指标，不值得引入 5 个间接依赖 |
| 依赖总数 | **1**（`gopkg.in/yaml.v3`） | 供应链面小到可以人工审计 |

---

## 快速开始

### 1. 编译

```bash
go build -o oaiprism.exe ./cmd/oaiprism
```

### 2. 准备凭据（你唯一需要提供的东西）

**推荐方式**：一条命令搞定，顺带在线校验。

```bash
# 从浏览器开发者工具复制整串 Cookie 后：
./oaiprism.exe import -cookie "__Secure-next-auth.session-token=eyJ..." -id main

# 或者从标准输入读，避免 shell 历史泄漏：
./oaiprism.exe import -stdin -id main

# 已经拿到 JWT 的话：
./oaiprism.exe import -access-token "eyJhbGci..." -id main

# 有 refresh_token 最省心（可无限自动续期）：
./oaiprism.exe import -refresh-token "..." -id main
```

命令会做四件事：向上游验证凭据 → 补全 email/plan/account_id →
（若可能）换取 refresh_token 以获得自愈能力 → 写入 `secrets/accounts.json`。

> 凭据从哪拿：登录 `prism.openai.com`，打开开发者工具 →
> Application → Cookies → 复制 `__Secure-next-auth.session-token` 那一行；
> 或 Network 面板里任意请求的完整 Cookie 头。

也可以直接编辑 `secrets/accounts.json`（参考 `secrets/accounts.example.json`）。
**服务运行中保存该文件即自动生效，5 秒内热加载，无需重启。**

### 3. 启动

```bash
cp configs/config.example.yaml configs/config.yaml
./oaiprism.exe serve -config configs/config.yaml
```

没配凭据也能正常启动——服务会打印明确警告，等你把凭据放进去后自动开始工作。

### 4. 用起来

```bash
# OpenAI 风格
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"你好"}]}'

# 直接指向官方 SDK
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY=sk-oaiprism-your-secret

# Anthropic 风格
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787

# 原样反代（不懂协议也能用）
curl http://127.0.0.1:8787/prism/api/auth/session
```

---

## 端点一览

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions（流式/非流式） |
| `POST /v1/completions` | 老式 Completions，内部转 chat |
| `POST /v1/responses` | OpenAI Responses API（新版 SDK / Codex CLI 首选） |
| `POST /v1/messages` | Anthropic Messages（含完整流式事件序列） |
| `GET /v1/models` | 可用模型列表 |
| `/prism/*` | **原样反代通道**，按白名单转发到上游 |
| `GET /healthz` | 存活探针（不检查下游，避免上游抖动导致误重启） |
| `GET /readyz` | 就绪探针（无可用账号时明确返回 503） |
| `GET /metrics` | Prometheus 文本指标 |
| `GET /admin/accounts` | 账号池状态（不含凭据） |
| `POST /admin/reload` | 手动重载凭据文件 |
| `POST /admin/accounts/{id}/refresh` | 强制刷新某个账号的 token |

### 请求级控制头

| 头 | 作用 |
|---|---|
| `X-Oaiprism-Session` | 指定会话身份 → 决定账号粘性与项目复用 |
| `X-Oaiprism-Account` | 强制使用某个账号（调试用） |
| `X-Oaiprism-Project` | 强制使用某个上游项目 ID |
| `X-Oaiprism-Model` | 覆盖模型名 |
| `X-Oaiprism-Effort` | 覆盖推理强度 |

---

## 性能设计

反代的性能瓶颈不在"算得快"，而在"连接复用、内存分配、延迟首字节"。
下面是具体做了什么：

**连接层**
- `MaxIdleConnsPerHost: 256`（默认 2 是吞吐的第一瓶颈）
- 启动时预热 TCP + TLS，把 DNS/握手成本挪出请求路径
- 出站强制 HTTP/2 多路复用
- 关闭 Go 的自动 gzip，否则 Content-Encoding/Length 会与实际不一致

**流式**
- SSE 缓冲来自 `sync.Pool`，稳态零分配
- 手写 JSON 转义（对照标准库做了等价性测试）
- 每个增量立即 `Flush`，并显式设置 `X-Accel-Buffering: no`，
  否则 Nginx 会把整个流缓冲到结束，"流式"退化成一次性返回

**轮询转流式**

上游是 start + poll 协议，天生不是流式的。这里做了三件事把它压成流式：
1. **长轮询**：`waitMs=10000`，有内容立刻返回、没内容挂起
2. **自适应退避**：若上游忽略 `waitMs`（响应耗时 < 250ms），自动退避，
   不会退化成忙轮询；长轮询生效时则不叠加 sleep
3. **前缀差分**：不管上游返回累计全文还是结构化消息列表，
   用前缀差分还原 token 级增量，因此不依赖具体响应形态

**项目复用**

`POST /api/projects` 比推理调用本身还慢。同一会话只建一次工程并长期复用；
无会话标识的请求走轮转分桶，避免所有请求挤在一个项目里被上游串行化。

**可观测**

`oaiprism_facade_first_delta_seconds` 直接量首字延迟——这是流式体验的核心指标，
比端到端耗时更能反映体验好坏。

---

## 协议（已实测校准）

> **状态：已用真实流量校准。** 详见 [`docs/协议校准报告.md`](docs/协议校准报告.md)。
> 三处独立证据交叉验证（上游错误信息、前端 bundle 源码、可复现的路径对照），
> 不是推断。

### 关键路径：是 `llm` 不是 `lim`

早期那版逆向文档写成 `/api/lim/response_with_tools_start`，**是笔误**：

| 路径 | 结果 |
|---|---|
| `/api/llm/response_with_tools_start` | JSON API（假 token 得到 401，说明路由与鉴权都正常） |
| `/api/lim/response_with_tools_start` | Next.js 的 404 HTML 页（和随便编的路径完全一样） |

决定性证据是前端源码：`grep "api/lim" *.js` 命中 **0** 次，
而 `/api/llm/response_with_tools_{start,status,stop}` 三个全在。
E2E 测试里有一条断言：一旦有请求打到 `/api/lim/`，测试立刻失败。

### 协议形状

```jsonc
// POST /api/llm/response_with_tools_start
{"input":[{"type":"message","role":"user",
           "content":[{"type":"input_text","text":"…"}]}],
 "previousResponseId":"req-…",          // 可选，camelCase
 "conversationId":"…",                  // 可选，camelCase
 "metadata":{"model":"gpt-5.6-sol",     // ← 模型参数在 metadata 里，不在顶层
             "reasoning_effort":"medium",
             "projectId":"…",
             "frontend_origin":"https://prism.openai.com"}}

// → {"status":"started","request_id":"…","turn_state":{…}}      需要轮询
// → {"status":"completed","request_id":"…","response":{…}}      已经结束

// POST /api/llm/response_with_tools_status
//   必须带 {request_id, turn_state} 两个字段
//   turn_state 是服务端下发的不透明续令牌，必须原样回传（自造值会被拒）
// → {"status":"pending","turn_state":{…新令牌…}}                继续轮询
// → {"status":"completed","response":{"status":"success","payload":{"output":[…],…}}}

// 答案位置：response.payload.output[-1].content[*].text
// 只取最后一条 output —— 多轮下 output 会累积历史，全拼会把上一轮当这一轮。
```

**三个反直觉的点，每个都是一个坑**：

1. **失败是 HTTP 200**。上游用 `status:"completed" + response.status:"error"`
   表达失败。只看状态码会把失败当成"成功但内容为空"，返回一个空回答给客户端。
2. **`turn_state` 是续令牌**，每轮换新；忘了更新就会永远拿到同一个 `pending`。
3. **大小写不统一**：start 用 camelCase 的 `conversationId`，
   status 用 snake_case 的 `request_id`。不要"顺手统一"。

### 轮询节奏的取舍

真实前端是**固定 5 秒**轮询，且 pending 分支只读 `turn_state` 不看 `response`
—— 这暗示 pending 帧可能根本没有正文。我们默认取 1 秒作为折中，
并且循环内做了**无条件节流**（绝不会退化成零间隔忙轮询）。
出现 429 就调大 `poll_interval`。

### 仍未确认的三件事

| 项 | 状态 |
|---|---|
| `pending` 帧里是否有累计正文 | **待验证**——两种形态都已支持并有测试，但哪种是真的要真凭据才知道 |
| OpenAI 的 `tools` 该映射到哪 | **待验证**——真实前端请求体里没有它，暂放 `metadata.tools` |
| 上游可用模型列表 | 默认 `gpt-5.6-sol`，真实列表由 Statsig 开关动态下发 |

### 字段名仍是可配置的

上面的值已经写进 `facade.schema` 的默认值，同时保留配置能力以应对上游变化。
改字段不需要重新编译。校准流程：

```bash
./oaiprism.exe capture-summary -file captures/capture-2026-09-16.jsonl
```

## 沙箱：必需的前置环节（已实测跑通）

> 完整调研与证据见 [`docs/Yjs依赖调研与沙箱方案.md`](docs/Yjs依赖调研与沙箱方案.md)。

Prism 的 AI 助手跑在一个**容器沙箱**里，而且它需要「项目工作区」才能工作。
完整链路是 **四步注入 + 一次等待**，全部已用真实凭据验证：

```
1. POST /api/backend/1/new                              申请沙箱
                                    → {url, token}
2. POST /api/projects/{id}/sandbox/resources-token       后端签发资源令牌（绑定项目，1h）
   body: {sandbox_session_id, sandbox_token}
                                    → {access_token, resources_base_url, expires_at}
3. POST <sandbox>/resources-token                        把令牌交给沙箱
   body: {token, resourceBaseUrl, projectId}
                                    → {"status":"success"}
4. POST /api/y                                           取 Y-Sweet 凭证
   body: {docId, requestContext}
                                    → {url(wss://…), baseUrl, authorization, token}
5. POST <sandbox>/token  (body = 第 4 步的整个对象)        原样转交
                                    → {"success":true,"message":"Token received"}
6. GET  <sandbox>/wait-for-sync?wait_ms=10000            等就绪 → status=synced
```

### 为什么不需要实现 Yjs

第 5 步之后是**沙箱自己**去连 Y-Sweet WebSocket 同步文档 ——
我们只是信使，一行 CRDT 代码都不用写。证据是状态机自证：

```jsonc
// 只申请沙箱，什么都不注入 → 永远停在这个状态
{"status":"syncing","tokens":{"hasResourceProjectId":false,
                              "hasCurrentYSweetToken":false,
                              "hasSyncedYSweetProvider":false,
                              "fileCredentialSource":"none"}}

// 交付 Y-Sweet 凭证后，1 秒内变成
{"status":"synced","tokens":{"hasResourceProjectId":true,
                             "hasCurrentYSweetToken":true,
                             "hasSyncedYSweetProvider":true,
                             "fileCredentialSource":"resources-token"}}
```

`hasSyncedYSweetProvider` 由 false 变 true 是它**自己同步完成**的标志。

> 顺带调研了 Go 生态的 Yjs 实现（全部实测 `go get`）：
> `github.com/reearth/ygo` v1.50.0 可用，只 import `crdt`+`sync` 时
> **只增加 1 个依赖**。其余候选要么模块路径写错、要么需要 cgo+Rust、要么已停更。
> **但当前不需要它**，所以本项目依赖数依然是 1。

### 漏掉注入会怎样（症状极具误导性）

沙箱**不报任何错**，只是永远停在 `syncing`，最终表现为会话处理
**固定 122 秒**后 504，文案是 `Please submit prompt again.`
看起来像「上游挂了」或「容器冷启动慢」，实际上是它在等凭证。

看到「122 秒」这个特征值，直接去查 `wait-for-sync` 的 `tokens` 字段。

### 两个实战坑

1. **认证是双重的**：`X-Crixet-Sandbox-Token` + **Cookie**。
   只带前者会得到 `401` 且**响应体为空**——极难排查。
2. **沙箱 URL 不能当绝对 URL 直接发**：它形如
   `https://prism.openai.com/s/sandboxes/proxy/`，而我们的客户端是把 path
   **拼接**到 BaseURL 上的，传绝对 URL 会拼出 `/https://…` 这种畸形路径。
   正确做法是只取 path 再拼子路径，复用同一套连接池与 Cookie 注入。

### 性能实测

| 场景 | 延迟 |
|---|---|
| 首次（建项目 + 申请沙箱 + 四步注入 + 生成） | **14.2 s** |
| 同会话再次请求（项目/沙箱/同步全命中） | **5.6 s** |
| 沙箱工作区同步本身 | **2.6 – 3.3 s** |

同步状态按 **(账号, 项目)** 粒度缓存，失效点绑资源令牌的 `expires_at`
（1 小时）——令牌过期后沙箱读不到项目文件，与其等失败重试不如到点主动重同步。

会话复用键是「system 提示 + 首条 user 消息」的哈希；
客户端可以用 `X-Oaiprism-Session` 请求头显式指定，复用最稳定。

### 相关配置

```yaml
facade:
  use_sandbox: true        # 关掉会退化成"申请了但不同步"，几乎必然失败
  sandbox_ttl: 30m
  sandbox_ready_wait: 90s  # 等的是"同步完成"，不是"容器启动"
```

## 安全要点

这几条是刻意实现的，改动时请留意：

1. **原样反代绝不回传上游的 `Set-Cookie`** —— 那是账号池账号的会话凭据，
   透传等于把账号送给调用方。
2. **原样反代丢弃调用方自带的 `Authorization` / `Cookie`** ——
   否则调用方可以用自己的凭据覆盖账号池，绕过所有调度与统计。
3. **`/admin/accounts` 不返回任何凭据内容**，只有元信息与运行态。
4. **凭据文件以 `0600` 写入**。
5. `facade.api_keys` 留空表示不校验——**仅限本机或内网**，暴露公网前务必设置。

---

## 运维

```bash
# 看账号是否还能用
./oaiprism.exe probe -config configs/config.yaml

# 看账号池运行态
curl http://127.0.0.1:8787/admin/accounts | jq

# 常用指标
curl -s http://127.0.0.1:8787/metrics | grep -E 'first_delta|facade_runs|poll_rounds'
```

`readyz` 在没有可用账号时返回 503。这是刻意的：让"漏配凭据"
在部署阶段就暴露，而不是等到线上请求全 502。

---

## 目录结构

```
cmd/oaiprism/        命令行入口（serve / probe / import / capture-summary）
internal/
  config/            配置加载与校验
  creds/             凭据建模、JWT 解析、自动续期（session + OAuth 双路径）
  httpc/             面向上游的调优 HTTP 客户端
  account/           账号池：调度策略、粘性、并发闸门、冷却、热重载
  prism/             上游协议客户端 + 宽容解析 + 前缀差分
  sse/               SSE 写出层（池化缓冲、手写 JSON 编码）
  facade/            兼容门面：OpenAI Chat / Responses / Anthropic Messages
  rawproxy/          原样反代通道
  capture/           抓包录制与协议摘要
  middleware/        恢复、请求 ID、指标、鉴权、限流
  metrics/           零依赖 Prometheus 指标
  server/            组件装配与生命周期
```

---

## 测试

```bash
go test ./...            # 全量
go test -race ./...      # 竞态检测
go test -bench=. ./internal/...   # 基准
```

测试里包含一个**模拟上游**（`internal/server/e2e_test.go`），
它把"协议漂移"变成可复现的输入 —— 想验证"上游把 `messages` 换成 `outputs` 会怎样"，
在那里加个分支即可，不用真去抓包。

覆盖的用例：非流式/流式 chat、Anthropic 事件序列、Responses API、
项目复用与隔离、账号失效自动换号、无凭据错误提示、参数校验、
原样反代（含凭据注入与 `Set-Cookie` 剥离）、白名单、API Key 鉴权、
请求 ID 透传、运维端点、20 路并发流。

### 关于 `internal/prism` 的端点覆盖

该包实现了文档里列出的**全部**端点（projects / project-access /
conversation-history / lim start+status / sandbox render+render-status /
project-files upload / PATCH thumbnail）。其中一部分目前只被
原样反代通道使用，门面路径用不到 —— 这是有意的：
逆向出来的协议客户端应当完整，否则等你要用某个端点时还得回头补一遍。
详见 `internal/prism/client.go` 的 `Path*` 常量。
