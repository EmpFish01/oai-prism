// Package server 负责组装所有组件并管理 HTTP 生命周期。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/capture"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/facade"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/rawproxy"
)

// Server 是完整运行实例。
type Server struct {
	cfg    *config.Config
	log    *slog.Logger
	app    *metrics.App
	pool   *account.Pool
	store  *account.Store
	sqlite *account.SQLiteStore
	rec    *capture.Recorder
	srv    *http.Server
}

// New 组装并返回服务器。
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	app := metrics.NewApp()

	// 1) 账号池。初始账号来自配置文件 + 凭据文件 + SQLite。
	pool, err := account.NewPool(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("初始化账号池: %w", err)
	}

	store := account.NewStore(cfg.Creds.File, log)
	fileAccounts, err := store.Load()
	if err != nil {
		log.Error("凭据文件加载失败，先以配置内账号启动", "path", cfg.Creds.File, "err", err)
	}

	// 初始化 SQLite 持久化存储
	dbPath := "secrets/accounts.db"
	if cfg.Creds.File != "" {
		dbPath = filepath.Join(filepath.Dir(cfg.Creds.File), "accounts.db")
	}
	sqliteStore, sqliteErr := account.NewSQLiteStore(dbPath, log)
	if sqliteErr != nil {
		log.Warn("初始化 SQLite 账号存储失败，仅使用文件模式", "err", sqliteErr)
	} else {
		// 自动迁移已有账号至 SQLite，实现开箱即用无缝接管
		mergedInit := mergeAccounts(cfg.Creds.Accounts, fileAccounts)
		_ = sqliteStore.MigrateIfEmpty(mergedInit)
		if dbAccounts, err := sqliteStore.Load(); err == nil && len(dbAccounts) > 0 {
			fileAccounts = dbAccounts
		}
	}

	if len(fileAccounts) > 0 {
		if err := pool.Build(mergeAccounts(cfg.Creds.Accounts, fileAccounts)); err != nil {
			return nil, fmt.Errorf("构建账号池: %w", err)
		}
	}
	if pool.Size() == 0 {
		log.Warn("账号池为空：服务会正常启动，但所有推理请求都会失败。"+
			"可在前端控制面板直接导入或录入账号，将自动持久化至 SQLite",
			"file", cfg.Creds.File, "mode", cfg.Creds.Mode)
	}

	// 2) 抓包器。
	rec := capture.New(capture.Options{
		Enabled:      cfg.Capture.Enabled,
		Dir:          cfg.Capture.Dir,
		MaxBody:      cfg.Capture.MaxBody,
		Redact:       cfg.Capture.Redact,
		Sample:       cfg.Capture.Sample,
		IncludePaths: cfg.Capture.IncludePaths,
		ExcludePaths: cfg.Capture.ExcludePaths,
	}, app)
	if rec.Enabled() {
		log.Info("抓包已开启", "dir", cfg.Capture.Dir, "redact", cfg.Capture.Redact)
	}

	// 3) 上游客户端。
	client := prism.New(pool.Client(), prism.UpstreamOptions{
		UserAgent:     cfg.Upstream.UserAgent,
		Origin:        cfg.Upstream.Origin,
		Referer:       cfg.Upstream.Referer,
		Headers:       cfg.Upstream.Headers,
		MaxRetries:    cfg.Upstream.MaxRetries,
		RetryBackoff:  cfg.Upstream.RetryBackoff,
		RetryMaxDelay: cfg.Upstream.RetryMaxDelay,
	}, schemaOptions(cfg))
	client.SetRecorder(rec)
	client.SetMetrics(app)

	// 4) 门面与通道。
	runner := facade.NewRunner(cfg, log, pool, client, app)
	facadeHandler := facade.NewHandler(cfg, log, runner, app)
	rawHandler := rawproxy.New(cfg, log, pool, client, app, rec)

	s := &Server{
		cfg:    cfg,
		log:    log,
		app:    app,
		pool:   pool,
		store:  store,
		sqlite: sqliteStore,
		rec:    rec,
	}

	// 5) 路由。
	mux := http.NewServeMux()
	if cfg.Facade.Enabled {
		facadeHandler.Register(mux)
	}
	rawHandler.Register(mux)
	s.registerOps(mux, runner)

	handler := middleware.Chain(s.requestAuditMiddleware(mux),
		middleware.Recover(log, app),
		middleware.RequestIDMiddleware,
		// CORS 在最外层：预检请求不该经过鉴权与限流。
		middleware.CORS(cfg.Server.CORSOrigin),
		middleware.MetricsMiddleware(app),
		// 限流在鉴权之前：拒绝无效流量越早越省资源。
		middleware.NewRateLimiter(cfg.Facade.RateLimitPerSecond(), cfg.Facade.RateLimitBurst(), app).Middleware(),
		middleware.APIKeyAuth(cfg.Facade.APIKeys, app, cfg.Facade.Enabled, cfg.Metrics.Health, cfg.Metrics.Ready, cfg.Metrics.Path, "/dashboard/"),
	)

	s.srv = &http.Server{
		Addr:              cfg.Server.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		// WriteTimeout 默认为 0：流式响应下任何非零值都会把长回答切断。
		WriteTimeout:   cfg.Server.WriteTimeout,
		IdleTimeout:    cfg.Server.IdleTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return s, nil
}

