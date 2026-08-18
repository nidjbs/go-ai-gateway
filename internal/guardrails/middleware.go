package guardrails

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"example.com/light-llm-gateway/internal/apierr"
	"example.com/light-llm-gateway/internal/auth"
)

// ctxKey 是 context 中存储检测结果的 key 类型。
type ctxKey string

const (
	ctxKeyScanResult ctxKey = "guardrails_scan_result"
	ctxKeyCanary     ctxKey = "guardrails_canary"
)

// Config 是 guardrails 中间件的配置。
type Config struct {
	Enabled          bool    // 是否启用
	Mode             string  // "flag" | "block" | "off"
	Threshold        float64 // 注入检测分数阈值（0.0 ~ 1.0）
	CanaryEnabled    bool    // 是否启用 canary token 出站检测
	CanaryBufferSize int     // 出站检测 buffer 大小（字节）
	Tracker          TrackerConfig
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		Mode:             "flag", // 默认 flag 模式，零误杀
		Threshold:        0.75,   // 需要 3 条规则同时命中才 block
		CanaryEnabled:    true,
		CanaryBufferSize: 2048,
		Tracker:          DefaultTrackerConfig(),
	}
}

// Middleware is the guardrails HTTP middleware.
type Middleware struct {
	config  Config
	scanner *Scanner
	tracker Tracker
	logger  *slog.Logger
}

// NewMiddleware builds the middleware around the supplied tracker. Callers
// (typically the gateway server) resolve the tracker from TrackerRegistry
// so a distributed backend can be plugged in without touching this package.
func NewMiddleware(cfg Config, tracker Tracker, logger *slog.Logger) *Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	if tracker == nil {
		tracker = NewInjectionTracker(cfg.Tracker)
	}
	return &Middleware{
		config:  cfg,
		scanner: NewScanner(),
		tracker: tracker,
		logger:  logger,
	}
}

// Handle 返回一个 http.Handler，包装下一个 handler。
func (m *Middleware) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 快速路径：未启用或非 chat/embeddings 请求直接放行
		if !m.config.Enabled || m.config.Mode == "off" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/embeddings" {
			next.ServeHTTP(w, r)
			return
		}

		// 1. 读取请求体（并恢复，供后续 handler 使用）
		body, err := readAndRestoreBody(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		// 2. 解析 messages（从完整请求体中提取）
		messages, ok := MessagesFromChatRequest(body)
		if !ok || len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// 3. 注入扫描
		result := m.scanner.ScanMessages(messages, m.config.Threshold)

		// 4. 获取 principal（从 context，由 auth middleware 注入）
		keyID := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
			keyID = principal.APIKeyID
		}

		// 5. 检查是否已被惩罚限速
		now := time.Now()
		if m.tracker.IsBlocked(keyID, now) {
			penalty := m.tracker.PenaltyRemaining(keyID, now)
			m.logger.Warn("guardrails: key blocked by injection tracker",
				"key_id", keyID,
				"penalty_remaining", penalty.Seconds(),
			)
			w.Header().Set("Retry-After", formatRetryAfter(penalty))
			apierr.Write(w, http.StatusTooManyRequests,
				"injection_tracker_blocked",
				"security_error",
				"API key temporarily blocked due to repeated security violations")
			return
		}

		// 6. 决策
		if result.Action == "block" && m.config.Mode == "block" {
			// 记录攻击尝试
			blocked := m.tracker.Record(keyID, now)
			m.logger.Warn("guardrails: prompt injection blocked",
				"key_id", keyID,
				"score", result.Score,
				"matched_count", len(result.Matched),
				"tracker_blocked", blocked,
			)
			apierr.Write(w, http.StatusTooManyRequests,
				"prompt_injection_detected",
				"security_error",
				"Request blocked by security policy")
			return
		}

		// 7. flag 模式或 block 模式下的 flag 级别：放行但打标
		if result.Action == "flag" || (result.Action == "block" && m.config.Mode == "flag") {
			m.logger.Info("guardrails: prompt injection flagged",
				"key_id", keyID,
				"score", result.Score,
				"matched_count", len(result.Matched),
			)
			// 将检测结果注入 context，供后续 handler 使用
			ctx := context.WithValue(r.Context(), ctxKeyScanResult, result)
			r = r.WithContext(ctx)
		}

		// 8. 放行
		next.ServeHTTP(w, r)
	})
}

// ScanResultFromContext 从 context 中获取检测结果。
// 如果没有检测结果（allow 或未扫描），返回零值和 false。
func ScanResultFromContext(ctx context.Context) (ScanResult, bool) {
	v := ctx.Value(ctxKeyScanResult)
	if v == nil {
		return ScanResult{}, false
	}
	result, ok := v.(ScanResult)
	return result, ok
}

// CanaryFromContext 从 context 中获取 canary token。
func CanaryFromContext(ctx context.Context) (CanaryToken, bool) {
	v := ctx.Value(ctxKeyCanary)
	if v == nil {
		return CanaryToken{}, false
	}
	canary, ok := v.(CanaryToken)
	return canary, ok
}

// SetCanaryToContext 将 canary token 注入 context。
func SetCanaryToContext(ctx context.Context, canary CanaryToken) context.Context {
	return context.WithValue(ctx, ctxKeyCanary, canary)
}

// ── 内部辅助函数 ──

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	// 恢复 body，供后续 handler 读取
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func formatRetryAfter(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return fmt.Sprintf("%d", sec)
}
