// Command oaiprism 是 OAIprism 的命令行入口。
//
// 子命令：
//
//	serve            启动网关
//	probe            逐个账号向上游校验凭据是否可用
//	import           校验并写入一个账号（secrets/accounts.json + SQLite）
//	capture-summary  汇总抓包文件，用于校准 schema 字段名
//	version          打印版本
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/capture"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/logx"
	"github.com/oai-prism/oaiprism/internal/server"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

const usage = `OAIprism — prism.openai.com 反向代理网关

用法:
  oaiprism serve           [-config FILE] [-host H] [-port N] [-debug]
  oaiprism probe           [-config FILE] [-id ID]
  oaiprism import          [-config FILE] -id ID (-cookie S | -session-token S | -access-token S | -refresh-token S | -stdin) [-no-verify]
  oaiprism capture-summary -file FILE
  oaiprism version

不带子命令时等同于 serve。
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "probe":
		err = runProbe(args)
	case "import":
		err = runImport(args)
	case "capture-summary":
		err = runCaptureSummary(args)
	case "version", "-version", "--version":
		fmt.Println("oaiprism", version)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// defaultConfigPath 优先用 configs/config.yaml，不存在时返回空串（纯默认值 + 环境变量）。
func defaultConfigPath() string {
	if v := os.Getenv(config.EnvPrefix + "CONFIG"); v != "" {
		return v
	}
	if _, err := os.Stat("configs/config.yaml"); err == nil {
		return "configs/config.yaml"
	}
	return ""
}

// loadConfig 读取配置并把数据路径解析成绝对路径。
//
// config.Load 本身不解析相对路径，规则见 config.ResolveDataPath：
// 凭据文件"哪里存在就用哪里"，抓包目录落在工作目录下。
func loadConfig(path string) (*config.Config, string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	baseDir := "."
	if path != "" {
		baseDir = filepath.Dir(path)
	}
	how := ""
	if cfg.Creds.File != "" {
		cfg.Creds.File, how = config.ResolveDataPath(baseDir, cfg.Creds.File)
	}
	if cfg.Capture.Dir != "" {
		cfg.Capture.Dir = config.ResolveOutputDir(baseDir, cfg.Capture.Dir)
	}
	return cfg, how, nil
}

// ---------------------------- serve ----------------------------

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	host := fs.String("host", "", "覆盖 server.host")
	port := fs.Int("port", 0, "覆盖 server.port")
	debug := fs.Bool("debug", false, "debug 日志")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, credsHow, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *host != "" {
		cfg.Server.Host = *host
	}
	if *port > 0 {
		cfg.Server.Port = *port
	}
	if *debug {
		cfg.Log.Level = "debug"
	}

	log := logx.Setup(cfg.Log.Level, cfg.Log.Format)
	log.Info("加载配置", "version", version, "config", *cfgPath,
		"creds_file", cfg.Creds.File, "creds_resolved_by", credsHow)

	if len(cfg.Facade.APIKeys) == 0 {
		if isLoopback(cfg.Server.Host) {
			log.Warn("facade.api_keys 为空：任何本机进程都能调用 /v1、/admin 与 /prism 通道")
		} else {
			log.Warn("facade.api_keys 为空且监听非回环地址：账号凭据对整个网络开放，务必设置 API Key",
				"host", cfg.Server.Host)
		}
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

func isLoopback(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// ---------------------------- probe ----------------------------

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	only := fs.String("id", "", "只检查这个账号")
	timeout := fs.Duration("timeout", 30*time.Second, "单账号超时")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	log := logx.Setup("warn", "text")

	accounts := collectAccounts(cfg, log)
	if len(accounts) == 0 {
		return fmt.Errorf("没有任何账号（creds_file=%s）；先运行 oaiprism import", cfg.Creds.File)
	}

	refresher, err := creds.NewRefresher(cfg.Creds, cfg.Upstream, nil)
	if err != nil {
		return err
	}

	failed := 0
	checked := 0
	for _, a := range accounts {
		if *only != "" && a.ID != *only {
			continue
		}
		checked++
		if !a.IsEnabled() {
			fmt.Printf("%-16s 跳过（已禁用）\n", a.ID)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		c, err := refresher.Refresh(ctx, creds.FromAccountConfig(a))
		cancel()
		if err != nil {
			failed++
			fmt.Printf("%-16s 失败  %v\n", a.ID, err)
			continue
		}
		fmt.Printf("%-16s 可用  email=%s plan=%s expires=%s source=%s\n",
			a.ID, orDash(c.Email), orDash(c.Plan), fmtTime(c.ExpiresAt), c.Source)
	}
	if checked == 0 {
		return fmt.Errorf("找不到账号 %q", *only)
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号不可用", failed)
	}
	return nil
}

// collectAccounts 汇总配置内、凭据文件、SQLite 三处的账号，与 server.New 的合并顺序一致。
func collectAccounts(cfg *config.Config, log *slog.Logger) []config.AccountConfig {
	list, err := account.NewStore(cfg.Creds.File, log).Load()
	if err != nil {
		log.Warn("读取凭据文件失败", "path", cfg.Creds.File, "err", err)
	}
	if db, err := openSQLite(cfg, log); err == nil {
		if dbList, err := db.Load(); err == nil && len(dbList) > 0 {
			list = mergeByID(list, dbList)
		}
		_ = db.Close()
	}
	return mergeByID(cfg.Creds.Accounts, list)
}

// openSQLite 只打开已存在的数据库，避免 probe 凭空建库。
func openSQLite(cfg *config.Config, log *slog.Logger) (*account.SQLiteStore, error) {
	p := sqlitePath(cfg)
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	return account.NewSQLiteStore(p, log)
}

// sqlitePath 与 server.New 的推导规则保持一致。
func sqlitePath(cfg *config.Config) string {
	if cfg.Creds.File == "" {
		return "secrets/accounts.db"
	}
	return filepath.Join(filepath.Dir(cfg.Creds.File), "accounts.db")
}

func mergeByID(base, override []config.AccountConfig) []config.AccountConfig {
	out := append([]config.AccountConfig(nil), base...)
	index := make(map[string]int, len(out))
	for i, a := range out {
		index[a.ID] = i
	}
	for _, a := range override {
		if i, ok := index[a.ID]; ok {
			out[i] = a
			continue
		}
		index[a.ID] = len(out)
		out = append(out, a)
	}
	return out
}

// ---------------------------- import ----------------------------

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	id := fs.String("id", "main", "账号 ID（同 ID 覆盖）")
	name := fs.String("name", "", "展示名，默认同 ID")
	cookie := fs.String("cookie", "", "浏览器复制的整串 Cookie")
	sessionToken := fs.String("session-token", "", "会话 cookie 的值")
	accessToken := fs.String("access-token", "", "JWT access token")
	refreshToken := fs.String("refresh-token", "", "OAuth refresh token")
	stdin := fs.Bool("stdin", false, "从标准输入读取凭据（避免进 shell 历史）")
	kind := fs.String("kind", "auto", "-stdin 读到的内容类型：auto|cookie|session|access|refresh")
	maxConc := fs.Int("max-concurrency", 2, "单账号并发上限")
	proxy := fs.String("proxy", "", "账号级出站代理")
	noVerify := fs.Bool("no-verify", false, "跳过上游校验，直接写入")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("-id 不能为空")
	}

	a := config.AccountConfig{
		ID:             strings.TrimSpace(*id),
		Name:           strings.TrimSpace(*name),
		Cookies:        strings.TrimSpace(*cookie),
		SessionToken:   strings.TrimSpace(*sessionToken),
		AccessToken:    strings.TrimSpace(*accessToken),
		RefreshToken:   strings.TrimSpace(*refreshToken),
		Proxy:          strings.TrimSpace(*proxy),
		MaxConcurrency: *maxConc,
	}
	if a.Name == "" {
		a.Name = a.ID
	}
	if *stdin {
		raw, err := readSecret(os.Stdin)
		if err != nil {
			return err
		}
		if err := assignSecret(&a, *kind, raw); err != nil {
			return err
		}
	}
	if a.Cookies == "" && a.SessionToken == "" && a.AccessToken == "" && a.RefreshToken == "" {
		return fmt.Errorf("没有凭据：用 -cookie / -session-token / -access-token / -refresh-token / -stdin 之一提供")
	}

	cfg, _, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Creds.File == "" {
		return fmt.Errorf("creds.file 未配置，无处写入")
	}
	log := logx.Setup("warn", "text")

	if !*noVerify {
		refresher, err := creds.NewRefresher(cfg.Creds, cfg.Upstream, nil)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		c, err := refresher.Refresh(ctx, creds.FromAccountConfig(a))
		cancel()
		if err != nil {
			return fmt.Errorf("上游校验失败（可加 -no-verify 强制写入）: %w", err)
		}
		// 只补元信息与换回来的长效材料，原始 Cookie 保持不动。
		a.Email = firstNonEmpty(a.Email, c.Email)
		a.Plan = firstNonEmpty(a.Plan, c.Plan)
		a.AccountID = firstNonEmpty(a.AccountID, c.AccountID)
		if a.RefreshToken == "" && c.RefreshToken != "" {
			a.RefreshToken = c.RefreshToken
		}
		if a.AccessToken == "" && a.Cookies == "" && a.SessionToken == "" {
			a.AccessToken = c.AccessToken
		}
		fmt.Printf("校验通过  email=%s plan=%s expires=%s\n", orDash(c.Email), orDash(c.Plan), fmtTime(c.ExpiresAt))
	}

	store := account.NewStore(cfg.Creds.File, log)
	existing, err := store.Load()
	if err != nil {
		return fmt.Errorf("读取现有凭据文件: %w", err)
	}
	if err := store.Persist(mergeByID(existing, []config.AccountConfig{a})); err != nil {
		return fmt.Errorf("写入凭据文件: %w", err)
	}
	fmt.Printf("已写入 %s（账号 %s）\n", cfg.Creds.File, a.ID)

	// 服务首次启动后以 SQLite 为准，只写 JSON 的话重启就会丢。
	db, err := account.NewSQLiteStore(sqlitePath(cfg), log)
	if err != nil {
		return fmt.Errorf("打开 SQLite: %w", err)
	}
	defer db.Close()
	if err := db.SaveAccount(a); err != nil {
		return err
	}
	fmt.Printf("已同步 %s\n", db.Path())
	return nil
}

// readSecret 读取标准输入的第一段非空内容，容忍末尾换行与 BOM。
func readSecret(r io.Reader) (string, error) {
	b, err := io.ReadAll(bufio.NewReader(io.LimitReader(r, 1<<20)))
	if err != nil {
		return "", fmt.Errorf("读取标准输入: %w", err)
	}
	s := strings.TrimSpace(strings.TrimPrefix(string(b), string(rune(0xFEFF))))
	if s == "" {
		return "", fmt.Errorf("标准输入为空")
	}
	return s, nil
}

func assignSecret(a *config.AccountConfig, kind, raw string) error {
	if kind == "auto" {
		switch {
		case looksLikeJWT(raw):
			kind = "access"
		case strings.Contains(raw, "="):
			kind = "cookie"
		default:
			return fmt.Errorf("无法判断标准输入的凭据类型，请用 -kind cookie|session|access|refresh 指明")
		}
	}
	switch kind {
	case "cookie":
		a.Cookies = raw
	case "session":
		a.SessionToken = raw
	case "access":
		a.AccessToken = strings.TrimPrefix(raw, "Bearer ")
	case "refresh":
		a.RefreshToken = raw
	default:
		return fmt.Errorf("-kind 非法 %q", kind)
	}
	return nil
}

func looksLikeJWT(s string) bool {
	s = strings.TrimPrefix(s, "Bearer ")
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2 && !strings.ContainsAny(s, "=; ")
}

// ---------------------------- capture-summary ----------------------------

func runCaptureSummary(args []string) error {
	fs := flag.NewFlagSet("capture-summary", flag.ContinueOnError)
	file := fs.String("file", "", "抓包 JSONL 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("需要 -file")
	}
	recs, err := capture.ReplayFile(*file)
	if err != nil {
		return err
	}
	fmt.Print(capture.Summarize(recs))
	return nil
}

// ---------------------------- helpers ----------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
