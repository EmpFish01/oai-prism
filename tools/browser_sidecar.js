/**
 * OAIprism 浏览器通道 sidecar。
 *
 * 背景：Cloudflare 对 prism.openai.com 启用 TLS 指纹校验，Go/Python
 * 客户端（无论凭据多新鲜）一律 403 "Request verification failed"；
 * 真实 Chrome（playwright 驱动）的请求全部放行。
 *
 * 原理：常驻一个 Chrome 实例（登录态 = 预置 access_token cookie，
 * Prism 会自动换发完整 session），本地 8790 收到 OAIprism 的上游请求
 * 后在浏览器页面内 fetch 转发，把状态码/头/体原样返回。
 * 上游协议是 start+poll（纯请求-响应，无 SSE），所以不需要流式透传。
 *
 * 首次运行（无 cookie 或 token 已过期）：自动弹出**有头** Chrome 打开
 * prism.openai.com 提示登录；确认登录后自动刷新页面，从浏览器上下文
 * 抓取 session 的 Request Cookie，并调用 `oaiprism import` 写入账号池
 * （JSON + SQLite 双写，经网关校验）。之后重启即走无头预置 cookie 路径。
 *
 * 用法：node browser_sidecar.js [auto|login|<access_token>] [端口=8790]
 *   auto   默认：有 cookie 用 cookie（无头），无 cookie 进登录向导
 *   login  强制进入登录向导（有头）
 *
 * 环境变量（均可省略）：
 *   OAIPRISM_HOME       仓库根目录（默认取本脚本上一级）
 *   OAIPRISM_CHROME     Chrome 可执行文件路径（默认按常见位置自动探测）
 *   OAIPRISM_SIDECAR_PORT  默认端口（命令行参数优先）
 */
const http = require("http");
const path = require("path");
const fs = require("fs");

// 仓库根：默认本脚本上一级；部署挪位置时只需改 OAIPRISM_HOME。
const HOME = process.env.OAIPRISM_HOME || path.join(__dirname, "..");

// playwright-core：优先 tools/node_modules（tools/package.json 安装），
// 其次全局安装与 @playwright/mcp 的内嵌副本，最后环境变量直接指定。
function loadPlaywright() {
  const candidates = [];
  if (process.env.OAIPRISM_PLAYWRIGHT) candidates.push(process.env.OAIPRISM_PLAYWRIGHT);
  candidates.push("playwright-core"); // 常规 require 解析（含 tools/node_modules）
  const npmGlobal = process.env.APPDATA
    ? path.join(process.env.APPDATA, "npm", "node_modules")
    : "";
  if (npmGlobal) {
    candidates.push(path.join(npmGlobal, "playwright-core"));
    candidates.push(path.join(npmGlobal, "@playwright", "mcp", "node_modules", "playwright-core"));
  }
  for (const c of candidates) {
    try { return require(c); } catch (_) { /* 下一个 */ }
  }
  throw new Error(
    "找不到 playwright-core。请在 tools/ 下执行: npm install playwright-core\n" +
    "或用环境变量 OAIPRISM_PLAYWRIGHT 指向已安装的 playwright-core 目录"
  );
}
const { chromium } = loadPlaywright();

// Chrome 可执行文件：按常见位置探测，可用 OAIPRISM_CHROME 覆盖。
function findChrome() {
  if (process.env.OAIPRISM_CHROME) return process.env.OAIPRISM_CHROME;
  const PF = process.env["ProgramFiles"] || "C:\\Program Files";
  const PFX86 = process.env["ProgramFiles(x86)"] || "C:\\Program Files (x86)";
  const LA = process.env["LOCALAPPDATA"] || "";
  for (const p of [
    path.join(PF, "Google", "Chrome", "Application", "chrome.exe"),
    path.join(PFX86, "Google", "Chrome", "Application", "chrome.exe"),
    path.join(LA, "Google", "Chrome", "Application", "chrome.exe"),
    path.join(PF, "Microsoft", "Edge", "Application", "msedge.exe"), // Edge 兜底（同为 Chromium）
  ]) {
    try { if (fs.existsSync(p)) return p; } catch (_) {}
  }
  return "chrome"; // 交给 PATH
}

