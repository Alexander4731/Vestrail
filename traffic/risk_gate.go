package traffic

import (
	"context"
	"encoding/json"
	"net/http"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// HTTPGate inspects an outbound HTTP request inside the proxy BEFORE it is
// forwarded to the target server. Returning allowed=false aborts the request
// locally with statusCode (default 403) and reason, without ever dialing the
// target or triggering MITM passthrough fallback.
type HTTPGate func(ctx context.Context, method, rawURL string) (allowed bool, statusCode int, reason string)

// SetHTTPGate installs or clears the live outbound HTTP risk gate.
func (t *Traffic) SetHTTPGate(gate HTTPGate) {
	if gate == nil {
		t.httpGate.Store(nil)
		return
	}
	t.httpGate.Store(&gate)
}

func (t *Traffic) checkHTTPGate(f *mproxy.Flow) bool {
	if f == nil || f.Request == nil || f.Request.URL == nil {
		return true
	}
	if f.Request.Method == http.MethodConnect {
		return true
	}
	gp := t.httpGate.Load()
	if gp == nil || *gp == nil {
		return true
	}
	ctx := context.Background()
	if raw := f.Request.Raw(); raw != nil && raw.Context() != nil {
		ctx = raw.Context()
	}
	allowed, code, reason := (*gp)(ctx, f.Request.Method, f.Request.URL.String())
	if allowed {
		return true
	}
	if code < 400 || code > 599 {
		code = http.StatusForbidden
	}
	body, _ := json.Marshal(map[string]any{
		"error":   "Blocked by Vestrail Risk Gate",
		"reason":  reason,
		"method":  f.Request.Method,
		"url":     f.Request.URL.String(),
		"blocked": true,
	})
	f.Response = &mproxy.Response{
		StatusCode: code,
		Header: http.Header{
			"Content-Type":       []string{"application/json; charset=utf-8"},
			"X-Vestrail-Blocked": []string{"true"},
		},
		Body: body,
	}
	return false
}
