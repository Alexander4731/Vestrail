package agent

import (
	"strings"
	"testing"
)

func TestCheckIntentDestructiveRisk(t *testing.T) {
	// 1. Unconstrained factory-reset intent must be rejected.
	bad := intentItem{
		Summary: "探测集群接口",
		Brief:   "次级：cluster/restoreFactorySettings、sysParam/restartTomcat",
	}
	bad.Scope = "admin+user 双会话"
	if err := checkIntentDestructiveRisk(&bad); err == nil {
		t.Fatal("expected factory-reset intent without static-only guard to be rejected")
	}

	// 2. Static-only analysis intent mentioning restoreFactorySettings is allowed (with ops warning).
	staticOK := intentItem{
		Summary: "仅静态分析高危端点鉴权",
		Brief:   "仅静态分析 cluster/restoreFactorySettings 与 sysParam/restartTomcat 的前端鉴权逻辑，禁止发包",
	}
	if err := checkIntentDestructiveRisk(&staticOK); err != nil {
		t.Fatalf("expected static-only intent to pass, got %v", err)
	}
	if !strings.Contains(staticOK.Brief, "【平台高危红线提醒】") {
		t.Fatalf("expected ops warning prefix on brief, got %q", staticOK.Brief)
	}

	// 3. Normal recon intent stays untouched.
	normal := intentItem{Summary: "枚举普通列表接口", Brief: "仅 GET /api/user/list"}
	if err := checkIntentDestructiveRisk(&normal); err != nil {
		t.Fatalf("unexpected error on normal intent: %v", err)
	}
	if strings.Contains(normal.Brief, "【平台高危红线提醒】") {
		t.Fatalf("normal intent should not receive ops warning")
	}
}