const TARGET = "https://prism.openai.com";
const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36";
const LOGIN_TIMEOUT_MS = 10 * 60 * 1000;

// 参数：node browser_sidecar.js [auto|login|<access_token>] [port]
let AT = "";
let forceLogin = false;
const arg2 = process.argv[2] || "";
if (arg2 === "login") forceLogin = true;
else if (arg2 && arg2 !== "auto") AT = arg2;
const PORT = parseInt(process.argv[3] || process.env.OAIPRISM_SIDECAR_PORT || "8790", 10);

// 从 secrets/accounts.json 的 cookies 字段自动提取 prism_oai_access_token。
function storedAccessCookie() {
  try {
    const accts = JSON.parse(fs.readFileSync(path.join(HOME, "secrets", "accounts.json"), "utf-8"));
    const cookies = (accts.accounts || []).map((a) => a.cookies || "").join("; ");
    const m = cookies.match(/prism_oai_access_token=([^;\s]+)/);
    return m ? m[1] : "";
  } catch (e) {
    return "";
  }
}

// 解析 JWT exp（毫秒）；解析失败返回 Infinity（当作不过期，交给预热判断）。
function jwtExpMs(token) {
  try {
    const p = JSON.parse(Buffer.from(token.split(".")[1], "base64").toString("utf8"));
    return typeof p.exp === "number" ? p.exp * 1000 : Infinity;
  } catch (_) {
    return Infinity;
  }
}

// 请求头透传白名单：这些头从 OAIprism 带到浏览器 fetch（其余由浏览器自己定）。
const PASS_HEADERS = new Set(["accept", "x-crixet-sandbox-token", "oai-account-id", "content-type"]);

// 页面内查 session（只在 prism.openai.com 源下调用）。
// 注意：匿名访客也有非空 user（email 为空）并会拿到 prism_session_token
// （实测 2026-10-01）—— 判断登录必须用 email 非空 + access_token cookie。
async function pageSession(page) {
  return page.evaluate(async () => {
    try {
      const r = await fetch(location.origin + "/api/auth/session", { credentials: "include" });
      if (!r.ok) return null;
      const j = await r.json();
      return j && j.user ? { email: j.user.email || "", anon: !!j.user.is_anonymous } : null;
    } catch (_) {
      return null;
    }
  });
}

// 登录向导：等用户在有头 Chrome 里完成登录，然后刷新页面并抓取
// 浏览器上下文里的全部 cookie —— 等价于 session 请求的 Request 头 Cookie。
async function loginWizard(page, ctx) {
  console.log("[sidecar] 已打开浏览器 —— 请在窗口里确认登录 prism.openai.com" +
    (AT ? "（已载入历史登录态，若服务端会话仍有效将自动继续）" : "（首次使用，需要登录一次）"));
  console.log("[sidecar] 登录完成后本程序会自动继续（最长等待 10 分钟）...");
  const deadline = Date.now() + LOGIN_TIMEOUT_MS;
  let hinted = 0;
  while (Date.now() < deadline) {
    if (page.isClosed()) throw new Error("用户关闭了登录窗口");
    if ((page.url() || "").startsWith("https://prism.openai.com")) {
      const s = await pageSession(page).catch(() => null);
      if (s && s.email && !s.anon) {
        console.log("[sidecar] 登录确认：" + s.email + " —— 刷新页面获取完整会话...");
        await page.reload({ waitUntil: "domcontentloaded" }).catch(() => {});
        await page.waitForTimeout(3000);
        const cookies = await ctx.cookies("https://prism.openai.com");
        const cs = cookies.map((c) => c.name + "=" + c.value).join("; ");
        if (cs.includes("prism_oai_access_token")) {
          console.log("[sidecar] 已捕获 Cookie（" + cookies.length + " 条，含 access_token）");
          return { cookieString: cs, email: s.email };
        }
      } else if (Date.now() - hinted > 60000) {
        hinted = Date.now();
        console.log("[sidecar] 仍在等待登录完成...");
      }
    }
    await page.waitForTimeout(2000);
  }
  throw new Error("登录等待超时（10 分钟）");
}

