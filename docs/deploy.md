# 部署与中转

两种用法，共用同一个二进制：

```text
本机模式   Codex / SDK ──127.0.0.1:8787/v1──▶ oaiprism ──(HTTPS, 经本机代理)──▶ prism.openai.com
中转模式   客户端 ──▶ SUB2API ──▶ oai-prism:8787/v1（Docker 私有网络）──▶ prism.openai.com
```

中转模式参照 [excel-codex-bridge 的 SUB2API 插件](https://github.com/Kaixxrua/excel-codex-bridge/blob/main/docs/sub2api.md)：
独立 sidecar、不发布宿主机端口、密钥走 Docker secret、不改 SUB2API 本身。

## 1. 本机部署（Windows）

```powershell
# 构建（需要 Go 1.26+）
go build -trimpath -ldflags "-s -w" -o bin\oaiprism.exe ./cmd/oaiprism

# 配置：基于示例，至少改 server.host 为 127.0.0.1；国内网络需设 upstream.http_proxy
# （本服务不读 HTTPS_PROXY 环境变量）。creds.file 建议写 ../secrets/accounts.json。
copy configs\config.example.yaml configs\config.yaml

# 启动 / 停止（首次启动会生成 secrets\api-key.txt）
powershell -ExecutionPolicy Bypass -File packaging\windows\start.ps1
powershell -ExecutionPolicy Bypass -File packaging\windows\stop.ps1
```

启动后：

| 地址 | 说明 |
|---|---|
| `http://127.0.0.1:8787/v1` | OpenAI / Anthropic 兼容接口，需 `Authorization: Bearer <secrets\api-key.txt>` |
| `http://127.0.0.1:8787/dashboard/` | 控制面板；页面可直接打开，面板内数据请求仍需在右上角填 API Key |
| `http://127.0.0.1:8787/readyz` | 没导入账号时返回 503 `no_credentials`，属正常 |
| `logs\oaiprism.log` | 运行日志（UTF-8） |

### 首次登录（自动登录向导）

不需要手动从开发者工具复制 Cookie。当 `secrets\accounts.json` 里没有可用的
`prism_oai_access_token`（首次使用，或 token 已过期）时，sidecar 启动会自动
弹出**有头** Chrome 打开 prism.openai.com：

1. 在弹出的窗口里完成登录（Google / 邮箱均可）；
2. sidecar 轮询 `/api/auth/session` 确认登录（判定标准：`user.email` 非空
   **且** 上下文里出现 `prism_oai_access_token` —— 匿名访客也会拿到
   `prism_session_token` 并带非空 user，不能作为登录依据，实测 2026-10-01）；
3. 确认后自动刷新页面，从浏览器上下文抓取完整 Cookie（等价于 session 请求的
   Request 头 Cookie），调用 `oaiprism import` 经上游校验后**双写**
   `accounts.json` + `accounts.db`，网关 ~5 秒内热加载；
4. 本次会话直接复用刚登录的浏览器上下文对外服务；下次启动改为无头预置
   cookie，不再弹窗。

异常自愈：登录窗口崩溃/页面加载失败会自动重开浏览器继续等待（最多 4 次）；
用户主动关闭登录窗口视为放弃。token 过期（剩余寿命 < 5 分钟）同样进入向导。
手动触发重登：`node tools\browser_sidecar.js login 8790`。

### 导入账号（手动方式，备用）

登录 `prism.openai.com`，开发者工具 → Network → 任意请求的 `Cookie` 请求头，整串复制后：

```powershell
Get-Clipboard | .\bin\oaiprism.exe import -config configs\config.yaml -stdin -id main
.\bin\oaiprism.exe probe -config configs\config.yaml
```

`import` 先向上游校验，再同时写入 `secrets\accounts.json` 与 `secrets\accounts.db`。
运行中的服务 5 秒内热加载 JSON；重启后以 SQLite 为准，所以两边都写。
从剪贴板经标准输入导入，Cookie 不会进 shell 历史。

### 接入 Codex（CLI 与桌面版）

**一键会话（推荐，对齐 excel-codex-bridge 的使用方式）：**

```text
tools\start_bridge.cmd            双击：CLI 会话
tools\start-bridge-desktop.cmd    双击：桌面版会话（会拉起 Codex 桌面应用）
```

窗口开着 = 代理开着。启动时临时把 Codex 配置指向本代理（provider + 用户环境
变量 `OAI_PRISM_API_KEY`），网关与 sidecar 作为该控制台的子进程运行、日志实时
可见；按 **Q + Enter / Ctrl+C / 关闭窗口** 即全部停止，并**自动还原** Codex
原始配置——代理不在时 Codex 自动回到官方登录，不存在"指向已死端口"的问题。
桌面模式下拉起的应用在会话结束后需重启才会切回官方登录（环境变量在启动时读取）。

窗口被直接硬关（绕过清理）时用兜底脚本：

```powershell
powershell -ExecutionPolicy Bypass -File tools\stop_bridge.ps1
```

**一次性覆盖方式**（不碰 config.toml，只影响单次进程）：

```powershell
powershell -ExecutionPolicy Bypass -File packaging\windows\codex.ps1 exec "解释这个仓库"
```

API Key 通过 provider 的 `env_key` 只交给当前进程。
默认模型 `gpt-6.1-sol`，可用环境变量 `OAI_PRISM_CODEX_MODEL` 改。

其他客户端（任意 OpenAI/Anthropic 兼容应用）：

```powershell
$env:OPENAI_BASE_URL = "http://127.0.0.1:8787/v1"
$env:OPENAI_API_KEY  = (Get-Content secrets\api-key.txt -Raw).Trim()
```

注意：Windows PowerShell 5.1 读取无 BOM 的 .ps1 时按 ANSI 解码，`tools\` 下的
.ps1 均已带 UTF-8 BOM，编辑时请保留；`.cmd` 用 `chcp 65001` 切到 UTF-8。

## 2. SUB2API 中转（Docker）

需要 Docker Compose 与一个已在运行的 SUB2API。在服务器上、本仓库根目录执行：

```sh
# API Key：给 SUB2API 用的上游密钥，不是 ChatGPT 凭据
mkdir -p packaging/sub2api/secrets && chmod 700 packaging/sub2api/secrets
( umask 077; printf 'sk-oaiprism-%s\n' "$(openssl rand -hex 24)" > packaging/sub2api/secrets/api-key )
# 容器以 UID 10001 运行，Compose 的 file secret 通常保留源文件属主
sudo chown 10001:10001 packaging/sub2api/secrets/api-key

cd packaging/sub2api
cp .env.example .env
# 把 SUB2API_NETWORK 改成现有网络名：
#   docker inspect <SUB2API容器> --format '{{json .NetworkSettings.Networks}}'
docker compose config --quiet
docker compose up -d --build
docker exec oai-prism wget -qO- http://127.0.0.1:8787/readyz   # 未导入账号时 no_credentials
```

### 导入账号（从你自己的电脑经 SSH）

```powershell
Get-Clipboard | ssh operator@your-vps "docker exec -i oai-prism oaiprism import -stdin -id main"
ssh operator@your-vps "docker exec oai-prism oaiprism probe"
```

凭据经 SSH 的标准输入传输，不进命令行参数、临时文件或日志，落在命名卷 `/data`，重建容器不丢。

### 在 SUB2API 添加账号

管理后台新建 **OpenAI / API Key** 账号：

| 项目 | 值 |
|---|---|
| Base URL | `http://oai-prism:8787/v1` |
| API Key | `packaging/sub2api/secrets/api-key` 的内容 |
| OpenAI 透传 | 开启（保留 Responses 的工具、reasoning 与输入结构） |
| 模型 | 取自 `/v1/models`，如 `gpt-6.1-sol`、`gpt-6-luna`、`gpt-5.6-sol`；映射保持恒等 |

若 SUB2API 拒绝私有地址，只为这个主机名/端口放行，不要全局关闭 SSRF 防护。
出站需要代理时在 `.env` 设 `OAI_PRISM_HTTP_PROXY`（必须是容器内可达的地址，不是宿主机 127.0.0.1）。

## 3. 安全边界

- **API Key 就是管理员权限。** `/v1/*`、`/admin/*`（账号增删改）共用同一组 Key；
  控制面板的登录框（admin/admin123）只是前端门面，真正的鉴权只有 API Key。
  控制面板「API Keys」页生成的 Key 存在 SQLite，**不参与鉴权**——有效的只有
  `facade.api_keys` / `OAI_PRISM_API_KEYS` / `OAI_PRISM_API_KEYS_FILE`。
- `facade.api_keys` 为空时服务不做任何鉴权，连同 `/prism/*`（会注入账号 Cookie）一起开放。
  本机部署因此只监听 `127.0.0.1` 并强制使用密钥文件；`OAI_PRISM_API_KEYS_FILE`
  指向的文件缺失或为空时直接拒绝启动，而不是静默关闭鉴权。
- 中转模式关闭了 `/prism/*` 原样反代，且不发布宿主机端口。
- `secrets/`、`configs/config.yaml`、`configs/secrets/`、`packaging/sub2api/secrets/` 均已加入 `.gitignore`，
  构建上下文由 `.dockerignore` 排除。
- 这是把 Prism 网页会话包装成 API 的非官方工具：只用你自己的账号，遵守 OpenAI 的服务条款。

## 4. 相对上游仓库的修改

| 问题 | 处理 |
|---|---|
| `cmd/oaiprism/` 从未入库：`.gitignore` 的 `oaiprism` 规则匹配任意层级同名路径，README 的构建命令直接失败 | 规则改为 `/oaiprism`，重写入口（`serve` / `probe` / `import` / `capture-summary` / `version`） |
| 设置 API Key 后控制面板打不开：浏览器地址栏导航带不上 Bearer，`/dashboard/` 被 401 | `/dashboard/` 静态资源对 GET/HEAD 豁免（先 `path.Clean`，`/dashboard/../admin` 仍被拦） |
| 任何垃圾 Cookie 都能「校验通过」：会话接口对未登录也回 200（`userTier: logged_out` / `{}`），而 `Usable()` 看到请求自带的 Cookie 就判可用；后台刷新同样会把失效账号当成刷新成功 | 明确未登录时返回 401 类 `APIError` |
| API Key 只能明文放配置或环境变量 | 新增 `OAI_PRISM_API_KEYS_FILE` |
| Codex 桥多轮上下文丢失：模型"收到文件读取结果却问任务是什么"。实测上游 input 数组只认**首条 system + 最后一条 user**，其余条目一律丢弃（见 `docs/协议校准报告.md` 第八节），而桥按"完整重放"实现 | `bridgeInputItems` 重写：桥指令作首条 system，整个对话（人设/任务/调用回放/执行结果，含无输出的执行）折叠进唯一一条 user 消息。超过 180 KB 时优先保留最近状态（约 2/3），再保留开头的任务，丢弃中段；收尾指令始终在最后 |
| 桥对纯回答（"42."）也触发"你没用 codex-exec"纠错，把模型带偏成反问 | 只在回复声称执行过操作（"已创建""ran the command"等）时纠错 |
| `packaging/windows/codex.ps1`（本次新增）在 `exec` 只带一个提示词时报 `unexpected argument`：`$rest = if (...) { ... }` 的语句输出把单元素数组解包成字符串，`@rest` 展开字符串变成逐字符传参 | 先 `$rest = @()`，再在 `if` 内用 `@(...)` 赋值 |
| 一键脚本无法让 Codex 桌面版走代理：桌面应用不支持 `-c` 覆盖 | 新增 `tools/setup_codex_config.ps1`（已并入 start_bridge）：向 config.toml 写 provider 并设默认 `model_provider`，API Key 写入用户环境变量 `OAI_PRISM_API_KEY`；`-Undo` 可精确回退 |

## 5. 浏览器通道 sidecar（必需，已接入）

Cloudflare 对 `prism.openai.com` 的 API 路径启用 TLS 指纹校验：Go 客户端
（无论凭据多新鲜）在 `/api/projects`、`/api/llm/*` 等路径上一律
`403 Request verification failed`；真实 Chrome 的请求全部放行。
`tools/browser_sidecar.js` 用 Playwright 驱动一个有头 Chrome
（无头模式 session 换发被拒，实测 401），登录态取自
`secrets/accounts.json` 的 `prism_oai_access_token`，在页面内 `fetch`
转发网关的全部上游请求。

链路与配置（`configs/config.yaml`）：

```text
codex / SDK ──▶ 127.0.0.1:8787 (oaiprism) ──▶ 127.0.0.1:8790 (sidecar) ──▶ prism.openai.com
```

- `upstream.base_url: http://127.0.0.1:8790` —— 上游整体改走 sidecar；
- `upstream.http_proxy` 必须留空 —— 静态代理对 127.0.0.1 同样生效，会把
  发往 sidecar 的流量也推进代理；上游出口由 sidecar 里的 Chrome 自理；
- `creds.persist_refresh: true` —— 续期结果写回 accounts.json，
  sidecar 每次重启都用它重建浏览器登录态，重启后自愈；
- OAuth 续期（`auth.openai.com`）不经过 sidecar，需本机可直连或自行解决。

一键启停（路径由脚本位置推导，可随仓库移动）：

```powershell
powershell -ExecutionPolicy Bypass -File tools\start_bridge.ps1   # 网关 + sidecar
powershell -ExecutionPolicy Bypass -File tools\stop_bridge.ps1    # 停止并清理自动化 Chrome
cmd /c tools\check_bridge.cmd                                     # 报错时先跑这个
```

sidecar 依赖 `tools/` 下的 `playwright-core`（`npm install` 一次即可，
已进 `.gitignore`）；Chrome 按常见位置自动探测，可用环境变量
`OAIPRISM_CHROME` / `OAIPRISM_HOME` / `OAIPRISM_PLAYWRIGHT` 覆盖。
sidecar 默认**无头**运行（Chrome 132+ 的 new headless，实测登录态换发
正常；原作者当年被拒的是已移除的 old headless），屏幕上不会出现浏览器
窗口。若上游行为变化导致 401，设 `OAIPRISM_SIDECAR_HEADED=1` 回到有头
模式（会出现一个 Chrome 窗口，属正常现象，不要关闭它）。

## 6. 其他已知限制

- 启动日志里的「上游连接预热 connections=0」是因为预热直接拨号、不走
  `upstream.http_proxy`；只影响预热，实际请求走代理。
- sidecar 把上游响应头收敛为只回传 `Content-Type`：网关因此读不到
  `Retry-After`，429 退避走默认节奏；其余协议字段全在响应体里，不受影响。
