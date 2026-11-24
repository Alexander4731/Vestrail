package report

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Alexander4731/Vestrail/db"
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

func TestFindingFilenameUTF8AndSevRank(t *testing.T) {
	longTitle := strings.Repeat("中文漏洞标题测试", 20)
	f1 := &db.DBFinding{ID: 101, Severity: "high", Name: longTitle}
	f2 := &db.DBFinding{ID: 102, Severity: "high", Name: longTitle}
	n1, n2 := FindingFilename(f1), FindingFilename(f2)
	if !utf8.ValidString(n1) || !utf8.ValidString(n2) {
		t.Fatalf("corrupted utf8 filename: %q / %q", n1, n2)
	}
	if n1 == n2 || !strings.HasSuffix(n1, "_101.md") || !strings.HasSuffix(n2, "_102.md") {
		t.Fatalf("filename collision or missing ID suffix: %q vs %q", n1, n2)
	}
	if !(sevRank("critical") < sevRank("HIGH") && sevRank("high") < sevRank("medium") && sevRank("medium") < sevRank("low") && sevRank("low") < sevRank("info") && sevRank("info") < sevRank("unknown")) {
		t.Fatalf("unexpected severity ordering: critical=%d high=%d medium=%d low=%d info=%d unknown=%d",
			sevRank("critical"), sevRank("HIGH"), sevRank("medium"), sevRank("low"), sevRank("info"), sevRank("unknown"))
	}
}
