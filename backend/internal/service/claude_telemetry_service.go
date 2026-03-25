package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	// 事件上报：基础间隔 + 随机抖动，模拟真实 SDK 的非精确批量上报
	telemetryEventBaseInterval = 20 * time.Second
	telemetryEventJitterRange  = 15 * time.Second // 实际间隔 = base ± jitter → 约 12–35 秒

	telemetrySessionIdleTTL  = 5 * time.Minute
	telemetryCleanupInterval = 1 * time.Minute
	telemetryHelloPath       = "/api/hello"
	telemetryEventPath       = "/api/event_logging/batch"
	telemetryBaseURL         = "https://api.anthropic.com"
)

// ClaudeTelemetryService simulates the telemetry traffic that a real Claude
// Code client sends alongside API requests.
//
// Real Claude Code behavior (captured via eBPF + mitmproxy):
//   - GET /api/hello  — called ONCE when the CLI session starts (not periodic)
//   - POST /api/event_logging/batch — batched events sent after user actions,
//     roughly every 10-30 seconds during active use, but NOT on a fixed timer
//
// Without any telemetry, Anthropic sees "API calls but zero side-channel
// traffic" — a strong indicator of proxy/automation usage.
type ClaudeTelemetryService struct {
	mu           sync.RWMutex
	sessions     map[int64]*telemetrySession
	httpUpstream HTTPUpstream
	stopped      chan struct{}
	once         sync.Once
}

type telemetrySession struct {
	accountID    int64
	token        string
	proxyURL     string
	enableTLS    bool
	userAgent    string
	sessionID    string
	deviceID     string
	lastActive   time.Time
	apiCallCount int64 // tracks how many API calls this session has made
	cancel       context.CancelFunc
}

func NewClaudeTelemetryService(httpUpstream HTTPUpstream) *ClaudeTelemetryService {
	s := &ClaudeTelemetryService{
		sessions:     make(map[int64]*telemetrySession),
		httpUpstream: httpUpstream,
		stopped:      make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// EnsureSession starts or extends a telemetry session for the given account.
// Called by the gateway on each API request.
func (s *ClaudeTelemetryService) EnsureSession(accountID int64, token, proxyURL, userAgent string, enableTLS bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[accountID]; ok {
		sess.lastActive = time.Now()
		sess.apiCallCount++
		if sess.token != token {
			sess.token = token
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &telemetrySession{
		accountID:    accountID,
		token:        token,
		proxyURL:     proxyURL,
		enableTLS:    enableTLS,
		userAgent:    userAgent,
		sessionID:    generateRandomHex(16),
		deviceID:     generateRandomHex(16),
		lastActive:   time.Now(),
		apiCallCount: 1,
		cancel:       cancel,
	}
	s.sessions[accountID] = sess

	// Real Claude Code sends /api/hello exactly once when a CLI session starts.
	go s.sendHello(ctx, sess)

	// Event logging runs on a jittered schedule, only while the session is active.
	go s.runEventLogging(ctx, sess)

	slog.Debug("telemetry_session_started", "account_id", accountID)
}

func (s *ClaudeTelemetryService) Stop() {
	s.once.Do(func() {
		close(s.stopped)
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, sess := range s.sessions {
			sess.cancel()
			delete(s.sessions, id)
		}
	})
}

// sendHello fires a single GET /api/hello, mimicking CLI startup.
func (s *ClaudeTelemetryService) sendHello(ctx context.Context, sess *telemetrySession) {
	// Small random delay (0-3s) — real CLI does some init before the first hello
	jitter := randomDuration(3 * time.Second)
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", telemetryBaseURL+telemetryHelloPath, nil)
	if err != nil {
		return
	}

	s.mu.RLock()
	token := sess.token
	ua := sess.userAgent
	s.mu.RUnlock()

	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("user-agent", ua)
	req.Header.Set("accept", "application/json")

	resp, err := s.httpUpstream.DoWithTLS(req, sess.proxyURL, sess.accountID, 1, sess.enableTLS)
	if err != nil {
		slog.Debug("telemetry_hello_failed", "account_id", sess.accountID, "error", err)
		return
	}
	_ = resp.Body.Close()
	slog.Debug("telemetry_hello_sent", "account_id", sess.accountID, "status", resp.StatusCode)
}

// runEventLogging sends event batches on a jittered schedule that correlates
// with actual API activity. If the account goes idle, event batches stop.
func (s *ClaudeTelemetryService) runEventLogging(ctx context.Context, sess *telemetrySession) {
	// Initial delay before first event batch (real SDK buffers initial events)
	initialDelay := 3*time.Second + randomDuration(5*time.Second)
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialDelay):
	}

	var lastSeenCallCount int64

	for {
		// Jittered sleep: base ± random jitter → non-periodic intervals
		interval := telemetryEventBaseInterval + randomDuration(telemetryEventJitterRange) - (telemetryEventJitterRange / 2)
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-s.stopped:
			return
		case <-time.After(interval):
		}

		// Only send events if there was recent API activity.
		// Real Claude Code only logs events in response to user actions.
		s.mu.RLock()
		currentCallCount := sess.apiCallCount
		idle := time.Since(sess.lastActive) > 2*time.Minute
		s.mu.RUnlock()

		if idle {
			continue
		}

		// If no new API calls since last check, reduce event frequency
		// (occasionally still send a "session_heartbeat" event, like real SDK does)
		newCalls := currentCallCount - lastSeenCallCount
		lastSeenCallCount = currentCallCount

		if newCalls == 0 && randomInt(3) != 0 {
			// 2/3 chance to skip when idle — real SDK doesn't send events every tick
			continue
		}

		s.sendEventBatch(ctx, sess, newCalls)
	}
}

func (s *ClaudeTelemetryService) sendEventBatch(ctx context.Context, sess *telemetrySession, recentAPICalls int64) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	payload := buildTelemetryBatch(sess, recentAPICalls)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", telemetryBaseURL+telemetryEventPath, bytes.NewReader(body))
	if err != nil {
		return
	}

	s.mu.RLock()
	token := sess.token
	ua := sess.userAgent
	s.mu.RUnlock()

	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", ua)
	req.Header.Set("accept", "application/json")

	resp, err := s.httpUpstream.DoWithTLS(req, sess.proxyURL, sess.accountID, 1, sess.enableTLS)
	if err != nil {
		slog.Debug("telemetry_event_batch_failed", "account_id", sess.accountID, "error", err)
		return
	}
	_ = resp.Body.Close()
	slog.Debug("telemetry_event_batch_sent", "account_id", sess.accountID, "status", resp.StatusCode)
}

