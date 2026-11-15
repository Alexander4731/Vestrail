package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestFindingVerificationRejectsForgedAuthority(t *testing.T) {
	s, fid := newRetestServer(t)
	for _, body := range []string{
		`{"request_key":"x","verifier":"evidence_integrity","verified":true}`,
		`{"request_key":"x","verifier":"evidence_integrity","actor":"verifier"}`,
		`{"request_key":"x","verifier":"controlled_marker"}`,
		`{"request_key":"x","verifier":"evidence_integrity"} {}`,
	} {
		w := retestRequest(s.startFindingVerification, "POST", fid, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("forgery %s: %d %s", body, w.Code, w.Body)
		}
	}
	w := retestRequest(s.startFindingVerification, "POST", fid, `{"request_key":"valid","verifier":"evidence_integrity","evidence_version":0}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"inconclusive"`) {
		t.Fatalf("missing evidence %d %s", w.Code, w.Body)
	}
	w = retestRequest(s.listFindingVerifications, "GET", fid, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"actor":"human:Vestrail"`) {
		t.Fatalf("history %d %s", w.Code, w.Body)
	}
}