// 把抓到的 Cookie 写进账号池：调用 oaiprism import（stdin 传 Cookie，避免
// 日志泄漏），它做上游校验并**双写** accounts.json + accounts.db —— 只写
// JSON 会被 SQLite 的同 ID 账号在合并时覆盖，必须走 import。
// 异步执行：spawnSync 会阻塞事件循环，导致导入期间 relay 假死。
function importAccount(cookieString) {
  const exe = path.join(HOME, "bin", "oaiprism.exe");
  if (!fs.existsSync(exe)) {
    console.log("[sidecar] 找不到 " + exe + "，跳过自动导入（网关将无账号，直到手动导入）");
    return;
  }
  console.log("[sidecar] 正在把登录态写入账号池（oaiprism import，含上游校验）...");
  const { spawn } = require("child_process");
  const p = spawn(exe,
    ["import", "-config", path.join(HOME, "configs", "config.yaml"), "-stdin", "-id", "main"],
    { cwd: HOME });
  // import -stdin 阻塞读 stdin：必须写入 Cookie 并立即 end，否则子进程
  // 永远等不到输入（实测进程挂死、close 事件不触发）。
  p.stdin.write(cookieString);
  p.stdin.end();
  let out = "";
  p.stdout.on("data", (d) => { out += d; });
  p.stderr.on("data", (d) => { out += d; });
  p.on("error", (e) => console.log("[sidecar] 账号导入失败：" + String(e).slice(0, 160)));
  p.on("close", (code) => {
    const s = out.trim().replace(/\s+/g, " ");
    if (code === 0) {
      console.log("[sidecar] 账号导入成功" + (s ? "：" + s.slice(0, 160) : "") +
        " —— 网关将在 ~5 秒内热加载（accounts.json + accounts.db）");
    } else {
      console.log("[sidecar] 账号导入失败（exit " + code + "）：" + s.slice(0, 200));
    }
  });
}

