package intercept

import (
	"encoding/json"
	"regexp"
	"strings"
)

// RiskAssessment captures the deterministic pre-triage classification of a tool
// or HTTP call across the R1–R6 risk taxonomy.
type RiskAssessment struct {
	Level       string   `json:"level"`        // "low" | "medium" | "high" | "critical"
	Domain      string   `json:"domain"`       // "" | "R1".."R6"
	DomainLabel string   `json:"domain_label"` // human-readable Chinese label
	Signals     []string `json:"signals,omitempty"`
	NeedsJudge  bool     `json:"needs_judge"`
}

var (
	reRiskR1 = regexp.MustCompile(`(?i)\b(?:mkfs|diskpart|format-volume|clear-disk|shred|wipe|pulumi\s+destroy|terraform\s+destroy)\b|\bformat\s+[a-z]:|\bdd\b[^|\n]{0,80}\bof=\s*/dev/|\brm\b[^|\n]{0,60}-(?:[a-z]*r[a-z]*f|[a-z]*f[a-z]*r)|kubectl\s+delete\s+(?:ns|namespace|all|cluster|node|pv|pvc)`)
	reRiskR2 = regexp.MustCompile(`(?i)(?:restore[_-]?factory|factory[_-]?reset|reset[_-]?factory|factory[_-]?restore|restore[_-]?default|reset[_-]?to[_-]?default|reset[_-]?default|reset[_-]?all|reset[_-]?system|sys[_-]?reset|reset[_-]?config|reinit|init[_-]?system|clear[_-]?all|wipe[_-]?all|purge[_-]?all|drop\s+(?:database|table|schema)|truncate\s+(?:table\s+)?\w|flushall|flushdb|dropdatabase)`)
	reRiskR3 = regexp.MustCompile(`(?i)(?:/|\b)(?:restart[_-]?\w*|reboot[_-]?\w*|shutdown[_-]?\w*|poweroff|halt|stop[_-]?service\w*|batch[_-]?stop\w*|kill[_-]?service\w*|disable[_-]?service\w*|systemctl\s+(?:stop|disable|mask|reboot|poweroff)|stop-service|restart-computer|stop-computer|iptables\s+-F|nft\s+flush)`)
	reRiskR4 = regexp.MustCompile(`(?i)(?:/|\b)(?:uninstall\w*|deinstall\w*|flash[_-]?(?:firmware|image)|install[_-]?(?:patch|firmware|agent|for[_-]?cluster[_-]?node)|upgrade[_-]?(?:system|firmware|cluster)|restore[_-]?(?:config|configuration|backup|snapshot|database|db)|rollback\w*|deploy[_-]?node|helm\s+uninstall|(?:apt|apt-get|yum|dnf|apk|pacman|brew|choco|winget)\s+(?:remove|purge|uninstall|erase)|rpm\s+-e|msiexec[^\n]*?/x)`)
	reRiskR5 = regexp.MustCompile(`(?i)(?:/(?:delete|remove|unlink|erase|destroy|del)\b|reset[_-]?(?:user|admin)?[_-]?(?:pwd|pass|password)|change[_-]?admin[_-]?p(?:wd|ass)|disable[_-]?(?:user|admin|account)|lock[_-]?(?:user|account)|ban[_-]?(?:user|account)|revoke[_-]?all|(?:^|[;&|\s"'])(?:passwd|chpasswd|userdel|deluser)\s+\S|\bcrontab\s+-r\b)`)

	// Unbounded SQL DELETE / UPDATE without a WHERE clause.
	reSQLDeleteStmt = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+[` + "`" + `"\[]?\w+[` + "`" + `"\]]?`)
	reSQLUpdateStmt = regexp.MustCompile(`(?i)\bUPDATE\s+[` + "`" + `"\[]?\w+[` + "`" + `"\]]?\s+SET\b`)
	reSQLWhere      = regexp.MustCompile(`(?i)\bWHERE\b`)

	// HTTP state-mutating signals in shell/script/tool inputs.
	reHTTPMutation = regexp.MustCompile(`(?i)(?:-X\s*(?:POST|PUT|PATCH|DELETE)|--request\s+(?:POST|PUT|PATCH|DELETE)|--data(?:-raw|-binary|-urlencode)?\b|\s-d\s|\s-F\s|\.(?:post|put|patch|delete)\s*\(|method\s*[:=]\s*['"](?:POST|PUT|PATCH|DELETE)['"]|[,(]\s*['"](?:POST|PUT|PATCH|DELETE)['"]\s*[,)])`)
	// Admin / privileged credential + batch write probe signal (Task #3 pattern).
	reAdminPrivSignal = regexp.MustCompile(`(?i)(?:admin\.jar|superman|role_admin|admin_token|admin_cookie|administrator|root_token)`)
	// High-impact local/remote execution commands worth semantic review when not purely read-only.
	reStateMutatingShell = regexp.MustCompile(`(?i)\b(?:chmod|chown|usermod|groupmod|iptables|nft|ufw|firewall-cmd|sysctl|mount|umount|mkswap|swapon|crontab|at\s|schtasks|reg\s+(?:add|delete)|sc\s+(?:stop|delete|config)|netsh|Set-ItemProperty|New-Service|Remove-Item|Move-Item|Clear-Content)\b`)
)

// readOnlyTools are never state-mutating and always fast-allow under risk_only mode.
var readOnlyTools = map[string]bool{
	"Read":           true,
	"Glob":           true,
	"Grep":           true,
	"shell_read":     true,
	"shell_list":     true,
	"shell_close":    true,
	"web_search":     true,
	"traffic_search": true,
	"traffic_get":    true,
	"traffic_blob":   true,
	"list_assets":    true,
	"list_facts":     true,
	"list_findings":  true,
	"node_detail":    true,
	"graph_overview": true,
}

// ClassifyRisk evaluates a tool call (or "http_proxy" request) across the R1–R6
// taxonomy to power both selective LLM judging (risk_only mode) and UI risk badges.
func ClassifyRisk(toolName string, input []byte) RiskAssessment {
	if readOnlyTools[toolName] || IsBenignDocWrite(toolName, input) {
		return RiskAssessment{Level: "low"}
	}
	sanitized := SanitizeWriteInputForMatch(toolName, input)
	norm := NormalizeForMatch(sanitized)

	// R1–R5 deterministic domain signals (highest severity wins).
	if m := reRiskR1.FindString(norm); m != "" {
		return RiskAssessment{
			Level:       "critical",
			Domain:      "R1",
			DomainLabel: "R1 · 毁灭性擦除与格式化",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}
	if m := reRiskR2.FindString(norm); m != "" {
		return RiskAssessment{
			Level:       "critical",
			Domain:      "R2",
			DomainLabel: "R2 · 恢复出厂与全量重置",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}
	if m := reRiskR3.FindString(norm); m != "" {
		return RiskAssessment{
			Level:       "high",
			Domain:      "R3",
			DomainLabel: "R3 · 服务中断与重启停服",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}
	if m := reRiskR4.FindString(norm); m != "" {
		return RiskAssessment{
			Level:       "high",
			Domain:      "R4",
			DomainLabel: "R4 · 卸载、刷机与变更覆盖",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}
	if m := reRiskR5.FindString(norm); m != "" {
		return RiskAssessment{
			Level:       "high",
			Domain:      "R5",
			DomainLabel: "R5 · 数据删除与账号篡改",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}
	if (reSQLDeleteStmt.MatchString(norm) || reSQLUpdateStmt.MatchString(norm)) && !reSQLWhere.MatchString(norm) {
		return RiskAssessment{
			Level:       "high",
			Domain:      "R5",
			DomainLabel: "R5 · 无条件 SQL 删除/覆写",
			Signals:     []string{"SQL DELETE/UPDATE without WHERE"},
			NeedsJudge:  true,
		}
	}

	// R6: privileged batch write / state-mutating HTTP requests or system changes.
	isHTTPWrite := reHTTPMutation.MatchString(sanitized)
	if toolName == "http_proxy" {
		var req struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(input, &req) == nil {
			m := strings.ToUpper(strings.TrimSpace(req.Method))
			isHTTPWrite = m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE"
		}
	}
	if isHTTPWrite {
		if reAdminPrivSignal.MatchString(norm) {
			return RiskAssessment{
				Level:       "high",
				Domain:      "R6",
				DomainLabel: "R6 · 高权凭据写接口调用",
				Signals:     []string{"privileged HTTP write"},
				NeedsJudge:  true,
			}
		}
		// Write/Edit merely saving a non-destructive script is reviewed when executed
		// via Bash/RunScript (with script dereferencing); still judge RunScript/Bash/WebFetch.
		if toolName != "Write" && toolName != "Edit" && toolName != "MultiEdit" {
			return RiskAssessment{
				Level:       "medium",
				Domain:      "R6",
				DomainLabel: "R6 · 状态变更类 HTTP 写请求",
				Signals:     []string{"HTTP state mutation"},
				NeedsJudge:  true,
			}
		}
	}

	if m := reStateMutatingShell.FindString(norm); m != "" && toolName != "Write" && toolName != "Edit" && toolName != "MultiEdit" {
		return RiskAssessment{
			Level:       "medium",
			Domain:      "R3",
			DomainLabel: "R3 · 系统配置/权限变更",
			Signals:     []string{strings.TrimSpace(m)},
			NeedsJudge:  true,
		}
	}

	// External MCP mutating tools (non-readonly) warrant semantic review in risk_only mode.
	if strings.HasPrefix(toolName, "mcp__") {
		return RiskAssessment{
			Level:       "medium",
			Domain:      "R6",
			DomainLabel: "R6 · 外部 MCP 写操作",
			Signals:     []string{toolName},
			NeedsJudge:  true,
		}
	}

	return RiskAssessment{Level: "low"}
}
