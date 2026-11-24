package kernel

import (
	"strings"
	"testing"
)

func TestValidateFactRangeConsistency(t *testing.T) {
	tests := []struct {
		name, summary, detail, wantError string
	}{
		{name: "bare range wrong total", summary: "22001-23000 共扫描 2000 个端口", wantError: "1000 个唯一对象"},
		{name: "thousands separators", summary: "22,001–23,000，共检测 2,000 个端口；批次一 500、批次二 500", wantError: "1000 个唯一对象"},
		{name: "label and one item", detail: "结果如下。\n* 闭区间：12–12，共扫描 2 项。", wantError: "1 个唯一对象"},
		{name: "second claim", detail: "1–2 共扫描 2 项；10–16 共扫描 8 项", wantError: "7 个唯一对象"},
		{name: "correct total", summary: "范围 22,001–23,000，共检测 1,000 个端口。"},
		{name: "single object", summary: "0–0 共 1 项"},
		{name: "largest supported inclusive range", summary: "0–9223372036854775807 共 9223372036854775808 项"},
		{name: "request count is a different unit", summary: "22001–23000，共 2000 次请求"},
		{name: "subset after unit", detail: "22001–23000，共 20 个端口开放。"},
		{name: "subset before range", detail: "开放的端口分布在 22001–23000，共 20 个端口。"},
		{name: "identifier", detail: "fixture-22001–23000 共 20 项。"},
		{name: "reversed range", detail: "23–22 共 0 项"},
		{name: "unrepresentable number", detail: "0–9223372036854775808 共 1 项"},
		{name: "malformed thousands separators", detail: "22,01–23,000，共 2 项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateFactRangeConsistency(test.summary, test.detail)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}
