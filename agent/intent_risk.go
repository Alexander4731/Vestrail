package agent

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reIntentHardDestructive = regexp.MustCompile(`(?i)(?:restore[-_]?factory[\w-]*|factory[-_]?reset[\w-]*|reset[-_]?factory[\w-]*|restore[-_]?default[\w-]*|reset[-_]?to[-_]?default[\w-]*|reset[-_]?all[\w-]*|format[-_]?(?:disk|volume|system)|wipe[-_]?(?:all|disk|data)|destroy[-_]?(?:cluster|all|db))`)
	reIntentOpsDisrupt      = regexp.MustCompile(`(?i)(?:restart[-_]?(?:tomcat|cluster|service|server|system|node)|batch[-_]?stop[\w-]*|stop[-_]?service[\w-]*|uninstall[\w-]*|flash[-_]?firmware[\w-]*|install[-_]?(?:for[-_]?cluster[-_]?node|patch|firmware)|restore[-_]?(?:configuration|config|backup))`)
	reIntentStaticOnly      = regexp.MustCompile(`(?i)(?:仅静态|只做静态|静态分析|禁止发包|不发包|严禁发送|不触碰|剔除|排除|static[\s_-]*only|read[\s_-]*only)`)
)

const intentOpsWarningPrefix = "【平台高危红线提醒】本意图提及了高危运维/状态变更类接口（如重启、停服、卸载、补丁安装或配置恢复）。严禁使用管理员/高权凭证对这些接口发送 POST/PUT/PATCH/DELETE 或空包 `{}`；仅允许静态分析代码或经人工审批后执行。\n\n"

// checkIntentDestructiveRisk prevents the Planner from dispatching destructive
// factory-reset/format/wipe endpoints as live probing targets (Task #3 root
// cause #1292), and injects a mandatory safety warning when operational
// disruption endpoints are referenced.
func checkIntentDestructiveRisk(it *intentItem) error {
	combined := strings.Join([]string{it.Summary, it.Brief, it.Scope}, "\n")
	if m := reIntentHardDestructive.FindString(combined); m != "" && !reIntentStaticOnly.MatchString(combined) {
		return fmt.Errorf("意图包含高危破坏性操作/接口「%s」（恢复出厂/全量重置/格式化/销毁）。请将其从发包测试清单中剔除，或明确标注「仅静态分析/禁止发包」后再提交", m)
	}
	if reIntentOpsDisrupt.MatchString(combined) && !strings.Contains(it.Brief, "【平台高危红线提醒】") {
		it.Brief = intentOpsWarningPrefix + strings.TrimSpace(it.Brief)
	}
	return nil
}
