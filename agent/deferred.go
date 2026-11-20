package agent

import (
	"encoding/json"
	"strings"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// deferredSystem builds an agent's system-prompt segments and cache boundary from
// its DeferredInfo. When globally-available deferred tools are present, their names
// + a "prefer core tools" instruction render into a <available-deferred-tools> block
// placed as the LAST static system-prompt segment. DynamicBoundary is always set to
// len(system) so the entire static system prompt (plus tools + rolling messages on
// Anthropic) participates in prompt caching even when no deferred tools exist.
func deferredSystem(sysText string, def DeferredInfo) (system []string, boundary int) {
	return deferredSystemWithDynamic(sysText, "", def)
}

// deferredSystemWithDynamic builds a two-zone system prompt: a static prefix
// (role instructions + constraints + optional deferred-tool index) cached up to
// DynamicBoundary, followed by a per-run dynamic tail (e.g. a worker's claimed
// intent, recovery summary, and target asset payload). Keeping per-intent data
// after DynamicBoundary preserves compaction immunity while letting all workers
// in the same task share the cached static prefix and tool schemas.
func deferredSystemWithDynamic(staticSys, dynamicTail string, def DeferredInfo) (system []string, boundary int) {
	staticSys += def.FindingGuidance
	system = []string{staticSys}
	if block := actool.RenderDeferredToolsBlock(def.GlobalNames); block != "" {
		system = append(system, block)
	}
	boundary = len(system)
	if tail := strings.TrimSpace(dynamicTail); tail != "" {
		system = append(system, tail)
	}
	return system, boundary
}

// deferBuiltinTools moves low-frequency built-in tools that are present in tools
// into DeferredInfo (schema withheld, advertised by name and callable on demand
// via SearchExtraTools / ExecuteExtraTool). Tools absent from tools (disabled or
// unbound by DB policy) are ignored. Existing MCP/skill-gated locks are preserved.
func deferBuiltinTools(tools []actool.CoreTool, def DeferredInfo, names ...string) DeferredInfo {
	if len(names) == 0 || len(tools) == 0 {
		return def
	}
	present := make(map[string]bool, len(tools))
	for _, t := range tools {
		present[t.Name()] = true
	}
	seenDeferred := make(map[string]bool, len(def.Deferred))
	for _, n := range def.Deferred {
		seenDeferred[n] = true
	}
	seenGlobal := make(map[string]bool, len(def.GlobalNames))
	for _, n := range def.GlobalNames {
		seenGlobal[n] = true
	}
	var added []string
	for _, n := range names {
		if !present[n] {
			continue
		}
		if !seenDeferred[n] {
			def.Deferred = append(def.Deferred, n)
			seenDeferred[n] = true
		}
		if !seenGlobal[n] {
			def.GlobalNames = append(def.GlobalNames, n)
			seenGlobal[n] = true
			added = append(added, n)
		}
	}
	if len(added) > 0 {
		if def.Unlock == nil {
			def.Unlock = actool.NewUnlockSet(def.GlobalNames...)
		} else {
			def.Unlock.Add(added...)
		}
	}
	return def
}

// seedUnlockFromHistory replays prior Skill() invocations in the conversation so
// their skill-gated MCPs are re-unlocked on a resumed session (design doc C2). The
// main agent builds a fresh session each turn; its in-memory unlock set would
// otherwise reset, leaving the model able to see a skill-revealed tool name yet
// unable to call it. No-op when unlockSkill is nil (no deferred tools).
func seedUnlockFromHistory(msgs []llm.Message, unlockSkill func(string)) {
	if unlockSkill == nil {
		return
	}
	for _, m := range msgs {
		for _, b := range m.ToolUses() {
			if b.Name != "Skill" {
				continue
			}
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
				unlockSkill(in.Name)
			}
		}
	}
}
