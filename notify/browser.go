package notify

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BrowserEvent is a structured notification payload delivered to connected
// browsers (rendered via the Web Notification API and in-app toast).
type BrowserEvent struct {
	Seq                int64     `json:"seq"`
	Kind               string    `json:"kind"` // finding_created | finding_status_changed | approval_pending | test
	Title              string    `json:"title"`
	Body               string    `json:"body"`
	Severity           string    `json:"severity"`
	URL                string    `json:"url,omitempty"`
	Tag                string    `json:"tag,omitempty"`
	TaskID             string    `json:"task_id,omitempty"`
	PendingID          int64     `json:"pending_id,omitempty"`
	Sound              bool      `json:"sound"`
	RequireInteraction bool      `json:"require_interaction"`
	CreatedAt          time.Time `json:"created_at"`
}

// BrowserSink receives browser notifications emitted by browserChannel.Send.
type BrowserSink func(BrowserEvent)

type browserSinkKey struct{}

// WithBrowserSink binds the server's BrowserNotifyHub publish function into ctx
// so browserChannel stays completely stateless and unit-testable.
func WithBrowserSink(ctx context.Context, sink BrowserSink) context.Context {
	return context.WithValue(ctx, browserSinkKey{}, sink)
}

func browserSinkFromCtx(ctx context.Context) BrowserSink {
	sink, _ := ctx.Value(browserSinkKey{}).(BrowserSink)
	return sink
}

// BrowserOnApproval reports whether a browser channel config enables high-risk
// approval desktop alerts (defaults to true when omitted).
func BrowserOnApproval(cfg map[string]any) bool {
	if _, ok := cfg["on_approval"]; !ok {
		return true
	}
	return cfgBool(cfg, "on_approval")
}

// BrowserSound reports whether sound alert is enabled (defaults to true).
func BrowserSound(cfg map[string]any) bool {
	if _, ok := cfg["sound"]; !ok {
		return true
	}
	return cfgBool(cfg, "sound")
}

// BrowserRequireInteraction reports whether notifications stay on screen until
// clicked or dismissed (defaults to true).
func BrowserRequireInteraction(cfg map[string]any) bool {
	if _, ok := cfg["require_interaction"]; !ok {
		return true
	}
	return cfgBool(cfg, "require_interaction")
}

type browserChannel struct{}

func (browserChannel) Kind() string              { return KindBrowser }
func (browserChannel) DefaultRatePerMin() int    { return 60 }
func (browserChannel) SecretKeys() []string      { return nil }
func (browserChannel) DestinationKeys() []string { return nil }

func (browserChannel) Validate(cfg map[string]any) error {
	// All browser channel options are optional booleans; empty {} is valid.
	for _, key := range []string{"on_approval", "require_interaction", "sound"} {
		if v, ok := cfg[key]; ok {
			switch v.(type) {
			case bool, string:
			default:
				return fmt.Errorf("字段 %s 必须是布尔值", key)
			}
		}
	}
	return nil
}

func (browserChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if len(m.Items) == 0 {
		return 0, Permanent(fmt.Errorf("消息为空"))
	}
	sink := browserSinkFromCtx(ctx)
	if sink == nil {
		return 0, fmt.Errorf("浏览器通知通道尚未就绪")
	}

	ev := BrowserEvent{
		Sound:              BrowserSound(cfg),
		RequireInteraction: BrowserRequireInteraction(cfg),
		CreatedAt:          time.Now(),
	}
	if m.Batch && len(m.Items) > 1 {
		first := m.Items[0]
		ev.Kind = EventFindingCreated
		ev.Severity = first.Severity
		ev.Title = fmt.Sprintf("Vestrail · 新增 %d 条漏洞发现", len(m.Items))
		ev.Body = fmt.Sprintf("首条：%s %s", SeverityLabel(first.Severity), first.Title())
		ev.URL = "/function/findings"
		ev.Tag = fmt.Sprintf("vestrail-batch-%d", time.Now().Unix())
	} else {
		it := m.Items[0]
		ev.Severity = it.Severity
		if it.FindingID == 0 {
			ev.Kind = "test"
			ev.Title = "Vestrail · " + it.Title()
			ev.Body = it.Summary
			ev.URL = "/system/notify"
			ev.Tag = fmt.Sprintf("vestrail-test-%d", time.Now().UnixNano())
		} else if it.IsStatusChange() {
			ev.Kind = EventFindingStatusChanged
			ev.Title = fmt.Sprintf("漏洞状态变更 · %s", it.Title())
			ev.Body = fmt.Sprintf("%s → %s", StatusLabel(it.FromStatus), StatusLabel(it.ToStatus))
			ev.URL = fmt.Sprintf("/function/findings/detail?id=%d", it.FindingID)
			ev.Tag = fmt.Sprintf("vestrail-finding-%d-%s", it.FindingID, it.ToStatus)
		} else {
			ev.Kind = EventFindingCreated
			ev.Title = fmt.Sprintf("%s 发现漏洞 · %s", SeverityLabel(it.Severity), it.Title())
			ev.Body = strings.TrimSpace(it.Summary)
			if len([]rune(ev.Body)) > 140 {
				ev.Body = string([]rune(ev.Body)[:140]) + "…"
			}
			ev.URL = fmt.Sprintf("/function/findings/detail?id=%d", it.FindingID)
			ev.Tag = fmt.Sprintf("vestrail-finding-%d", it.FindingID)
		}
	}
	sink(ev)
	return len(m.Items), nil
}