(async () => {
  // 决定运行形态：无 cookie / token 过期 / 强制 login → 有头登录向导；
  // 否则无头预置 cookie。无头预热发现会话失效时也会自动转登录向导。
  // login 模式同样预置已存 cookie：服务端会话若仍有效，向导秒过；
  // 若失效，用户在有头窗口里重新登录即可。
  if (!AT) AT = storedAccessCookie();
  let interactive = forceLogin || !AT || jwtExpMs(AT) < Date.now() + 5 * 60 * 1000;
  if (AT && !forceLogin) {
    const exp = jwtExpMs(AT);
    console.log("[sidecar] 已从 accounts.json 提取 access_token（" + AT.length + " chars" +
      (isFinite(exp) ? "，有效期至 " + new Date(exp).toISOString() : "") + "）");
    if (interactive) console.log("[sidecar] token 已过期或即将过期 —— 进入登录向导");
  } else if (!AT) {
    console.log("[sidecar] accounts.json 里没有可用凭据 —— 进入登录向导");
  }

  let browser = null;
  let page = null;
  let cookieString = "";

  // 最多 4 次尝试：无头预热失败/页面崩溃都会重开浏览器继续，
  // 只有"用户主动关闭登录窗口"视为放弃（直接退出）。
  for (let attempt = 0; attempt < 4 && !page; attempt++) {
    browser = await chromium.launch({
      executablePath: findChrome(),
      // 无头：老版 Chrome 的 old headless 实测 session 换发被拒（401），
      // 但 Chrome 132+ 已移除 old headless，--headless 即 new headless
      // （完整浏览器内核，仅无 UI），实测通过（2026-10-01，Chrome 154）。
      // 登录向导必须无头=false（用户要看到并操作登录页）。
      headless: !interactive,
      args: ["--no-first-run", "--disable-blink-features=AutomationControlled"],
    });
    const ctx = await browser.newContext({
      viewport: { width: 1280, height: 800 },
      userAgent: UA,
    });
    if (AT) {
      await ctx.addCookies([
        { name: "prism_oai_access_token", value: AT, domain: "prism.openai.com", path: "/", secure: true, sameSite: "Lax" },
        { name: "oai-did", value: "a4f590ce-d1b4-4f4e-9f0a-4c1e2d3b5a6f", domain: ".openai.com", path: "/", secure: true, sameSite: "Lax" },
      ]);
    }
    page = await ctx.newPage();
    try {
      await page.goto(TARGET + "/", { waitUntil: "domcontentloaded", timeout: 90000 });
    } catch (e) {
      console.log("[sidecar] 页面加载失败（" + String((e && e.message) || e).slice(0, 120) + "）—— 重开浏览器重试");
      try { await browser.close(); } catch (_) {}
      browser = null; page = null;
      continue;
    }

    if (interactive) {
      try {
        const info = await loginWizard(page, ctx);
        cookieString = info.cookieString;
        AT = (cookieString.match(/prism_oai_access_token=([^;\s]+)/) || [])[1] || AT;
        break; // 已登录，直接用这个上下文对外服务
      } catch (e) {
        const msg = String((e && e.message) || e);
        if (msg.includes("用户关闭了登录窗口")) throw e; // 用户放弃，不重试
        console.log("[sidecar] 登录向导异常（" + msg.slice(0, 120) + "）—— 重开浏览器继续");
        try { await browser.close(); } catch (_) {}
        browser = null; page = null;
        continue;
      }
    }

    // 无头预热：触发一次 session 换发确认（GET 只读端点）。
    // 整段纳入恢复循环：页面崩溃/超时不 FATAL，重开浏览器再试。
    try {
      await page.waitForTimeout(20000);
      const warm = await page.evaluate(async () => {
        const r = await fetch(location.origin + "/api/maintenance", { credentials: "include" });
        return r.status;
      });
      console.log("[sidecar] 预热 maintenance:", warm);
      const s = await pageSession(page).catch(() => null);
      if (!s || !s.email || s.anon) {
        console.log("[sidecar] 预置 cookie 的会话已失效 —— 自动转入登录向导");
        interactive = true;
        try { await browser.close(); } catch (_) {}
        browser = null; page = null;
        continue; // 重开有头浏览器（保留旧 cookie，登录页可能自动放行）
      }
      console.log("[sidecar] 浏览器已就绪:", await page.title(), "（登录：" + (s.email || "未知") + "）");
    } catch (e) {
      console.log("[sidecar] 无头预热异常（" + String((e && e.message) || e).slice(0, 120) + "）—— 重开浏览器重试");
      try { await browser.close(); } catch (_) {}
      browser = null; page = null;
      continue;
    }
  }

  if (!page) {
    console.error("[sidecar] 无法建立浏览器会话");
    process.exit(1);
  }

  // 先监听端口（import 的上游校验要经过本 sidecar），再写账号池。
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const ch of req) chunks.push(ch);
    const body = Buffer.concat(chunks).toString("utf-8");

    const headers = {};
    for (const [k, v] of Object.entries(req.headers)) {
      if (PASS_HEADERS.has(k.toLowerCase())) headers[k.toLowerCase()] = Array.isArray(v) ? v[0] : v;
    }

    const spec = { path: req.url, method: req.method, headers, body: body || null };
    try {
      // 与实测成功版本（prism_inbrowser_v2）完全一致的内联 evaluate fetch。
      // 注意：不走 exposeFunction —— 对照实验证明 exposeFunction 链路会被
      // Prism 校验拒绝（401），原因未明（疑似 isolated world 上下文差异）。
      const result = await page.evaluate(async (s) => {
        const r = await fetch("https://prism.openai.com" + s.path, {
          method: s.method,
          headers: s.headers,
          credentials: "include",
          body: s.body === null || s.body === "" ? undefined : s.body,
        });
        const headers = {};
        r.headers.forEach((v, k) => { headers[k] = v; });
        const body = await r.text();
        return { status: r.status, headers, body };
      }, spec);
      res.writeHead(result.status, { "Content-Type": result.headers["content-type"] || "application/json" });
      res.end(result.body);
    } catch (e) {
      res.writeHead(502, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "sidecar: " + String(e).slice(0, 200) }));
    }
  });

  server.listen(PORT, "127.0.0.1", () => {
    console.log("[sidecar] listening http://127.0.0.1:" + PORT + " -> " + TARGET);
    if (cookieString) importAccount(cookieString);
  });
})().catch((e) => { console.error("[sidecar] FATAL", e); process.exit(1); });
