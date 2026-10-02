package account

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	_ "modernc.org/sqlite"
)

// SQLiteStore 负责通过 SQLite 持久化账号配置，支持动态增删改查 (CRUD)。
type SQLiteStore struct {
	db   *sql.DB
	path string
	log  *slog.Logger
	mu   sync.Mutex
}

// NewSQLiteStore 打开或创建 SQLite 数据库。
func NewSQLiteStore(dbPath string, log *slog.Logger) (*SQLiteStore, error) {
	if dbPath == "" {
		dbPath = "secrets/accounts.db"
	}
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite 数据库失败: %w", err)
	}

	// 优化 SQLite 并发与可靠性（使用 DELETE 模式确保 Windows 下句柄关闭后彻底释放文件）
	db.SetMaxOpenConns(1)
	_, _ = db.Exec("PRAGMA journal_mode=DELETE;")
	_, _ = db.Exec("PRAGMA synchronous=NORMAL;")

	s := &SQLiteStore{
		db:   db,
		path: dbPath,
		log:  log,
	}

	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// initSchema 创建 accounts、request_logs、chat_sessions 和 chat_messages 表。
func (s *SQLiteStore) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		plan TEXT DEFAULT 'pro',
		email TEXT DEFAULT '',
		cookies TEXT DEFAULT '',
		access_token TEXT DEFAULT '',
		refresh_token TEXT DEFAULT '',
		max_concurrency INTEGER DEFAULT 2,
		tags TEXT DEFAULT '',
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS request_logs (
		id TEXT PRIMARY KEY,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		model TEXT DEFAULT '',
		account_id TEXT DEFAULT '',
		status_code INTEGER DEFAULT 200,
		duration_ms INTEGER DEFAULT 0,
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		error_message TEXT DEFAULT '',
		client_ip TEXT DEFAULT '',
		user_agent TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_req_time ON request_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_req_model ON request_logs(model);
	CREATE INDEX IF NOT EXISTS idx_req_account ON request_logs(account_id);

	CREATE TABLE IF NOT EXISTS chat_sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		model TEXT NOT NULL,
		reasoning_effort TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS chat_messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT DEFAULT '',
		reasoning TEXT DEFAULT '',
		status TEXT DEFAULT 'success',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_msg_session ON chat_messages(session_id);

	CREATE TABLE IF NOT EXISTS api_keys (
		key TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("初始化 sqlite 表结构失败: %w", err)
	}

	// 初始化默认主密钥
	var keyCount int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&keyCount)
	if keyCount == 0 {
		_, _ = s.db.Exec("INSERT INTO api_keys (key, name, created_at) VALUES (?, ?, CURRENT_TIMESTAMP)",
			"sk-prism-live-master", "系统默认主调用密钥")
	}

	return nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// Path 返回数据库路径。
func (s *SQLiteStore) Path() string {
	return s.path
}

