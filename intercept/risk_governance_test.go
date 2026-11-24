package intercept

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
)

func TestNormalizeForMatchCamelCaseAndURLDecode(t *testing.T) {
	got := NormalizeForMatch(`{"url":"https:\/\/t\/api\/cluster\/restoreFactorySettings%3Fx%3D1"}`)
	if !strings.Contains(got, "restore_factory_settings") {
		t.Fatalf("expected camelCase split token, got %q", got)
	}
	if !strings.Contains(got, "/api/cluster/restoreFactorySettings?x=1") {
		t.Fatalf("expected unescaped/decoded URL, got %q", got)
	}
}

func TestBenignDocWriteAndSafetyFilterSanitization(t *testing.T) {
	mdInput, _ := json.Marshal(map[string]string{
		"file_path": "report.md",
		"content":   "Finding: endpoint POST /api/cluster/restoreFactorySettings and rm -rf / example",
	})
	if !IsBenignDocWrite("Write", mdInput) {
		t.Fatal("expected markdown write to be classified as benign doc write")
	}

	pyInput, _ := json.Marshal(map[string]string{
		"file_path": "p_secondary.py",
		"content":   "tests=[('POST','api/cluster/restoreFactorySettings','{}')]",
	})
	if IsBenignDocWrite("Write", pyInput) {
		t.Fatal("expected python script write NOT to be benign doc write")
	}

	// Task #3 activity #105086 regression: defensive regex denylist must not be false-positived.
	filterScript, _ := json.Marshal(map[string]string{
		"file_path": "scan.cjs",
		"content":   "const risky = /delete|remove|restart|restore|factory|reboot|kill/i;\nif (risky.test(p)) continue;\nconsole.log('ok');",
	})
	sanitized := SanitizeWriteInputForMatch("Write", filterScript)
	if strings.Contains(sanitized, "reboot") || strings.Contains(sanitized, "factory") {
		t.Fatalf("expected safety filter line to be stripped for static Write matching, got %s", sanitized)
	}
	// Real payload must remain intact.
	sanitizedPayload := SanitizeWriteInputForMatch("Write", pyInput)
	if !strings.Contains(sanitizedPayload, "restoreFactorySettings") {
		t.Fatalf("real payload was unexpectedly stripped: %s", sanitizedPayload)
	}
}

func TestClassifyRiskTaxonomyAndReadOnlyFastAllow(t *testing.T) {
	// L0 pure read-only fast allow (0 LLM cost)
	for _, tc := range []struct {
		tool string
		in   string
	}{
		{"Read", `{"file_path":"app.js"}`},
		{"Bash", `{"command":"curl -s https://t/api/user/list"}`},
		{"Bash", `{"command":"ls -la && cat README.md"}`},
		{"Write", `{"file_path":"notes.md","content":"rm -rf / restoreFactorySettings"}`},
	} {
		r := ClassifyRisk(tc.tool, []byte(tc.in))
		if r.NeedsJudge || r.Level != "low" {
			t.Errorf("expected L0 low/no-judge for %s %s, got %+v", tc.tool, tc.in, r)
		}
	}

	// R1–R6 high-risk classifications
	riskCases := []struct {
		tool   string
		in     string
		domain string
	}{
		{"Bash", `{"command":"mkfs.ext4 /dev/sdb1"}`, "R1"},
		{"Bash", `{"command":"curl -X POST https://t/api/cluster/restoreFactorySettings -d '{}'"}`, "R2"},
		{"Bash", `{"command":"curl -X POST https://t/api/sysParam/restartTomcat -d '{}'"}`, "R3"},
		{"Bash", `{"command":"apt purge -y nginx"}`, "R4"},
		{"Bash", `{"command":"mysql -e \"DELETE FROM users;\""}`, "R5"},
		{"RunScript", `{"path":"p.py","content":"req('admin.jar','api/foo','POST','{}')"}`, "R6"},
	}
	for _, tc := range riskCases {
		r := ClassifyRisk(tc.tool, []byte(tc.in))
		if !r.NeedsJudge || r.Domain != tc.domain {
			t.Errorf("expected domain=%s NeedsJudge=true for %s %s, got %+v", tc.domain, tc.tool, tc.in, r)
		}
	}
}

