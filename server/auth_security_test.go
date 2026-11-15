package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestPasswordChangeRevokesOldSession(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	prior, err := m.pg.GetSettings([]string{authPassKey, authEpochKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, key := range []string{authPassKey, authEpochKey} {
			if value, ok := prior[key]; ok {
				m.pg.SetSetting(key, value)
			} else {
				m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		}
	})
	hash, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	if err := m.pg.SetSettings(map[string]string{authPassKey: string(hash), authEpochKey: "old"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, jwtKey: []byte(strings.Repeat("k", 32))}
	for _, username := range []string{adminUsername, legacyAdminUsername} {
		w := httptest.NewRecorder()
		s.authLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"`+username+`","password":"old-password"}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
		}
	}
	old, _ := signJWT(s.jwtKey, "old")
	r := httptest.NewRequest("POST", "https://example.com/api/auth/change-password", strings.NewReader(`{"old_password":"old-password","new_password":"new-password"}`))
	r.Header.Set("Authorization", "Bearer "+old)
	w := httptest.NewRecorder()
	s.authChangePassword(w, r)
	if w.Code != 200 {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	if valid, err := s.validAuth(r); valid || err != nil {
		t.Fatalf("old token valid=%v err=%v", valid, err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != authCookieName || !cookies[0].HttpOnly || !cookies[0].Secure ||
		cookies[1].Name != legacyAuthCookieName || cookies[1].MaxAge != -1 {
		t.Fatal("missing secure HttpOnly session cookie")
	}
	fresh := httptest.NewRequest("GET", "https://example.com/api/events", nil)
	fresh.AddCookie(cookies[0])
	if valid, err := s.validAuth(fresh); !valid || err != nil {
		t.Fatalf("new cookie valid=%v err=%v", valid, err)
	}
}

func TestRebrandSessionCompatibility(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	for _, subject := range []string{adminUsername, legacyAdminUsername, "", "other"} {
		for _, expired := range []bool{false, true} {
			expires := time.Now().Add(time.Hour)
			if expired {
				expires = time.Now().Add(-time.Hour)
			}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
				Subject: subject, ID: "epoch", ExpiresAt: jwt.NewNumericDate(expires),
			}).SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			want := !expired && (subject == adminUsername || subject == legacyAdminUsername)
			if verifyJWT(token, key, "epoch") != want || verifyJWT(token, key, "changed") {
				t.Fatalf("subject=%q expired=%v: session validity or revocation failed", subject, expired)
			}
		}
	}
	r := httptest.NewRequest("POST", "https://example.com/api/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: legacyAuthCookieName, Value: "legacy"})
	if extractToken(r) != "legacy" {
		t.Fatal("legacy cookie ignored")
	}
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: "current"})
	if extractToken(r) != "current" {
		t.Fatal("current cookie must take precedence")
	}
	w := httptest.NewRecorder()
	(&Server{}).authLogout(w, r)
	cookies := w.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("logout cookies=%d", len(cookies))
	}
	for _, cookie := range cookies {
		if cookie.Value != "" || cookie.MaxAge != -1 || !cookie.HttpOnly || !cookie.Secure {
			t.Fatalf("logout did not securely clear %s", cookie.Name)
		}
	}
}

func TestAuthRejectsQueryTokenAndCrossSiteWrite(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/api/events?token=long-secret", nil)
	if extractToken(r) != "" {
		t.Fatal("URL credential accepted")
	}
	s := &Server{}
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r = httptest.NewRequest("POST", "http://example.com/api/auth/login", nil)
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-site mutation status=%d", w.Code)
	}
	for n := 0; n < 10; n++ {
		if !s.allowLogin(r) {
			t.Fatal("limit reached too early")
		}
	}
	if s.allowLogin(r) {
		t.Fatal("login attempts unbounded")
	}
}

func TestAuthOriginMatchesSchemeHostAndPort(t *testing.T) {
	for _, tc := range []struct {
		request, origin, forwarded string
		want                       bool
	}{
		{"http://127.0.0.1:8787/api/auth/login", "http://127.0.0.1:8787", "", true},
		{"http://127.0.0.1:8787/api/auth/login", "http://127.0.0.1:3000", "", false},
		{"http://localhost:8787/api/auth/login", "http://127.0.0.1:8787", "", false},
		{"https://example.com/api/auth/login", "http://example.com", "", false},
		{"http://example.com/api/auth/login", "https://example.com", "https", true},
	} {
		r := httptest.NewRequest("POST", tc.request, nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Proto", tc.forwarded)
		if got := allowedOrigin(r); got != tc.want {
			t.Errorf("request=%s origin=%s: allowed=%v", tc.request, tc.origin, got)
		}
	}
}