// Load 读取 SQLite 中所有已保存的账号。
func (s *SQLiteStore) Load() ([]config.AccountConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT id, name, plan, email, cookies, access_token, refresh_token, max_concurrency, tags
		FROM accounts
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("查询 accounts 失败: %w", err)
	}
	defer rows.Close()

	var list []config.AccountConfig
	for rows.Next() {
		var a config.AccountConfig
		var tagsStr string
		err := rows.Scan(
			&a.ID,
			&a.Name,
			&a.Plan,
			&a.Email,
			&a.Cookies,
			&a.AccessToken,
			&a.RefreshToken,
			&a.MaxConcurrency,
			&tagsStr,
		)
		if err != nil {
			return nil, fmt.Errorf("读取账号行失败: %w", err)
		}
		if tagsStr != "" {
			_ = json.Unmarshal([]byte(tagsStr), &a.Tags)
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// SaveAccount 插入或更新单账号 (Create or Update)。
func (s *SQLiteStore) SaveAccount(a config.AccountConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(a.ID) == "" {
		a.ID = fmt.Sprintf("acc_%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(a.Name) == "" {
		a.Name = a.ID
	}
	if strings.TrimSpace(a.Plan) == "" {
		a.Plan = "pro"
	}
	if a.MaxConcurrency <= 0 {
		a.MaxConcurrency = 2
	}

	tagsJSON := "[]"
	if len(a.Tags) > 0 {
		if b, err := json.Marshal(a.Tags); err == nil {
			tagsJSON = string(b)
		}
	}

	query := `
	INSERT INTO accounts (id, name, plan, email, cookies, access_token, refresh_token, max_concurrency, tags, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		plan = excluded.plan,
		email = excluded.email,
		cookies = CASE WHEN excluded.cookies != '' THEN excluded.cookies ELSE accounts.cookies END,
		access_token = CASE WHEN excluded.access_token != '' THEN excluded.access_token ELSE accounts.access_token END,
		refresh_token = CASE WHEN excluded.refresh_token != '' THEN excluded.refresh_token ELSE accounts.refresh_token END,
		max_concurrency = excluded.max_concurrency,
		tags = excluded.tags,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query,
		a.ID,
		a.Name,
		a.Plan,
		a.Email,
		a.Cookies,
		a.AccessToken,
		a.RefreshToken,
		a.MaxConcurrency,
		tagsJSON,
	)
	if err != nil {
		return fmt.Errorf("保存账号 %s 至 SQLite 失败: %w", a.ID, err)
	}
	return nil
}

// DeleteAccount 从 SQLite 中物理删除账号 (Delete)。
func (s *SQLiteStore) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec("DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("从 SQLite 删除账号 %s 失败: %w", id, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("账号不存在: %s", id)
	}
	return nil
}

// MigrateIfEmpty 如果 SQLite 为空，自动把现有列表迁移进来。
func (s *SQLiteStore) MigrateIfEmpty(existing []config.AccountConfig) error {
	s.mu.Lock()
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	s.mu.Unlock()

	if count > 0 || len(existing) == 0 {
		return nil
	}

	s.log.Info("SQLite 数据库为空，自动初始化导入已有账号", "count", len(existing))
	for _, a := range existing {
		if a.ID == "" {
			continue
		}
		if err := s.SaveAccount(a); err != nil {
			s.log.Warn("初始化迁移账号失败", "id", a.ID, "err", err)
		}
	}
	return nil
}

// -------------------------------------------------------------
// 请求流水明细 (Request Logs) 持久化与分析
// -------------------------------------------------------------

// RequestLogItem 对应一次真实请求明细流水记录。
type RequestLogItem struct {
	ID               string    `json:"id"`
	Timestamp        time.Time `json:"timestamp"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	AccountID        string    `json:"account_id"`
	StatusCode       int       `json:"status_code"`
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ErrorMessage     string    `json:"error_message"`
	ClientIP         string    `json:"client_ip"`
	UserAgent        string    `json:"user_agent"`
}

// RequestLogFilter 过滤条件
type RequestLogFilter struct {
	Page       int
	PageSize   int
	Model      string
	AccountID  string
	StatusCode int
}

// ModelUsageStat 真实模型使用占比
type ModelUsageStat struct {
	Model        string  `json:"model"`
	Requests     int     `json:"requests"`
	Percentage   float64 `json:"percentage"`
	AvgLatencyMs int64   `json:"avg_latency_ms"`
}

// TimeSeriesPoint 真实时间序列统计点
type TimeSeriesPoint struct {
	Timestamp string  `json:"timestamp"`
	Requests  int     `json:"requests"`
	QPS       float64 `json:"qps"`
	Latency   int64   `json:"latency"`
	ErrorRate float64 `json:"error_rate"`
}

// AggregatedStats 真实聚合统计数据（拒绝假数据）
type AggregatedStats struct {
	TotalRequests int               `json:"total_requests"`
	Failures      int               `json:"failures"`
	SuccessRate   float64           `json:"success_rate"`
	AvgLatencyMs  int64             `json:"avg_latency_ms"`
	ModelUsages   []ModelUsageStat  `json:"model_usages"`
	TimeSeries    []TimeSeriesPoint `json:"time_series"`
}

// RecordRequestLog 将一笔真实请求记录持久化至 SQLite。
func (s *SQLiteStore) RecordRequestLog(item RequestLogItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if item.ID == "" {
		item.ID = fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}

	query := `
	INSERT INTO request_logs (
		id, timestamp, method, path, model, account_id,
		status_code, duration_ms, prompt_tokens, completion_tokens,
		error_message, client_ip, user_agent
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`
	_, err := s.db.Exec(query,
		item.ID,
		item.Timestamp.UTC().Format("2006-01-02 15:04:05"),
		item.Method,
		item.Path,
		item.Model,
		item.AccountID,
		item.StatusCode,
		item.DurationMs,
		item.PromptTokens,
		item.CompletionTokens,
		item.ErrorMessage,
		item.ClientIP,
		item.UserAgent,
	)
	return err
}

// QueryRequestLogs 分页查询请求流水明细。
func (s *SQLiteStore) QueryRequestLogs(filter RequestLogFilter) ([]RequestLogItem, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 10
	}
	offset := (filter.Page - 1) * filter.PageSize

	var conditions []string
	var args []any

	if filter.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, filter.Model)
	}
	if filter.AccountID != "" {
		conditions = append(conditions, "account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.StatusCode > 0 {
		conditions = append(conditions, "status_code = ?")
		args = append(args, filter.StatusCode)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// 统计总数
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM request_logs %s", whereClause)
	var total int
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 分页查询列表
	query := fmt.Sprintf(`
		SELECT id, timestamp, method, path, model, account_id,
		       status_code, duration_ms, prompt_tokens, completion_tokens,
		       error_message, client_ip, user_agent
		FROM request_logs
		%s
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`, whereClause)

	queryArgs := append(args, filter.PageSize, offset)
	rows, err := s.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var list []RequestLogItem
	for rows.Next() {
		var it RequestLogItem
		var tsStr any
		if err := rows.Scan(
			&it.ID,
			&tsStr,
			&it.Method,
			&it.Path,
			&it.Model,
			&it.AccountID,
			&it.StatusCode,
			&it.DurationMs,
			&it.PromptTokens,
			&it.CompletionTokens,
			&it.ErrorMessage,
			&it.ClientIP,
			&it.UserAgent,
		); err != nil {
			continue
		}
		it.Timestamp = dbTime(tsStr)
		list = append(list, it)
	}

	return list, total, nil
}

// GetAggregatedStats 基于 SQLite 真实请求明细计算真实统计指标。
func (s *SQLiteStore) GetAggregatedStats() (*AggregatedStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := &AggregatedStats{
		ModelUsages: []ModelUsageStat{},
		TimeSeries:  []TimeSeriesPoint{},
	}

	// 1. 基础总览
	var total, fails int
	var avgLatency sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		       AVG(duration_ms)
		FROM request_logs
	`).Scan(&total, &fails, &avgLatency)
	if err != nil {
		return stats, err
	}

	stats.TotalRequests = total
	stats.Failures = fails
	if total > 0 {
		stats.SuccessRate = float64(total-fails) / float64(total) * 100
		if avgLatency.Valid {
			stats.AvgLatencyMs = int64(avgLatency.Float64)
		}
	} else {
		stats.SuccessRate = 100.0
	}

	// 2. 模型使用分布
	mRows, err := s.db.Query(`
		SELECT model, COUNT(*), COALESCE(AVG(duration_ms), 0)
		FROM request_logs
		WHERE model != ''
		GROUP BY model
		ORDER BY COUNT(*) DESC
	`)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var m ModelUsageStat
			var mAvg sql.NullFloat64
			if err := mRows.Scan(&m.Model, &m.Requests, &mAvg); err == nil {
				if total > 0 {
					m.Percentage = float64(m.Requests) / float64(total) * 100
				}
				if mAvg.Valid {
					m.AvgLatencyMs = int64(mAvg.Float64)
				}
				stats.ModelUsages = append(stats.ModelUsages, m)
			}
		}
	}

	// 3. 真实时间趋势（按小时/分钟分桶）
	tRows, err := s.db.Query(`
		SELECT strftime('%H:%M', timestamp) as bucket,
		       COUNT(*),
		       COALESCE(AVG(duration_ms), 0),
		       COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0)
		FROM request_logs
		WHERE timestamp >= datetime('now', '-24 hours')
		GROUP BY bucket
		ORDER BY bucket ASC
		LIMIT 24
	`)
	if err == nil {
		defer tRows.Close()
		for tRows.Next() {
			var p TimeSeriesPoint
			var pAvg sql.NullFloat64
			var pFails int
			if err := tRows.Scan(&p.Timestamp, &p.Requests, &pAvg, &pFails); err == nil {
				if pAvg.Valid {
					p.Latency = int64(pAvg.Float64)
				}
				if p.Requests > 0 {
					p.ErrorRate = float64(pFails) / float64(p.Requests) * 100
					// QPS 估算 (每分钟请求数 / 60)
					p.QPS = float64(p.Requests) / 60.0
				}
				stats.TimeSeries = append(stats.TimeSeries, p)
			}
		}
	}

	return stats, nil
}

// -------------------------------------------------------------
// Chat 调试工作台会话与消息持久化 (彻底替换浏览器 LocalStorage)
// -------------------------------------------------------------

// ChatSessionRecord 会话实体
type ChatSessionRecord struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ChatMessageRecord 消息实体
type ChatMessageRecord struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Reasoning string    `json:"reasoning"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ListChatSessions 查询所有持久化调试会话。
func (s *SQLiteStore) ListChatSessions() ([]ChatSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT id, title, model, reasoning_effort, created_at, updated_at
		FROM chat_sessions
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ChatSessionRecord
	for rows.Next() {
		var r ChatSessionRecord
		var cStr, uStr any
		if err := rows.Scan(&r.ID, &r.Title, &r.Model, &r.ReasoningEffort, &cStr, &uStr); err != nil {
			continue
		}
		r.CreatedAt, r.UpdatedAt = dbTime(cStr), dbTime(uStr)
		list = append(list, r)
	}
	return list, nil
}

// SaveChatSession 保存或更新会话。
func (s *SQLiteStore) SaveChatSession(sess ChatSessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	INSERT INTO chat_sessions (id, title, model, reasoning_effort, created_at, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		title = excluded.title,
		model = excluded.model,
		reasoning_effort = excluded.reasoning_effort,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query, sess.ID, sess.Title, sess.Model, sess.ReasoningEffort)
	return err
}

// DeleteChatSession 删除会话及级联消息。
func (s *SQLiteStore) DeleteChatSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.db.Exec("DELETE FROM chat_messages WHERE session_id = ?", id)
	_, err := s.db.Exec("DELETE FROM chat_sessions WHERE id = ?", id)
	return err
}

// ListChatMessages 获取会话的消息历史。
func (s *SQLiteStore) ListChatMessages(sessionID string) ([]ChatMessageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT id, session_id, role, content, reasoning, status, created_at
		FROM chat_messages
		WHERE session_id = ?
		ORDER BY created_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ChatMessageRecord
	for rows.Next() {
		var m ChatMessageRecord
		var cStr any
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Status, &cStr); err != nil {
			continue
		}
		m.CreatedAt = dbTime(cStr)
		list = append(list, m)
	}
	return list, nil
}

