package traffic

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPGateBlocksHighRiskBeforeUpstreamDial(t *testing.T) {
	var upstreamHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer target.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	tr, err := Open(t.TempDir(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	tr.SetHTTPGate(func(ctx context.Context, method, rawURL string) (bool, int, string) {
		if strings.Contains(rawURL, "restoreFactorySettings") {
			return false, http.StatusForbidden, "blocked factory reset"
		}
		return true, 0, ""
	})

	go func() { _ = tr.Start() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy %s did not start listening in time", addr)
		}
		time.Sleep(25 * time.Millisecond)
	}

	proxyURL, err := url.Parse(tr.ProxyAddr())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   5 * time.Second,
	}

	// 1. Blocked high-risk request must never reach upstream server.
	resp, err := client.Post(target.URL+"/shterm/api/cluster/restoreFactorySettings", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("blocked request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", resp.StatusCode, string(body))
	}
	if resp.Header.Get("X-Vestrail-Blocked") != "true" {
		t.Fatalf("expected X-Vestrail-Blocked header, got %v", resp.Header)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("blocked request must not dial upstream, hits=%d", upstreamHits.Load())
	}

	// 2. Safe GET request must pass through normally.
	resp2, err := client.Get(target.URL + "/shterm/api/user/list")
	if err != nil {
		t.Fatalf("safe request failed: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || string(body2) != "upstream-ok" {
		t.Fatalf("expected safe request 200 upstream-ok, got %d %q", resp2.StatusCode, string(body2))
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", upstreamHits.Load())
	}
}