func TestV5RulesSelectiveJudgeAndHTTPGatePG(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("test PostgreSQL not configured")
	}
	lock, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	lock.SetMaxOpenConns(1)
	t.Cleanup(func() { lock.Close() })
	if _, err := lock.ExecContext(t.Context(), `SELECT pg_advisory_lock(7337741002)`); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	ic := New(d)

	// 1. V5 seeded rules: restoreFactorySettings -> ask, kubectl delete ns -> deny
	dec, ok := ic.Match("Bash", []byte(`{"command":"curl -X POST https://t/shterm/api/cluster/restoreFactorySettings -d '{}'"}`))
	if !ok || dec.Action != "ask" {
		t.Fatalf("expected V5 ask for restoreFactorySettings, got ok=%v dec=%+v", ok, dec)
	}
	dec2, ok := ic.Match("Bash", []byte(`{"command":"kubectl delete ns prod"}`))
	if !ok || dec2.Action != "deny" {
		t.Fatalf("expected V5 deny for kubectl delete ns, got ok=%v dec=%+v", ok, dec2)
	}
	// Safety filter in Write must NOT false-positive.
	filterWrite, _ := json.Marshal(map[string]string{
		"file_path": "scan.cjs",
		"content":   "const risky = /delete|remove|restart|restore|factory|reboot/i;\nconsole.log('safe');",
	})
	if dec3, matched := ic.Match("Write", filterWrite); matched {
		t.Fatalf("safety filter Write should not match rules, got %+v", dec3)
	}

	// 2. Selective Judge (risk_only mode): read-only skips reviewer, HTTP write triggers reviewer.
	priorCfg := ic.GetJudgeConfig()
	t.Cleanup(func() { _ = ic.SetJudgeConfig(priorCfg) })
	if err := ic.SetJudgeConfig(JudgeConfig{
		Enabled:           true,
		Mode:              "risk_only",
		FailAction:        "ask",
		AskTimeoutSeconds: 60,
		AskTimeoutAction:  "deny",
		HTTPGateEnabled:   true,
		RiskPreset:        "balanced",
	}); err != nil {
		t.Fatal(err)
	}
	var judgeCalls atomic.Int32
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error) {
		judgeCalls.Add(1)
		return Decision{Action: "ask", Message: "实际操作：发送写请求；成功后的后果：可能变更状态；命中规则：ASK"}, nil
	})
	if _, judged := ic.Judge(t.Context(), "Bash", []byte(`{"command":"cat /etc/passwd"}`)); judged {
		t.Fatal("pure read-only Bash should skip LLM judge under risk_only mode")
	}
	if judgeCalls.Load() != 0 {
		t.Fatalf("expected 0 reviewer calls for read-only command, got %d", judgeCalls.Load())
	}
	verdict, judged := ic.Judge(t.Context(), "Bash", []byte(`{"command":"curl -X POST https://t/api/custom/op -d '{}'"}`))
	if !judged || verdict.Action != "ask" || judgeCalls.Load() != 1 {
		t.Fatalf("expected 1 reviewer call returning ask for HTTP POST, got judged=%v verdict=%+v calls=%d", judged, verdict, judgeCalls.Load())
	}

	// 3. Outbound HTTP proxy gate: safe GET allowed; cancelled high-risk POST blocked with 403.
	if ok, _, _ := ic.EvaluateHTTPRequest(t.Context(), "GET", "https://t/shterm/api/user/list"); !ok {
		t.Fatal("expected safe GET to pass HTTP gate")
	}
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if ok, code, _ := ic.EvaluateHTTPRequest(cancelledCtx, "POST", "https://t/shterm/api/cluster/restoreFactorySettings"); ok || code != 403 {
		t.Fatalf("expected cancelled high-risk POST to be blocked with 403, got ok=%v code=%d", ok, code)
	}
}
