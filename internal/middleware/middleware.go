// Package middleware 提供入站请求的横切关注点。
//
// 顺序很重要，链式顺序即代码顺序：
//
//	Recover -> RequestID -> Metrics -> RateLimit -> Auth -> handler
//
// Recover 必须在最外层，否则一次 panic 会带走整个连接且没有任何日志。
// Metrics 在 Auth 之前，这样鉴权失败也能被观测到。
package middleware

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/metrics"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

// RequestID 从上下文取出请求 ID。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithRequestID 写入请求 ID。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// Middleware 是标准签名。
type Middleware func(http.Handler) http.Handler

// Chain 按给定顺序组合中间件。第一个是最外层。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ---------------------------- Recover ----------------------------

// Recover 捕获 panic，返回 500 并记录堆栈。
func Recover(log *slog.Logger, app *metrics.App) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler 是标准库约定的"静默中断"，
					// 不该记成错误，直接重新抛出交给 net/http 处理。
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					if app != nil {
						app.Panics.Inc()
					}
					log.Error("handler panic",
						"path", r.URL.Path,
						"method", r.Method,
						"panic", rec,
						"stack", string(debug.Stack()))
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"内部错误","type":"server_error"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------- CORS ----------------------------

// CORS 处理跨域，origin 为空时不启用。
//
// 两个刻意的设计：
//  1. 预检（OPTIONS）在这里直接终结，不打到业务路由 ——
//     否则每个跨域请求都会多一次无意义的完整路由匹配。
//  2. 不启用时返回透传的 next，零成本（没有闭包判断，只有一次赋值）。
func CORS(origin string) Middleware {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	allowHeaders := "authorization, content-type, x-prism-conversation-id, x-oaiprism-session, x-oaiprism-previous"
	// Expose-Headers 是关键：自定义响应头（尤其是 x-prism-conversation-id）
	// 不在 CORS 安全列表里，浏览器默认不给 JS 读。少了它，跨域客户端
	// 拿不到会话 ID，多轮续写直接断链 —— 而且不报错，只是"每次都是新会话"。
	exposeHeaders := "x-prism-conversation-id, x-request-id"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------- RequestID ----------------------------

// RequestIDMiddleware 为每个请求生成/透传请求 ID。
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = r.Header.Get("X-Correlation-Id")
		}
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

var reqIDState uint64 = 0x2545f4914f6cdd1d
var reqIDMu sync.Mutex

// newRequestID 生成一个短 ID。
//
// 这里用 mutex 保护 xorshift 是有意的：请求 ID 生成频率是"每请求一次"，
// 远低于 SSE 增量写出的频率，为它做 CAS 循环是过度优化；
// 而 mutex 版本更容易读对。
func newRequestID() string {
	reqIDMu.Lock()
	s := reqIDState
	s ^= s << 13
	s ^= s >> 7
	s ^= s << 17
	reqIDState = s
	reqIDMu.Unlock()

	const alphabet = "0123456789abcdef"
	var buf [16]byte
	for i := range buf {
		buf[i] = alphabet[s&15]
		s = s>>4 | (uint64(time.Now().UnixNano()&0xF) << 60)
	}
	return "req_" + string(buf[:])
}

// ---------------------------- Metrics ----------------------------

// statusRecorder 记录状态码与写出字节数。
//
// 必须实现 Flusher，否则包一层就会让所有流式响应的 Flush 失效——
// 这是包装 ResponseWriter 最经典的坑。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	flush   func()
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if s.flush != nil {
		s.flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 writer。
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// MetricsMiddleware 采集入站指标。
func MetricsMiddleware(app *metrics.App) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if app == nil {
				next.ServeHTTP(w, r)
				return
			}
			path := routeLabel(r.URL.Path)
			start := time.Now()

			rec := &statusRecorder{ResponseWriter: w}
			if f, ok := w.(http.Flusher); ok {
				rec.flush = f.Flush
			}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			code := itoa(rec.status)
			app.HTTPRequests.Inc(path, r.Method, code)
			app.HTTPDuration.Observe(time.Since(start).Seconds(), path, r.Method)
		})
	}
}