// SaveChatMessage 保存一条消息。
func (s *SQLiteStore) SaveChatMessage(m ChatMessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m.ID == "" {
		m.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}

	query := `
	INSERT INTO chat_messages (id, session_id, role, content, reasoning, status, created_at)
	VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		content = excluded.content,
		reasoning = excluded.reasoning,
		status = excluded.status;
	`
	_, err := s.db.Exec(query, m.ID, m.SessionID, m.Role, m.Content, m.Reasoning, m.Status)
	if err == nil {
		_, _ = s.db.Exec("UPDATE chat_sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", m.SessionID)
	}
	return err
}

// -------------------------------------------------------------
// 对外 API 密钥 (API Keys) 持久化管理
// -------------------------------------------------------------

// APIKeyItem 对外调用 API 密钥。
type APIKeyItem struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ListAPIKeys 列出所有已授权的 API Keys。
func (s *SQLiteStore) ListAPIKeys() ([]APIKeyItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT key, name, created_at
		FROM api_keys
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []APIKeyItem
	for rows.Next() {
		var it APIKeyItem
		var cStr any
		if err := rows.Scan(&it.Key, &it.Name, &cStr); err != nil {
			continue
		}
		it.CreatedAt = dbTime(cStr)
		list = append(list, it)
	}
	return list, nil
}

