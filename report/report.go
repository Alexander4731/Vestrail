// Package report renders a deliverable Markdown report from the exploration
// graph, preserving the distinction between claims and verified evidence.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Alexander4731/Vestrail/db"
)

// Input bundles what the report needs.
type Input struct {
	Title         string
	Goal          string
	GeneratedAt   time.Time
	AssetCounts   map[string]int
	Findings      []*db.Node
	TaskID        int64
	TaskStatus    string
	Goals         []*db.Node
	FindingsTotal int
	GoalsTotal    int
	Limitations   []string
}

type findingView struct {
	Verification *db.VerificationSummary
	Status       string
	VulnClass    string
	Name         string
	Severity     string
	Summary      string
	PoC          string
}

func parseFinding(n *db.Node) findingView {
	var p struct {
		VulnClass string `json:"vulnclass"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Evidence  struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return findingView{Verification: n.FindingVerification, Status: n.FindingStatus, VulnClass: p.VulnClass, Name: p.Name, Severity: p.Severity, Summary: p.Summary, PoC: p.Evidence.PoC}
}

func sevRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	case "info":
		return 4
	default:
		return 5
	}
}

// Markdown renders the report.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 渗透测试报告 — %s\n\n", nz(in.Title, "未命名任务"))
	fmt.Fprintf(&b, "- **任务目标**：%s\n", nz(in.Goal, "（未指定）"))
	fmt.Fprintf(&b, "- **生成时间**：%s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	// summary
	fmt.Fprintf(&b, "## 摘要\n\n")
	fmt.Fprintf(&b, "- 登记发现：**%d** 个（不等同于机器已验证）\n", len(in.Findings))
	if in.TaskID > 0 {
		fmt.Fprintf(&b, "- **任务**：#%d；状态：%s\n", in.TaskID, in.TaskStatus)
		fmt.Fprintf(&b, "- 发现导出：%d / %d；目标导出：%d / %d\n", len(in.Findings), in.FindingsTotal, len(in.Goals), in.GoalsTotal)
		if in.TaskStatus != "done" {
			b.WriteString("- 本任务尚未完整结束；以下为当前已保存的部分结果。\n")
		}
	}
	fmt.Fprintf(&b, "- 资产：")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s %d", t, in.AssetCounts[t])
	}
	b.WriteString("\n\n")
	if len(in.Limitations) > 0 {
		b.WriteString("## 数据与范围限制\n\n")
		for _, limitation := range in.Limitations {
			fmt.Fprintf(&b, "- %s\n", limitation)
		}
		b.WriteString("\n")
	}
	if len(in.Goals) > 0 {
		b.WriteString("## 逐项目标\n\n")
		for _, g := range in.Goals {
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(g.Payload, &payload)
			state := "未完成"
			if g.State == "met" {
				state = "已判定达成（按任务记录）"
			}
			fmt.Fprintf(&b, "- #%d [%s] %s\n", g.ID, state, nz(payload.Text, "（目标正文缺失）"))
			if a := g.GoalAssessment; a != nil {
				freshness := "当前证据版本"
				if a.Stale {
					freshness = "证据或目标已变化，需重新判定"
				}
				fmt.Fprintf(&b, "  - 判定：%s；%s；证据 #%d；理由：%s\n", a.Method, freshness, a.EvidenceID, nz(a.Reason, "（旧调用未提供理由）"))
			} else if g.State == "met" {
				b.WriteString("  - 历史判定；没有新格式的判定审计记录。\n")
			}
		}
		b.WriteString("\n")
	}

	// findings
	fmt.Fprintf(&b, "## 发现\n\n")
	if len(in.Findings) == 0 {
		b.WriteString("_当前没有登记发现；这不代表测试完整或目标不存在漏洞。_\n\n")
	} else {
		fs := make([]findingView, 0, len(in.Findings))
		for _, n := range in.Findings {
			fs = append(fs, parseFinding(n))
		}
		sort.SliceStable(fs, func(i, j int) bool { return sevRank(fs[i].Severity) < sevRank(fs[j].Severity) })
		for i, f := range fs {
			fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), nz(f.Name, nz(f.VulnClass, "未分类")))
			label := "历史未验证"
			if v := f.Verification; v != nil {
				label = map[string]string{"candidate": "候选发现", "running": "验证中", "verified": "机器已验证（限定判据）", "not_reproduced": "未复现", "inconclusive": "无法判定", "legacy_unverified": "历史未验证"}[v.State]
				if v.Stale {
					label = "验证已过期，需要重新验证"
				}
				if v.Assurance == "evidence_bound" && !v.Stale {
					label += "；仅证据来源/完整性已核对"
				}
			}
			fmt.Fprintf(&b, "- 验证：%s；人工处置：%s\n\n", label, nz(f.Status, "未记录"))
			fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
			if f.PoC != "" {
				fmt.Fprintf(&b, "**PoC / 证据：**\n\n```\n%s\n```\n\n", f.PoC)
			}
		}
	}

	return b.String()
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