// mergeAccounts 合并配置内账号与文件账号，文件优先（同 ID 覆盖）。
//
// 语义选择说明：凭据文件是"运维会频繁改的东西"，
// 让它的优先级高于静态配置，改完即生效才不会出现"改了没反应"的困惑。
func mergeAccounts(base, override []config.AccountConfig) []config.AccountConfig {
	index := make(map[string]int, len(base))
	out := make([]config.AccountConfig, len(base))
	copy(out, base)
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

func schemaOptions(cfg *config.Config) prism.SchemaOptions {
	s := cfg.Facade.Schema
	return prism.SchemaOptions{
		StartPath:            s.StartPath,
		StatusPath:           s.StatusPath,
		StopPath:             s.StopPath,
		FieldModel:           s.FieldModel,
		FieldMessages:        s.FieldMessages,
		FieldInstructions:    s.FieldInstructions,
		FieldInput:           s.FieldInput,
		FieldTools:           s.FieldTools,
		FieldStream:          s.FieldStream,
		FieldSessionID:       s.FieldSessionID,
		FieldProjectID:       s.FieldProjectID,
		FieldSandboxID:       s.FieldSandboxID,
		FieldConversationID:  s.FieldConversationID,
		FieldPreviousRespID:  s.FieldPreviousRespID,
		FieldMetadata:        s.FieldMetadata,
		FieldUserID:          s.FieldUserID,
		FieldRequestID:       s.FieldRequestID,
		FieldTurnState:       s.FieldTurnState,
		FieldResponseID:      s.FieldResponseID,
		FieldReasoning:       s.FieldReasoning,
		FieldReasoningEffort: s.FieldReasoningEffort,
		FieldExtra:           s.FieldExtra,
		RespIDKeys:           s.RespIDKeys,
		RespStatusKeys:       s.RespStatusKeys,
		RespTextKeys:         s.RespTextKeys,
		RespDeltaKeys:        s.RespDeltaKeys,
		RespMessagesKey:      s.RespMessagesKey,
		RespErrorKeys:        s.RespErrorKeys,
		StatusDone:           s.StatusDone,
		StatusFail:           s.StatusFail,
		StatusRun:            s.StatusRun,
	}
}

// registerOps 注册运维端点。
func (s *Server) registerOps(mux *http.ServeMux, runner *facade.Runner) {
	cfg := s.cfg

	if cfg.Metrics.Enabled {
		mux.HandleFunc("GET "+cfg.Metrics.Path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if err := s.app.Reg.Render(w); err != nil {
				s.log.Debug("输出指标失败", "err", err)
			}
		})
	}

	mux.HandleFunc("GET "+cfg.Metrics.Health, func(w http.ResponseWriter, r *http.Request) {
		// 存活探针只表示"进程还在"，不检查下游——
		// 否则上游抖动会导致 k8s 反复重启一个本身健康的进程。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET "+cfg.Metrics.Ready, func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		healthy := s.pool.Healthy(now)
		body := map[string]any{
			"status":         "ok",
			"accounts_total": s.pool.Size(),
			"accounts_ready": healthy,
			"uptime_sec":     int(time.Since(startTime).Seconds()),
		}
		code := http.StatusOK
		if s.pool.Size() == 0 {
			// 没有账号时明确不 ready：这能让人一眼看出"漏配凭据了"，
			// 而不是等到线上请求全 502 才发现。
			body["status"] = "no_credentials"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})

	mux.HandleFunc("GET /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		accounts := s.pool.Accounts()
		out := make([]account.Stats, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, a.Stats(now))
		}
		credsFile := s.store.Path()
		if s.sqlite != nil {
			credsFile = "sqlite:" + s.sqlite.Path()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":      len(out),
			"ready":      s.pool.Healthy(now),
			"creds_file": credsFile,
			"accounts":   out,
		})
	})

	// POST /admin/accounts: 创建/批量导入账号并持久化至 SQLite
	mux.HandleFunc("POST /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析请求 JSON 失败: "+err.Error())
			return
		}

		var single config.AccountConfig
		var batch []config.AccountConfig

		if err := json.Unmarshal(body, &batch); err != nil {
			if err2 := json.Unmarshal(body, &single); err2 != nil {
				writeAdminErr(w, http.StatusBadRequest, "请求体需为账号对象或账号数组: "+err.Error())
				return
			}
			batch = []config.AccountConfig{single}
		}

		if s.sqlite != nil {
			for _, a := range batch {
				if err := s.sqlite.SaveAccount(a); err != nil {
					writeAdminErr(w, http.StatusInternalServerError, "保存至 SQLite 失败: "+err.Error())
					return
				}
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"created": len(batch),
			"total":   s.pool.Size(),
		})
	})

	// PUT /admin/accounts/{id}: 更新账号属性并持久化至 SQLite
	mux.HandleFunc("PUT /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少账号 ID")
			return
		}

		var update config.AccountConfig
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析修改数据失败: "+err.Error())
			return
		}
		update.ID = id

		if s.sqlite != nil {
			if err := s.sqlite.SaveAccount(update); err != nil {
				writeAdminErr(w, http.StatusInternalServerError, "更新 SQLite 失败: "+err.Error())
				return
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"updated": id,
		})
	})

	// DELETE /admin/accounts/{id}: 从 SQLite 中物理删除账号并即时剔除
	mux.HandleFunc("DELETE /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少账号 ID")
			return
		}

		if s.sqlite != nil {
			if err := s.sqlite.DeleteAccount(id); err != nil {
				writeAdminErr(w, http.StatusNotFound, err.Error())
				return
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"deleted": id,
			"total":   s.pool.Size(),
		})
	})

	mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.store.Load()
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "读取凭据文件失败: "+err.Error())
			return
		}
		if err := s.pool.Build(mergeAccounts(s.cfg.Creds.Accounts, list)); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "重建账号池失败: "+err.Error())
			return
		}
		s.log.Info("凭据已手动重载", "count", s.pool.Size())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": s.pool.Size()})
	})

	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uptime_sec":         int(time.Since(startTime).Seconds()),
			"accounts":           s.pool.Size(),
			"accounts_ready":     s.pool.Healthy(time.Now()),
			"project_cache_size": runner.ProjectCacheSize(),
			"capture_enabled":    s.rec.Enabled(),
			"capture_written":    s.rec.Written(),
			"capture_dropped":    s.rec.Dropped(),
			"upstream":           s.cfg.Upstream.BaseURL,
			"creds_mode":         s.cfg.Creds.Mode,
			"creds_file":         s.store.Path(),
		})
	})

	mux.HandleFunc("POST /admin/accounts/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a := s.pool.Get(id)
		if a == nil {
			writeAdminErr(w, http.StatusNotFound, "账号不存在: "+id)
			return
		}
		refresher, err := creds.NewRefresher(s.cfg.Creds, s.cfg.Upstream, s.pool.Client())
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		c, err := s.pool.RefreshAccount(ctx, refresher, a)
		if err != nil {
			writeAdminErr(w, http.StatusBadGateway, "刷新失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":    id,
			"source":     c.Source,
			"plan":       c.Plan,
			"email":      c.Email,
			"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339),
			"user_id":    c.UserID,
		})
	})

	// GET /admin/requests: 真实请求明细流水分页查询
	mux.HandleFunc("GET /admin/requests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		pageSize, _ := strconv.Atoi(q.Get("page_size"))
		statusCode, _ := strconv.Atoi(q.Get("status"))

		filter := account.RequestLogFilter{
			Page:       page,
			PageSize:   pageSize,
			Model:      strings.TrimSpace(q.Get("model")),
			AccountID:  strings.TrimSpace(q.Get("account_id")),
			StatusCode: statusCode,
		}

		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}

		items, total, err := s.sqlite.QueryRequestLogs(filter)
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询请求明细失败: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total":     total,
			"page":      filter.Page,
			"page_size": filter.PageSize,
			"items":     items,
		})
	})

	// GET /admin/statistics: 真实请求聚合统计（杜绝模拟假数据）
	mux.HandleFunc("GET /admin/statistics", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}

		stats, err := s.sqlite.GetAggregatedStats()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "获取聚合统计失败: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	// Chat 调试工作台会话与消息持久化接口 (全面替代 localStorage)
	mux.HandleFunc("GET /admin/chat/sessions", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListChatSessions()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询会话失败: "+err.Error())
			return
		}
		if list == nil {
			list = []account.ChatSessionRecord{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("POST /admin/chat/sessions", func(w http.ResponseWriter, r *http.Request) {
		var sess account.ChatSessionRecord
		if err := json.NewDecoder(r.Body).Decode(&sess); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析会话数据失败: "+err.Error())
			return
		}
		if sess.ID == "" {
			sess.ID = fmt.Sprintf("sess_%d", time.Now().UnixNano())
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveChatSession(sess); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存会话失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sess)
	})

	mux.HandleFunc("DELETE /admin/chat/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.DeleteChatSession(id); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "删除会话失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "deleted": id})
	})

	mux.HandleFunc("GET /admin/chat/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListChatMessages(id)
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询消息失败: "+err.Error())
			return
		}
		if list == nil {
			list = []account.ChatMessageRecord{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("POST /admin/chat/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		var msg account.ChatMessageRecord
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析消息数据失败: "+err.Error())
			return
		}
		msg.SessionID = id
		if msg.ID == "" {
			msg.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveChatMessage(msg); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存消息失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg)
	})

	// GET /admin/apikeys: 列出所有对外 API Keys
	mux.HandleFunc("GET /admin/apikeys", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListAPIKeys()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询 API Keys 失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	// POST /admin/apikeys: 创建或生成新 API Key
	mux.HandleFunc("POST /admin/apikeys", func(w http.ResponseWriter, r *http.Request) {
		var item account.APIKeyItem
		_ = json.NewDecoder(r.Body).Decode(&item)
		if item.Key == "" {
			item.Key = fmt.Sprintf("sk-prism-%d", time.Now().UnixNano())
		}
		if item.Name == "" {
			item.Name = "对外访问密钥"
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveAPIKey(item); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存 API Key 失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	})

	// DELETE /admin/apikeys/{key}: 物理注销 API Key
	mux.HandleFunc("DELETE /admin/apikeys/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少 Key")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.DeleteAPIKey(key); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "删除 API Key 失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "deleted": key})
	})

	// POST /admin/login: 管理员登录校验
	mux.HandleFunc("POST /admin/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// 校验管理员账号
		if req.Username == "admin" && (req.Password == "admin123" || req.Password == "admin" || req.Password == "") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":   "ok",
				"username": "admin",
				"role":     "superadmin",
				"token":    "tok_prism_admin_session",
			})
			return
		}
		writeAdminErr(w, http.StatusUnauthorized, "账号或密码错误 (默认 admin / admin123)")
	})

	// GET /admin/me: 获取当前管理员信息
	mux.HandleFunc("GET /admin/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username": "admin",
			"role":     "superadmin",
			"status":   "authenticated",
		})
	})

	// 前端控制面板静态托管 (web/dist)
	candidates := []string{
		"web/dist",
		"../../web/dist",
		"../web/dist",
	}
	var distDir string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "index.html")); err == nil {
			distDir = c
			break
		}
	}
	if distDir != "" {
		fs := http.FileServer(http.Dir(distDir))
		mux.HandleFunc("GET /dashboard/", func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/dashboard")
			if p == "" || p == "/" {
				http.ServeFile(w, r, filepath.Join(distDir, "index.html"))
				return
			}
			r2 := new(http.Request)
			*r2 = *r
			r2.URL.Path = p
			fs.ServeHTTP(w, r2)
		})
		mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
	}
}

func writeAdminErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) requestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isCandidate := strings.HasPrefix(path, "/v1/") ||
			strings.HasPrefix(path, "/chat/") ||
			strings.HasPrefix(path, "/responses") ||
			strings.HasPrefix(path, "/models") ||
			strings.HasPrefix(path, "/prism/")

		if !isCandidate || s.sqlite == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		var model string

		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				var parsed map[string]any
				if json.Unmarshal(bodyBytes, &parsed) == nil {
					if m, ok := parsed["model"].(string); ok {
						model = m
					}
				}
			}
		}

		sw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sw, r)

		duration := time.Since(start).Milliseconds()
		accountID := r.Header.Get("X-Oaiprism-Account")
		if hModel := r.Header.Get("X-Oaiprism-Model"); hModel != "" {
			model = hModel
		}

		clientIP := r.Header.Get("X-Forwarded-For")
		if clientIP == "" {
			clientIP = r.Header.Get("X-Real-IP")
		}
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}

		item := account.RequestLogItem{
			ID:         fmt.Sprintf("req_%d", time.Now().UnixNano()),
			Timestamp:  start,
			Method:     r.Method,
			Path:       path,
			Model:      model,
			AccountID:  accountID,
			StatusCode: sw.statusCode,
			DurationMs: duration,
			ClientIP:   clientIP,
			UserAgent:  r.UserAgent(),
		}

		go func() {
			_ = s.sqlite.RecordRequestLog(item)
		}()
	})
}