// SaveAPIKey 保存新的 API Key。
func (s *SQLiteStore) SaveAPIKey(item APIKeyItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if item.Key == "" {
		item.Key = fmt.Sprintf("sk-prism-%d", time.Now().UnixNano())
	}
	if item.Name == "" {
		item.Name = "新建访问令牌"
	}

	query := `
	INSERT INTO api_keys (key, name, created_at)
	VALUES (?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(key) DO UPDATE SET name = excluded.name;
	`
	_, err := s.db.Exec(query, item.Key, item.Name)
	return err
}

// DeleteAPIKey 删除指定的 API Key。
func (s *SQLiteStore) DeleteAPIKey(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM api_keys WHERE key = ?", key)
	return err
}

// dbTime 把时间列的扫描结果转成 time.Time。
//
// TIMESTAMP 列会被 modernc.org/sqlite 解析成 time.Time，扫进 string 时格式是
// RFC 3339 而不是写入时的 "2006-01-02 15:04:05"。只按写入格式解析的话，
// 所有时间都会变成零值（控制面板里显示 0001-01-01）。两种形态都接受。
func dbTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case []byte:
		return dbTime(string(t))
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano} {
			if p, err := time.Parse(layout, t); err == nil {
				return p
			}
		}
	}
	return time.Time{}
}