// routeLabel 把路径归一化成低基数标签。
//
// 直接用原始 path 会让指标基数爆炸（每个 UUID 一条时间序列），
// 最终把 Prometheus 打挂。这是监控里最常见的自伤方式之一。
func routeLabel(p string) string {
	switch {
	case p == "/":
		return "/"
	case strings.HasPrefix(p, "/v1/chat/completions"):
		return "/v1/chat/completions"
	case strings.HasPrefix(p, "/v1/completions"):
		return "/v1/completions"
	case strings.HasPrefix(p, "/v1/responses"):
		return "/v1/responses"
	case strings.HasPrefix(p, "/v1/messages"):
		return "/v1/messages"
	case strings.HasPrefix(p, "/v1/models"):
		return "/v1/models"
	case strings.HasPrefix(p, "/healthz"):
		return "/healthz"
	case strings.HasPrefix(p, "/readyz"):
		return "/readyz"
	case strings.HasPrefix(p, "/metrics"):
		return "/metrics"
	case strings.HasPrefix(p, "/admin/"):
		return p
	}
	// 原样反代通道：按最后两段归类，保留端点语义但去掉 UUID。
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) >= 2 {
		last := segs[len(segs)-1]
		if len(last) >= 32 || looksLikeUUID(last) {
			if len(segs) >= 3 {
				return "/" + strings.Join(segs[:len(segs)-2], "/") + "/*"
			}
			if len(segs) >= 2 {
				return "/" + segs[0] + "/*"
			}
		}
	}
	if len(segs) > 3 {
		return "/" + strings.Join(segs[:3], "/")
	}
	return p
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ---------------------------- Auth ----------------------------

// APIKeyAuth 校验调用方的 API Key。
//
// 细节：用 constant-time 比较。虽然 API Key 不是密码，
// 但把"比较耗时随前缀匹配长度变化"这种信道留着没有任何好处。
func APIKeyAuth(keys []string, app *metrics.App, enabled bool, exemptPaths ...string) Middleware {
	// 预先把 key 存成 []byte，避免每请求分配。
	valid := make([][]byte, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			valid = append(valid, []byte(k))
		}
	}

	exempt := map[string]struct{}{
		"/healthz": {},
		"/readyz":  {},
		"/metrics": {},
	}
	// 以 "/" 结尾的豁免项按前缀匹配，且只放行 GET/HEAD：
	// 用于控制面板静态资源——浏览器地址栏导航带不上 Bearer，
	// 而面板的数据请求走 /admin/*，仍然要鉴权。
	var exemptPrefixes []string
	for _, p := range exemptPaths {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if len(p) > 1 && strings.HasSuffix(p, "/") {
			exemptPrefixes = append(exemptPrefixes, p)
			continue
		}
		exempt[p] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		if !enabled || len(valid) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 探针端点豁免鉴权：k8s / Docker / 监控系统探针拿 401 会导致容器被反复重启。
			if _, ok := exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			if len(exemptPrefixes) > 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				// 先 Clean：防止 /dashboard/../admin/... 借前缀绕过鉴权。
				clean := path.Clean("/"+r.URL.Path) + "/"
				for _, p := range exemptPrefixes {
					if strings.HasPrefix(clean, p) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// 兼容三种常见携带方式。
			key := bearerToken(r.Header.Get("Authorization"))
			if key == "" {
				key = strings.TrimSpace(r.Header.Get("x-api-key"))
			}
			if key == "" {
				key = strings.TrimSpace(r.Header.Get("api-key"))
			}

			if !matchAny(valid, key) {
				if app != nil {
					app.AuthFailed.Inc()
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="oaiprism"`)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"API Key 无效或缺失","type":"invalid_request_error","code":"invalid_api_key"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func matchAny(valid [][]byte, key string) bool {
	if key == "" {
		return false
	}
	kb := []byte(key)
	ok := false
	for _, v := range valid {
		// 不 break：让比较次数与 key 数量固定，不泄漏"命中了第几个"。
		if subtle.ConstantTimeCompare(v, kb) == 1 {
			ok = true
		}
	}
	return ok
}

// ---------------------------- RateLimit ----------------------------

// RateLimiter 是全局令牌桶。
//
// 单实例场景下不需要分布式限流；跨实例时应当在网关层做。
// 这里的目的是保护上游账号不被自己的重试打爆。
type RateLimiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64
	last   time.Time

	app *metrics.App
}

// NewRateLimiter 构造限流器。rate<=0 表示不限流。
func NewRateLimiter(rate float64, burst int, app *metrics.App) *RateLimiter {
	if rate <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(rate)
		if burst < 1 {
			burst = 1
		}
	}
	return &RateLimiter{
		tokens: float64(burst),
		burst:  float64(burst),
		rate:   rate,
		last:   time.Now(),
		app:    app,
	}
}

// Middleware 返回限流中间件。
func (l *RateLimiter) Middleware() Middleware {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(time.Now()) {
				if l.app != nil {
					l.app.RateLimited.Inc()
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"请求过于频繁，请稍后重试","type":"rate_limit_exceeded"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (l *RateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// ---------------------------- 工具 ----------------------------

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