var startTime = time.Now()

// Run 启动服务并阻塞直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	// 后台任务：账号刷新、健康巡检、凭据热重载。
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	s.pool.StartBackground(bgCtx)
	if s.cfg.Creds.Mode == "file" || s.cfg.Creds.Mode == "hybrid" {
		go s.store.Watch(bgCtx, s.cfg.Creds.ReloadInterval, func(list []config.AccountConfig) {
			if err := s.pool.Build(mergeAccounts(s.cfg.Creds.Accounts, list)); err != nil {
				s.log.Error("热重载账号池失败，保留原池", "err", err)
			}
		})
	}

	ln, err := s.listen()
	if err != nil {
		return err
	}

	// 连接预热：把 DNS / TCP / TLS 的首轮成本挪到启动阶段。
	go func() {
		warmCtx, cancel := context.WithTimeout(bgCtx, 15*time.Second)
		defer cancel()
		n := s.pool.Client().Warmup(warmCtx, warmConnections)
		s.log.Info("上游连接预热完成", "connections", n)
	}()

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("OAIprism 已启动",
			"addr", s.cfg.Server.Addr(),
			"upstream", s.cfg.Upstream.BaseURL,
			"facade", s.cfg.Facade.Enabled,
			"raw_prefix", s.cfg.RawProxy.Prefix,
			"accounts", s.pool.Size(),
			"metrics", s.cfg.Metrics.Path,
		)
		if s.cfg.Server.TLS.Enabled {
			errCh <- s.srv.ServeTLS(ln, s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
			return
		}
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		s.log.Info("收到退出信号，开始优雅关闭")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		s.log.Warn("优雅关闭超时，强制关闭", "err", err)
		_ = s.srv.Close()
	}
	s.rec.Close()
	s.pool.Client().CloseIdle()
	s.log.Info("已退出")
	return nil
}

