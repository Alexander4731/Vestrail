package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Alexander4731/Vestrail/intercept"
	"github.com/Alexander4731/Vestrail/notify"
)

const browserNotifyRingSize = 200

// BrowserNotifyHub buffers recent browser desktop notifications in memory with
// monotonically increasing sequence IDs so connected browser tabs receive
// near-real-time notifications (findings + high-risk approval requests + tests).
type BrowserNotifyHub struct {
	mu     sync.RWMutex
	seq    int64
	events []notify.BrowserEvent
}

func newBrowserNotifyHub() *BrowserNotifyHub {
	return &BrowserNotifyHub{events: make([]notify.BrowserEvent, 0, 32)}
}

// Publish appends ev to the ring buffer and assigns the next sequence ID.
func (h *BrowserNotifyHub) Publish(ev notify.BrowserEvent) notify.BrowserEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	h.events = append(h.events, ev)
	if len(h.events) > browserNotifyRingSize {
		h.events = append([]notify.BrowserEvent(nil), h.events[len(h.events)-browserNotifyRingSize:]...)
	}
	return ev
}

// Since returns the latest sequence ID and all events strictly after `after`.
func (h *BrowserNotifyHub) Since(after int64) (int64, []notify.BrowserEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.events) == 0 {
		return h.seq, []notify.BrowserEvent{}
	}
	out := make([]notify.BrowserEvent, 0)
	for _, ev := range h.events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return h.seq, out
}

func (s *Server) browserNotifyHub() *BrowserNotifyHub {
	if s.browserNotify == nil {
		s.browserNotify = newBrowserNotifyHub()
	}
	return s.browserNotify
}

// browserChannelPolicy checks whether global notifications are enabled and at
// least one enabled "browser" channel exists, returning its aggregated options.
func (s *Server) browserChannelPolicy(ctx context.Context) (enabled, onApproval, sound, requireInteraction bool) {
	if s.m == nil || s.m.pg == nil {
		return false, false, true, true
	}
	if !s.m.pg.GetBool(settingNotifyEnabled, true) {
		return false, false, true, true
	}
	channels, err := s.m.pg.ListNotificationChannels(ctx)
	if err != nil {
		return false, false, true, true
	}
	for _, ch := range channels {
		if ch.Kind != notify.KindBrowser || !ch.IsEnabled() {
			continue
		}
		enabled = true
		var cfg map[string]any
		if len(ch.Config) > 0 {
			_ = json.Unmarshal(ch.Config, &cfg)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		if notify.BrowserOnApproval(cfg) {
			onApproval = true
		}
		if notify.BrowserSound(cfg) {
			sound = true
		}
		if notify.BrowserRequireInteraction(cfg) {
			requireInteraction = true
		}
	}
	return enabled, onApproval, sound, requireInteraction
}

// wireApprovalNotifications connects Interceptor's pending-approval observer to
// the BrowserNotifyHub so high-risk approval requests immediately notify the
// user's browser even when they are on another tab or page.
func (s *Server) wireApprovalNotifications() {
	if s.m == nil || s.m.interceptor == nil {
		return
	}
	s.m.interceptor.SetPendingObserver(func(n intercept.PendingNotice) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		enabled, onApproval, sound, requireInteraction := s.browserChannelPolicy(ctx)
		if !enabled || !onApproval {
			return
		}
		toolLabel := n.ToolName
		if toolLabel == "http_proxy" {
			toolLabel = "HTTP 代理闸门"
		}
		title := fmt.Sprintf("高危操作待审批 · %s", toolLabel)
		if n.AgentName != "" {
			title = fmt.Sprintf("高危操作待审批 · %s (%s)", toolLabel, n.AgentName)
		}
		body := strings.TrimSpace(n.Reason)
		if body == "" {
			body = "检测到高危操作，该 Worker 已暂停并等待你审批（任务与其余 Worker 继续运行）"
		}
		if len([]rune(body)) > 160 {
			body = string([]rune(body)[:160]) + "…"
		}
		targetURL := "/system/intercept/approvals"
		if n.TaskID != "" {
			targetURL = fmt.Sprintf("/function/tasks/detail?id=%s&approval=%d", n.TaskID, n.PendingID)
		}
		s.browserNotifyHub().Publish(notify.BrowserEvent{
			Kind:               notify.EventApprovalPending,
			Title:              title,
			Body:               body,
			Severity:           "high",
			URL:                targetURL,
			Tag:                fmt.Sprintf("vestrail-approval-%d", n.PendingID),
			TaskID:             n.TaskID,
			PendingID:          n.PendingID,
			Sound:              sound,
			RequireInteraction: requireInteraction,
			CreatedAt:          n.CreatedAt,
		})
	})
}

// notifyBrowserEvents serves incremental browser notifications for connected UI
// clients: GET /api/notify/browser/events (initial cursor handshake) or
// GET /api/notify/browser/events?after=<seq> (incremental poll).
func (s *Server) notifyBrowserEvents(w http.ResponseWriter, r *http.Request) {
	rawAfter := strings.TrimSpace(r.URL.Query().Get("after"))
	enabled, _, _, _ := s.browserChannelPolicy(r.Context())
	if rawAfter == "" {
		latest, _ := s.browserNotifyHub().Since(0)
		writeJSON(w, 200, map[string]any{
			"latest_seq": latest,
			"enabled":    enabled,
			"items":      []notify.BrowserEvent{},
		})
		return
	}
	after, _ := strconv.ParseInt(rawAfter, 10, 64)
	latest, items := s.browserNotifyHub().Since(after)
	writeJSON(w, 200, map[string]any{
		"latest_seq": latest,
		"enabled":    enabled,
		"items":      items,
	})
}
