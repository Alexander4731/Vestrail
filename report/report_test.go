package report

import (
	"encoding/json"
	"github.com/Alexander4731/Vestrail/db"
	"strings"
	"testing"
)

func TestMarkdownDoesNotOverstateCandidateOrPartialTask(t *testing.T) {
	out := Markdown(Input{TaskID: 1, TaskStatus: "timeout", FindingsTotal: 1001, GoalsTotal: 1,
		Findings: []*db.Node{{Payload: json.RawMessage(`{"summary":"candidate only"}`)}},
		Goals:    []*db.Node{{ID: 2, State: "open", Payload: json.RawMessage(`{"text":"check permissions"}`)}}, Limitations: []string{"导出截断"}})
	for _, want := range []string{"登记发现", "部分结果", "1 / 1001", "未完成", "导出截断"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in report", want)
		}
	}
	if strings.Contains(out, "确认发现：") {
		t.Fatal("candidate presented as confirmed")
	}
	if !strings.Contains(Markdown(Input{}), "不代表测试完整") {
		t.Fatal("empty report implies safety")
	}
}
