package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/internal/testutil"
)

func TestProxyEnforcesActualSharedRequestCapacity(t *testing.T) {
	for _, https := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[https], func(t *testing.T) {
			capacity := 2
			if https {
				capacity = 1
			}
			t.Setenv("VESTRAIL_SHARED_CHANNEL_CAP", strconv.Itoa(capacity))
			var active, peak atomic.Int32
			entered := make(chan struct{}, 16)
			release := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Write([]byte("fixture complete"))
			})
			upstream := httptest.NewServer(handler)
			if https {
				upstream.Close()
				upstream = httptest.NewTLSServer(handler)
			}
			defer upstream.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			proxy, err := Open(testutil.TempDir(t), address)
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			go proxy.Start()
			ready := false
			for i := 0; i < 100; i++ {
				conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
				if err == nil {
					conn.Close()
					ready = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("proxy did not start")
			}
			proxyURL, _ := url.Parse(proxy.ProxyAddr())
			roots := x509.NewCertPool()
			cert := proxy.proxy.GetCertificate()
			roots.AddCert(&cert)
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			var wg sync.WaitGroup
			errors := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					response, err := client.Get(upstream.URL)
					if err == nil {
						_, err = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
					errors <- err
				}()
			}
			for range capacity {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					close(release)
					t.Fatal("capacity was not admitted")
				}
			}
			select {
			case <-entered:
				close(release)
				t.Fatal("shared capacity exceeded")
			case <-time.After(80 * time.Millisecond):
			}
			close(release)
			wg.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			if peak.Load() > int32(capacity) {
				t.Fatalf("actual peak=%d cap=%d", peak.Load(), capacity)
			}
		})
	}
}

func TestCancelledRequestDoesNotConsumeSlot(t *testing.T) {
	traffic := &Traffic{requestSlots: make(chan struct{}, 1), closed: make(chan struct{})}
	traffic.requestSlots <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if traffic.acquireRequest(ctx) {
		t.Fatal("cancelled request admitted")
	}
	if len(traffic.requestSlots) != 1 {
		t.Fatal("existing slot was released")
	}
}