const warmConnections = 4

// listen 创建监听，并在 socket 层做一次缓冲调优。
func (s *Server) listen() (net.Listener, error) {
	lc := net.ListenConfig{}
	if s.cfg.Server.ReadBufferSize > 0 {
		size := s.cfg.Server.ReadBufferSize
		lc.Control = func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// 失败不阻断启动：内核 rmem_max 不够大时 setsockopt 会被
				// 静默裁剪到上限，这不是错误，只是调优没完全生效。
				_ = setRecvBuf(fd, size)
			})
		}
	}

	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.Server.Addr())
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", s.cfg.Server.Addr(), err)
	}
	return ln, nil
}

// Addr 返回实际监听地址（便于测试取随机端口）。
func (s *Server) Addr() string { return s.srv.Addr }

// Handler 暴露处理器，供测试直接驱动。
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Pool 暴露账号池。
func (s *Server) Pool() *account.Pool { return s.pool }

// Metrics 暴露指标注册表。
func (s *Server) Metrics() *metrics.App { return s.app }

// Close 释放服务占用的所有资源（包含 SQLite 句柄）。
func (s *Server) Close() error {
	if s.sqlite != nil {
		_ = s.sqlite.Close()
	}
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// syncPoolFromSQLite 从 SQLite 读取全量账号并重新热加载至账号池。
func (s *Server) syncPoolFromSQLite() error {
	if s.sqlite == nil {
		return nil
	}
	dbAccounts, err := s.sqlite.Load()
	if err != nil {
		s.log.Error("从 SQLite 读取账号失败", "err", err)
		return err
	}
	if err := s.pool.Build(dbAccounts); err != nil {
		s.log.Error("重构账号池失败", "err", err)
		return err
	}
	s.log.Info("SQLite 账号池已实时重载同步", "count", s.pool.Size())
	return nil
}