func (s *ClaudeTelemetryService) cleanupLoop() {
	ticker := time.NewTicker(telemetryCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			s.cleanupIdleSessions()
		}
	}
}

func (s *ClaudeTelemetryService) cleanupIdleSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-telemetrySessionIdleTTL)
	for id, sess := range s.sessions {
		if sess.lastActive.Before(cutoff) {
			sess.cancel()
			delete(s.sessions, id)
			slog.Debug("telemetry_session_expired", "account_id", id)
		}
	}
}

type telemetryBatch struct {
	Events []telemetryEvent `json:"events"`
}

type telemetryEvent struct {
	Type       string                 `json:"type"`
	SessionID  string                 `json:"session_id"`
	DeviceID   string                 `json:"device_id"`
	Timestamp  string                 `json:"timestamp"`
	Properties map[string]interface{} `json:"properties"`
}

// buildTelemetryBatch creates a realistic event batch.
// Event count and types correlate with recent API activity.
func buildTelemetryBatch(sess *telemetrySession, recentAPICalls int64) *telemetryBatch {
	now := time.Now().UTC()

	// Event count correlates with activity: more API calls → more events
	eventCount := 1
	if recentAPICalls > 0 {
		eventCount = 1 + randomInt(int(min(recentAPICalls+1, 4))) // 1–4 events
	}

	// Event type distribution changes based on activity
	activeTypes := []string{
		"tengu_unary_event",
		"tengu_tool_use",
		"tengu_permission_request_option_selected",
	}
	idleTypes := []string{
		"tengu_session_heartbeat",
	}

	events := make([]telemetryEvent, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		var evtType string
		if recentAPICalls > 0 {
			evtType = activeTypes[randomInt(len(activeTypes))]
		} else {
			evtType = idleTypes[0]
		}

		events = append(events, telemetryEvent{
			Type:      evtType,
			SessionID: sess.sessionID,
			DeviceID:  sess.deviceID,
			Timestamp: now.Add(-time.Duration(randomInt(10)) * time.Second).Format(time.RFC3339Nano),
			Properties: map[string]interface{}{
				"platform":            "linux",
				"is_running_with_bun": true,
			},
		})
	}

	return &telemetryBatch{Events: events}
}

func generateRandomHex(length int) string {
	b := make([]byte, length)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomDuration(maxDuration time.Duration) time.Duration {
	if maxDuration <= 0 {
		return 0
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(maxDuration)))
	return time.Duration(n.Int64())
}

func randomInt(maxVal int) int {
	if maxVal <= 0 {
		return 0
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(maxVal)))
	return int(n.Int64())
}

func (s *ClaudeTelemetryService) FormatTelemetryStats() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("active_telemetry_sessions=%d", len(s.sessions))
}
