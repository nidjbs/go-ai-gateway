package guardrails

import (
	"sync"
	"time"
)

// InjectionTracker 按 API key 追踪注入检测频率。
// 核心思路：不阻止单次尝试（可能是误报），但对短时间内多次触发检测的 key 做惩罚限速。
// 这大幅提高了自动化攻击的成本，同时不影响正常用户。
type InjectionTracker struct {
	mu       sync.Mutex
	windows  map[string]*attackWindow
	maxCount int           // 触发惩罚的阈值（默认 3 次）
	window   time.Duration // 计数窗口（默认 1 分钟）
	penalty  time.Duration // 惩罚时长（默认 30 秒）
}

type attackWindow struct {
	count   int       // 当前窗口内的触发次数
	resetAt time.Time // 窗口重置时间
	blocked bool      // 是否处于惩罚期
}

// TrackerConfig 是 InjectionTracker 的配置。
type TrackerConfig struct {
	MaxAttempts int           // 窗口内最大触发次数（默认 3）
	Window      time.Duration // 计数窗口（默认 1 分钟）
	Penalty     time.Duration // 惩罚时长（默认 30 秒）
}

// DefaultTrackerConfig 返回默认配置。
func DefaultTrackerConfig() TrackerConfig {
	return TrackerConfig{
		MaxAttempts: 3,
		Window:      time.Minute,
		Penalty:     30 * time.Second,
	}
}

// NewInjectionTracker 创建一个新的 InjectionTracker。
func NewInjectionTracker(cfg TrackerConfig) *InjectionTracker {
	return &InjectionTracker{
		windows:  make(map[string]*attackWindow),
		maxCount: cfg.MaxAttempts,
		window:   cfg.Window,
		penalty:  cfg.Penalty,
	}
}

// Record 记录一次注入检测触发。
// 返回 true 表示该 key 应被限速（已进入惩罚期）。
func (t *InjectionTracker) Record(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok || now.After(w.resetAt) {
		// 新窗口或窗口已过期
		t.windows[keyID] = &attackWindow{
			count:   1,
			resetAt: now.Add(t.window),
			blocked: false,
		}
		return false
	}

	w.count++
	if w.count >= t.maxCount {
		// 达到阈值，进入惩罚期
		w.blocked = true
		w.resetAt = now.Add(t.penalty) // 惩罚期结束后重新计数
		w.count = 0
		return true
	}
	return false
}

// IsBlocked 检查 key 是否处于惩罚期。
func (t *InjectionTracker) IsBlocked(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok {
		return false
	}
	if now.After(w.resetAt) {
		// 惩罚期已过，清理
		delete(t.windows, keyID)
		return false
	}
	return w.blocked
}

// PenaltyRemaining 返回 key 剩余的惩罚时间。
func (t *InjectionTracker) PenaltyRemaining(keyID string, now time.Time) time.Duration {
	if keyID == "" {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.windows[keyID]
	if !ok || now.After(w.resetAt) {
		return 0
	}
	if w.blocked {
		return w.resetAt.Sub(now)
	}
	return 0
}

// Reset 清除 key 的所有追踪记录（用于手动解除惩罚）。
func (t *InjectionTracker) Reset(keyID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.windows, keyID)
}

// ActiveBlocks 返回当前处于惩罚期的 key 数量（用于监控）。
func (t *InjectionTracker) ActiveBlocks(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, w := range t.windows {
		if w.blocked && now.Before(w.resetAt) {
			count++
		}
	}
	return count
}
