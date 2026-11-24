package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if OptimizationEnabled("VESTRAIL_OPT_DATA_CONTRACT") {
			m["summary"] = truncRuneSummary(p["summary"], 200)
		}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上游：本意图派生自哪些节点（多个事实可共同产生一个意图）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下游：本意图产生了哪些事实/发现
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode      int64
	workerIntentID int64
	GoalMet        bool
	Reason         string
	writes         WriteCounts
	writesMu       sync.Mutex
	writtenAssets  map[int64]struct{} // unique asset writes within this run
	artifactDir    string
	contract       kernel.IntentContract
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal).
	resumeTask func()
	// dispatchAdHoc, if set, executes ONE single MainAgent-created intent on a paused
	// or terminal task without reviving the old queue (F23/F24).
	dispatchAdHoc func(intentID int64) error
	// notifyGoal, if set, wakes the planner AND records ONE "人新增了 N 个目标：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "人新增了 N 条战略提示：…"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetDispatchAdHoc wires single-intent ad-hoc dispatch for paused/done tasks (F23/F24).
func (t *ToolSet) SetDispatchAdHoc(fn func(int64) error) { t.dispatchAdHoc = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts                    int
	Assets                   int
	Findings                 int
	DeliveryStatus           kernel.DeliveryStatus
	DeliveryFinalized        bool
	Checkpoint               *kernel.Checkpoint
	ValidBatches             []kernel.EvidenceBatch
	FinalText                string
	AbortCode                kernel.TerminationCode
	ResearchGain             kernel.ResearchGain
	EvidenceIDs              []int64
	ConsumedEvidenceVersions map[int64]int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事实1 资产25 漏洞0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事实%d 资产%d 漏洞%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts {
	t.writesMu.Lock()
	defer t.writesMu.Unlock()
	w := t.writes
	w.EvidenceIDs = append([]int64(nil), w.EvidenceIDs...)
	w.ValidBatches = append([]kernel.EvidenceBatch(nil), w.ValidBatches...)
	w.ConsumedEvidenceVersions = make(map[int64]int, len(t.writes.ConsumedEvidenceVersions))
	for id, version := range t.writes.ConsumedEvidenceVersions {
		w.ConsumedEvidenceVersions[id] = version
	}
	return w
}

func (t *ToolSet) updateWrites(fn func(*WriteCounts)) {
	t.writesMu.Lock()
	defer t.writesMu.Unlock()
	fn(&t.writes)
}

func (t *ToolSet) SetExecutionContract(dir string, contract kernel.IntentContract) {
	t.artifactDir, t.contract = dir, contract
}

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own范围定义 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/自定义 agent/旁路提问). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " 需要任务上下文（探索图）：当前 agent 不在某个任务内运行，取不到任务的探索图，该工具不可用。请在任务内使用它，或改用带 task_id 的跨任务读取工具（get_task_node_detail / list_task_findings / get_task_graph 等）。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索链路图)探索态势蒸馏摘要：资产计数、无接口的站点、frontier、发现、hints(人类/主 agent 的战略提示，生成意图时须纳入)。规划时先调它。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	counts, err := t.ts.NodeCounts()
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	}
	out["goals_total"] = counts[db.KindGoal]
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, moreGoals, err := t.ts.ListByKindPage(db.KindGoal, 0, 100)
	if err != nil {
		return map[string]any{"error": "目标读取失败: " + err.Error()}
	}
	out["goals_truncated"] = moreGoals
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人类/主 agent 通过 add_hint 挂上图的战略提示；folded in so the
	// planner reads them every round when generating intents (否则只写不读).
	if OptimizationEnabled("VESTRAIL_OPT_DATA_CONTRACT") {
		views, resolved, active, err := t.hintViews(nil, 15)
		if err != nil {
			return map[string]any{"error": "提示读取失败: " + err.Error()}
		}
		out["hints"], out["hints_resolved_total"], out["hints_truncated"] = views, resolved, active > int64(len(views))
	} else {
		hints, err := t.ts.ListByKind(db.KindHint, 50)
		if err != nil {
			return map[string]any{"error": "图概览读取失败: " + err.Error()}
		}
		hsum := make([]map[string]any, 0, len(hints))
		for _, h := range hints {
			var p map[string]any
			_ = json.Unmarshal(h.Payload, &p)
			hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
			if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
				hint["traffic_refs"] = p["traffic_refs"]
			}
			hsum = append(hsum, hint)
		}
		out["hints"] = hsum
		out["hints_truncated"] = counts[db.KindHint] > int64(len(hsum))
	}
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, err := t.ts.Edges(5000)
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	}
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, err := t.ts.Frontier(openIntentsCap)
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	} // priority DESC, id ASC —— 优先级最高的前 N 条；真实总数见 frontier_open
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, err := t.ts.ListByKind(db.KindIntent, 300)
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	}
	running, err := t.ts.IntentsInState("running")
	if err != nil {
		return map[string]any{"error": "运行意图读取失败: " + err.Error()}
	}
	paused, err := t.ts.IntentsInState("paused")
	if err != nil {
		return map[string]any{"error": "暂停意图读取失败: " + err.Error()}
	}
	out["paused_intents"] = compactIntents(paused, parentsOf, yieldsOf)
	var recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 最新在前（all 按 id 降序）；折叠的已剔除，输出时截最新 N
		}
	}
	runningCompact := compactIntents(running, parentsOf, yieldsOf)
	if len(running) > 0 {
		runningIDs := make([]int64, 0, len(running))
		for _, n := range running {
			runningIDs = append(runningIDs, n.ID)
		}
		digests, _ := t.ts.LatestRunningDigests(runningIDs)
		for i, n := range running {
			var ip map[string]any
			if json.Unmarshal(n.Payload, &ip) == nil {
				if scope, ok := ip["scope"].(string); ok && strings.TrimSpace(scope) != "" {
					runningCompact[i]["declared_scope"] = truncRuneSummary(scope, 80)
				}
			}
			if d, ok := digests[n.ID]; ok {
				runningCompact[i]["steps"] = d.Steps
				runningCompact[i]["elapsed_s"] = d.ElapsedSec
				runningCompact[i]["recent_errors"] = d.RecentErrors
				if d.LatestAction != "" {
					runningCompact[i]["latest_action"] = firstLine(d.LatestAction, 100)
				}
			}
		}
	}
	out["running_intents"] = runningCompact
	// done_intents_total：已结束意图（done/blocked/exhausted）总数，与 recent_done_intents
	// 平行命名——后者只是它的最新窗口截断视图。两键并排即自描述："看到的是 N/总数"，
	// 让 planner 去重时别把"没显示"当成"没派过"，无需在提示词里另行解释。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	} else {
		return map[string]any{"error": "意图计数失败: " + err.Error()}
	}
	// frontier_open：开放意图真实总数（open_intents 只是其中优先级最高的前 N 条截断视图）。
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["error"] = "待执行意图计数失败: " + err.Error()
	}
	// findings (registered claims) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, err := t.ts.ListByKind(db.KindFinding, 1000)
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	}
	factNodes, err := t.ts.ListByKind(db.KindFact, 1000)
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	} // newest first
	out["findings_total"] = counts[db.KindFinding]
	out["facts"] = counts[db.KindFact]
	// findings 是任务里最高价值的产物 → 概览带最新一窗（≤10 条，vulnNodes 已按 id 降序即最新在前），
	// 让 planner 每轮判目标时一眼看到最近确认的漏洞；全量/更早的用 list_findings 取。
	// 每条只留 {id, summary, from_intent?}：from_intent 是产生本漏洞的意图。
	// evidence/assets/vulnclass/severity/state 等仍可用 list_findings / node_detail(id) 取。
	const findingListCap = 10
	if err := t.ts.PopulateFindingTrafficIDs(vulnNodes[:min(findingListCap, len(vulnNodes))]); err != nil {
		return map[string]any{"error": err.Error()}
	}
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"], "verification": n.FindingVerification, "finding_status": n.FindingStatus}
		if OptimizationEnabled("VESTRAIL_OPT_DATA_CONTRACT") {
			m["summary"] = truncRuneSummary(fp["summary"], 180)
		}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 本漏洞由哪个意图产生
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	out["findings_truncated"] = counts[db.KindFinding] > int64(len(findingList))
	// recent_facts：非折叠事实里最新的一窗（≤N，factNodes 按 id 降序即最新在前）。已折进
	// digest 且仍冷的（hidden）走 cold_digests，不在此重复。每条 {id, summary, from_intent?,
	// confidence?}；evidence 等详情用 node_detail(id)。更早的用 list_facts 翻。
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // 已折进 digest 且仍冷 —— 见 cold_digests
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := kernel.ProjectFactCompact(n.ID, n.State, fp, 0)
		if n.Inherited {
			inheritedMap(m, n.SourceTaskID)
		}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 本事实由哪个意图产生
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	out["facts_truncated"] = counts[db.KindFact] > int64(len(recentFacts))
	// recent_done_intents：非折叠的已结束意图里最新的一窗（≤N，recentDone 已按 id 降序）。
	// 更早的看 done_intents_total 计数 + node_detail(id)。
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 折叠冷区的 digest body，按最新成员时间降序取前 N；被截的更旧 digest
	// 只给裸 id（仍可 expand_digest 展开），避免冷区唯一出口被无限拉长。
	coldDigestsCap := 15
	if OptimizationEnabled("VESTRAIL_OPT_DATA_CONTRACT") {
		coldDigestsCap = 8
	}
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— 直接读 body (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 被截断的更旧 digest 的 id；用 expand_digest(id) 展开
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：粗略的资产测试覆盖度参考——范围(task_scope)内的资产里，被 fact 碰过的
	// 占比 + by_type(按类型的 总数/已测)。要看未测的具体资产由 agent 按需调 list_untested_assets 自行判断。仅任务上下文有。
	// 资产覆盖度关闭时保留任务范围和 host_count，仅省略覆盖度度量。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if scope, err := t.as.ListTaskScope(t.taskID); err == nil {
				rows := make([]map[string]any, 0, len(scope))
				for _, row := range scope {
					entry := map[string]any{"kind": row.Kind, "source": row.Source}
					switch {
					case row.Kind == "collection":
						entry["collection_id"] = row.CollectionID
						entry["collection_name"] = row.CollectionName
						entry["snapshot"] = true
					case row.Domain != "":
						entry["value"] = row.Domain
					case row.Net != "":
						entry["value"] = row.Net
					case row.Value != "":
						entry["value"] = row.Value
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
						entry["company_name"] = row.CompanyName
						if t.cs != nil {
							if rules, err := t.cs.GetScope(*row.CompanyID); err == nil {
								companyScope := make([]map[string]any, 0, len(rules))
								keywords := []string{}
								for _, rule := range rules {
									companyScope = append(companyScope, map[string]any{"kind": rule.Kind, "value": rule.Raw})
									if rule.Kind == "keyword" {
										keywords = append(keywords, rule.Raw)
									}
								}
								entry["company_scope"] = companyScope
								entry["company_keywords"] = keywords
							}
						}
					}
					rows = append(rows, entry)
				}
				m["scope"] = rows
			}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["metric"] = "exploration_reachability"
					m["note"] = "资产探索触达率：只表示资产有事实锚点，包含直接关联任务的只读范围；不代表漏洞类别或身份测试完成。具体测试项与缺口查看 list_coverage_checks。"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "范围未锚定"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// 只给主机总数，不再把 host 列表平铺进 graph_overview（大范围任务里那是每轮
				// 都重复携带的大量字符串，对规划决策价值有限）；具体主机按需 list_assets 查。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	if t.taskID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ledger, err := t.ts.TaskCoverageSummary(ctx)
		cancel()
		if err != nil {
			out["check_coverage"] = map[string]any{"availability": "unavailable", "error": err.Error()}
		} else {
			out["check_coverage"] = ledger
		}
	}
	return out
}

// workerOverviewData returns a compact, worker-focused situational snapshot for
// the worker's launch user message. Unlike graphOverviewData (which carries up
// to 42 intent objects, 15 cold digests, coverage ledgers, and multi-KB
// inherited task blackboards for the planner), workers only need:
//   - task root, goals, hints (human steering)
//   - direct parent nodes of this intent (why this intent was spawned)
//   - recent finding summaries (prevent duplicate reports)
//   - up to 10 compact recent facts (140-rune cap) + lightweight counts
//
// Everything deeper remains one tool call away (node_detail / list_assets /
// search_all_worker_traces / get_worker_trace), cutting per-worker launch
// context by ~70-85% across every turn of the intent run.
func (t *ToolSet) workerOverviewData(intent *db.Node) map[string]any {
	out := map[string]any{}
	if t.ts == nil {
		return out
	}
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	counts, err := t.ts.NodeCounts()
	if err != nil {
		return map[string]any{"error": "图概览读取失败: " + err.Error()}
	}
	if goals, _, err := t.ts.ListByKindPage(db.KindGoal, 0, 50); err == nil {
		gsum := make([]map[string]any, 0, len(goals))
		for _, g := range goals {
			var p map[string]any
			_ = json.Unmarshal(g.Payload, &p)
			gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
		}
		out["goals"] = gsum
	}
	if OptimizationEnabled("VESTRAIL_OPT_DATA_CONTRACT") {
		views, resolved, active, err := t.hintViews(intent, 10)
		if err != nil {
			return map[string]any{"error": "提示读取失败: " + err.Error()}
		}
		out["hints"], out["hints_resolved_total"], out["hints_truncated"] = views, resolved, active > int64(len(views))
		if goals, ok := out["goals"].([]map[string]any); ok {
			for _, g := range goals {
				g["text"] = truncRuneSummary(g["text"], 240)
			}
		}
	} else {
		if hints, err := t.ts.ListByKind(db.KindHint, 30); err == nil && len(hints) > 0 {
			hsum := make([]map[string]any, 0, len(hints))
			for _, h := range hints {
				var p map[string]any
				_ = json.Unmarshal(h.Payload, &p)
				hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
				if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
					hint["traffic_refs"] = p["traffic_refs"]
				}
				hsum = append(hsum, hint)
			}
			out["hints"] = hsum
		}
	}
	edges, _ := t.ts.Edges(5000)
	factFrom := map[int64]int64{}
	var parentIDs []int64
	for _, e := range edges {
		if e.Rel == db.RelYields {
			factFrom[e.To] = e.From
		}
		if intent != nil && e.To == intent.ID && (e.Rel == db.RelDerivedFrom || e.Rel == db.RelSpawns) {
			parentIDs = append(parentIDs, e.From)
		}
	}
	if len(parentIDs) > 0 {
		parents := make([]map[string]any, 0, len(parentIDs))
		for _, pidVal := range parentIDs {
			if n, err := t.ts.GetNodeWithSources(pidVal); err == nil && n != nil {
				item := compactFact(n)
				item["kind"] = n.Kind
				parents = append(parents, item)
			}
		}
		if len(parents) > 0 {
			out["parent_context"] = parents
		}
	}
	const workerFindingCap = 10
	if vulnNodes, err := t.ts.ListByKind(db.KindFinding, workerFindingCap); err == nil {
		_ = t.ts.PopulateFindingTrafficIDs(vulnNodes)
		fl := make([]map[string]any, 0, len(vulnNodes))
		for _, n := range vulnNodes {
			var fp map[string]any
			_ = json.Unmarshal(n.Payload, &fp)
			m := map[string]any{"id": n.ID, "summary": truncRuneSummary(fp["summary"], 160), "verification": n.FindingVerification, "finding_status": n.FindingStatus}
			if from := factFrom[n.ID]; from > 0 {
				m["from_intent"] = from
			}
			fl = append(fl, m)
		}
		out["finding_list"] = fl
	}
	covered, _ := t.ts.CoveredMembers()
	const workerFactsCap = 10
	if factNodes, err := t.ts.ListByKind(db.KindFact, 40); err == nil {
		recentFacts := make([]map[string]any, 0, workerFactsCap)
		for _, n := range factNodes {
			if len(recentFacts) >= workerFactsCap {
				break
			}
			if _, isCovered := covered[n.ID]; isCovered {
				continue
			}
			m := compactNode(n)
			if s, ok := m["summary"].(string); ok {
				m["summary"] = truncRuneSummary(s, 140)
			}
			if from := factFrom[n.ID]; from > 0 {
				m["from_intent"] = from
			}
			var fp map[string]any
			if json.Unmarshal(n.Payload, &fp) == nil {
				if c, ok := fp["confidence"].(string); ok && c != "" {
					m["confidence"] = c
				}
			}
			recentFacts = append(recentFacts, m)
		}
		out["recent_facts"] = recentFacts
	}
	out["facts_total"] = counts[db.KindFact]
	out["findings_total"] = counts[db.KindFinding]
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	}
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	if sources, err := t.ts.DirectSourceStores(); err == nil && len(sources) > 0 {
		out["related_tasks_count"] = len(sources)
	}
	return out
}

func truncRuneSummary(v any, maxRunes int) any {
	s, ok := v.(string)
	if !ok || maxRunes <= 0 {
		return v
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 12_000
	relatedOverviewMaxTextPerSource    = 3_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 被截断的更旧 digest 的 id；expand_digest(id) 展开
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	m["verification"] = n.FindingVerification
	if n.FindingVerification == nil {
		m["verification"] = db.VerificationSummary{State: "legacy_unverified", Assurance: "none", EvidenceVersion: -1}
	}
	m["finding_status"] = n.FindingStatus
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "列本任务及直接关联任务的登记发现。verification 为宿主验证状态，finding_status 为人工处置；旧图 state=confirmed 不代表机器验证。关联任务条目带 source_task_id/inherited=true 且只读。普通探索事实用 list_facts，详情用 node_detail(id)。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, err := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 产生它的 intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "分页或批量多关键词召回本任务及直接关联任务的【探索事实/结论】，最新在前（紧凑投影保留 id/state/summary/confidence/provenance/reentry_when，全文用 node_detail(ids=[...])）。优先用 queries 数组一次合并召回多个关键词（最多 12 个），避免逐词多次调用。返回 {facts, total, has_more, next_before}。漏洞看 list_findings。",
		obj(map[string]any{
			"limit":      intp("返回条数，默认 20，上限 100"),
			"before":     intp("分页游标：只返回 id 小于该值的更旧事实；省略或 0 = 最新一页"),
			"after_id":   intp("增量游标：只返回 id 大于该值的新增事实"),
			"q":          str("单个关键词过滤（不区分大小写，兼容旧调用）"),
			"queries":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "【优先用这个】多关键词数组（最多 12 个），单次合并召回并标注每条命中的 matched_queries"},
			"confidence": str("按置信度过滤：observed | inferred"),
			"provenance": str("按来源过滤：target_response | source_code | derived_diff | guess_wordlist | history_log"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit      int      `json:"limit"`
				Before     int64    `json:"before"`
				AfterID    int64    `json:"after_id"`
				Q          string   `json:"q"`
				Queries    []string `json:"queries"`
				Confidence string   `json:"confidence"`
				Provenance string   `json:"provenance"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			queries := append([]string(nil), a.Queries...)
			if q := strings.TrimSpace(a.Q); q != "" {
				queries = append(queries, q)
			}
			useBatch := len(a.Queries) > 0 || a.AfterID > 0 || strings.TrimSpace(a.Confidence) != "" || strings.TrimSpace(a.Provenance) != ""
			var (
				f       []*db.Node
				matched map[int64][]string
				hasMore bool
				total   int
				err     error
			)
			if useBatch {
				f, matched, hasMore, total, err = t.ts.ListFactsBatchWithSources(db.FactBatchFilter{
					Queries:    queries,
					Confidence: a.Confidence,
					Provenance: a.Provenance,
					Before:     a.Before,
					After:      a.AfterID,
					Limit:      limit,
				})
			} else {
				f, hasMore, total, err = t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			}
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				item := compactFact(n)
				if hits := matched[n.ID]; len(hits) > 0 {
					item["matched_queries"] = hits
				}
				out = append(out, item)
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // 传回它取下一页(更旧的)
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact delegates to kernel.ProjectFactCompact so confidence, provenance,
// reentry_when, and checkpoint indicators are never stripped in list_facts (F11).
func compactFact(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := kernel.ProjectFactCompact(n.ID, n.State, p, factSummaryMax)
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "按 id 或批量 ids（最多 8 个）取本任务或直接关联任务的【探索图节点】完整内容。继承节点带 source_task_id/inherited=true 且只读。仅限探索节点 id；资产请用 list_assets/asset_neighbors。",
		obj(map[string]any{
			"id":  idp("单个探索图节点 id（兼容旧调用）"),
			"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "【批量优先】一次取多个探索节点详情（最多 8 个），省往返"},
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID  json.RawMessage   `json:"id"`
				IDs []json.RawMessage `json:"ids"`
			}
			_ = json.Unmarshal(in, &a)
			if len(a.IDs) > 0 {
				const maxBatchIDs = 8
				const maxBatchBytes = 32 * 1024
				var ids []int64
				seen := map[int64]bool{}
				for _, raw := range a.IDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				if len(ids) == 0 {
					return actool.Errorf("ids 不能为空"), nil
				}
				var omitted []int64
				if len(ids) > maxBatchIDs {
					omitted = append(omitted, ids[maxBatchIDs:]...)
					ids = ids[:maxBatchIDs]
				}
				nodes := make([]*db.Node, 0, len(ids))
				missing := []int64{}
				totalBytes := 0
				for idx, id := range ids {
					n, err := t.ts.GetNodeWithSources(id)
					if err != nil {
						return actool.Errorf(err.Error()), nil
					}
					if n == nil {
						missing = append(missing, id)
						continue
					}
					_ = t.ts.PopulateFindingTrafficIDs([]*db.Node{n})
					sz := len(n.Payload) + 128
					if len(nodes) > 0 && totalBytes+sz > maxBatchBytes {
						omitted = append(omitted, ids[idx:]...)
						break
					}
					totalBytes += sz
					nodes = append(nodes, n)
				}
				res := map[string]any{"nodes": nodes}
				readIDs := make([]int64, 0, len(nodes))
				for _, node := range nodes {
					readIDs = append(readIDs, node.ID)
				}
				t.noteEvidenceRead(ctx, readIDs)
				if len(missing) > 0 {
					res["missing_ids"] = missing
				}
				if len(omitted) > 0 {
					res["omitted_ids"] = omitted
				}
				return jsonResult(res)
			}
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id 或 ids 必填"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("未找到探索节点 %d。若你想查的是资产，请用 list_assets / asset_neighbors（资产与探索节点是不同的 id 空间，资产 id 不能传给 node_detail）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			t.noteEvidenceRead(ctx, []int64{n.ID})
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem 是 add_intent 批量/单条的一条探索方向，兼容旧纯文本与新结构化 IntentContract (F07/F17/F22)。
type intentItem struct {
	Summary          string            `json:"summary"`
	Brief            string            `json:"brief,omitempty"`
	ContinueIntentID json.RawMessage   `json:"continue_intent_id,omitempty"`
	AssetIDs         []json.RawMessage `json:"asset_ids"`
	ParentIDs        []json.RawMessage `json:"parent_ids"`
	Priority         int               `json:"priority"`
	ToolBudget       *int              `json:"max_tool_calls,omitempty"`
	kernel.IntentContract
}

// addOneIntent 创建一条意图节点并连上游血缘，返回 (id, standalone, err)。
// 当由 MainAgent 直投（t.dispatchAdHoc != nil）时走 AddAdHocIntentLinked：
// paused/done 任务上直接原子建为 standalone=true，绝不复活旧队列 (F23/F24)。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	id, _, err := t.addOneIntentMode(it)
	return id, err
}

func (t *ToolSet) addOneIntentMode(it intentItem, requireContract ...bool) (int64, bool, error) {
	if t.ts == nil {
		return 0, false, fmt.Errorf("add_intent 需要任务上下文")
	}
	lock := intentCreationLock(t.ts.ID())
	lock.Lock()
	defer lock.Unlock()
	if strings.TrimSpace(it.Summary) == "" {
		return 0, false, fmt.Errorf("summary 不能为空")
	}
	// 强制分离紧凑索引 summary 与详细执行交底 brief (F07)
	it.Summary, it.Brief = kernel.SplitSummaryAndBrief(it.Summary, it.Brief)
	// 先校验锚点（建节点前，避免坏锚点留下孤儿意图）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, false, fmt.Errorf("parent_id %d 不存在于本任务或直接关联任务：parent_ids 必须是已存在的【事实(fact)/发现(finding)】节点 id；顶层全新方向请留空 parent_ids", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, false, fmt.Errorf("parent_id %d 是 %q 节点，不能作为意图锚点：意图只能锚在已确认的【事实(fact)/发现(finding)】上，不能挂在意图/目标/提示上；如需接力上游意图请使用 continue_intent_id", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	if err := t.normalizeIntentContract(&it, anchors, len(requireContract) > 0 && requireContract[0]); err != nil {
		return 0, false, err
	}
	// 资产拦截：意图绑定的资产若命中系统资产拦截规则，则禁止下发该意图。
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, false, fmt.Errorf("资产拦截校验失败：%w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "意图「%s」绑定的资产未通过测试范围校验，请停止对相关资产进行测试：", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, false, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if brief := strings.TrimSpace(it.Brief); brief != "" {
		payload["brief"] = truncRuneSummary(brief, 3000)
		if payload["brief"] != brief {
			payload["brief_original"] = brief
			payload["brief_truncated"] = true
		}
	}
	if strings.TrimSpace(it.Hypothesis) != "" {
		payload["hypothesis"] = strings.TrimSpace(it.Hypothesis)
	}
	if len(it.Preconditions) > 0 {
		payload["preconditions"] = it.Preconditions
	}
	if strings.TrimSpace(it.Scope) != "" {
		payload["scope"] = strings.TrimSpace(it.Scope)
	}
	if len(it.Inputs) > 0 {
		payload["inputs"] = it.Inputs
	}
	if strings.TrimSpace(it.Deliverable) != "" {
		payload["deliverable"] = strings.TrimSpace(it.Deliverable)
	}
	if len(it.StopConditions) > 0 {
		payload["stop_conditions"] = it.StopConditions
	}
	if it.ToolBudget != nil || it.MaxToolCalls > 0 {
		payload["max_tool_calls"] = it.MaxToolCalls
	}
	if len(it.AfterIntents) > 0 {
		payload["after_intents"] = it.AfterIntents
	}
	if len(it.ResourceLocks) > 0 {
		payload["resource_locks"] = it.ResourceLocks
	}
	if OptimizationEnabled("VESTRAIL_OPT_INTENT_DEDUP") {
		if err := t.checkIntentDuplicate(it); err != nil {
			return 0, false, err
		}
	}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	if err := t.assignIntentLane(it, payload, anchors, parents); err != nil {
		return 0, false, err
	}
	if len(parents) == 0 {
		origin, err := t.ts.OriginFactID()
		if err != nil {
			return 0, false, err
		}
		if origin > 0 {
			parents = append(parents, origin)
		}
	}
	origin := "planner"
	if t.worker == "human" {
		origin = "human"
	}
	if t.dispatchAdHoc != nil {
		id, standalone, err := t.ts.AddAdHocIntentLinked(payload, priority, origin, anchors, parents, db.RelDerivedFrom)
		return id, standalone, err
	}
	id, err := t.ts.AddNodeLinked(db.KindIntent, payload, priority, "open", origin, anchors, parents, db.RelDerivedFrom)
	return id, false, err
}

func (t *ToolSet) addIntent() actool.CoreTool {
	intentProps := map[string]any{
		"summary":            str("≤160 字的目标与核心方向索引；详细背景、参数、路径和验证信号放入 brief。"),
		"brief":              str("完整执行交底：背景线索、凭据/路径、验证信号及止损边界；建议不超过 3000 字。"),
		"continue_intent_id": intp("可选：本任务已 done/exhausted 的上游意图 id，沿同一执行线复用上下文、脚本目录与结构化断点。"),
		"hypothesis":         str("可选：本轮要证实或排除的具体假设"),
		"scope":              str("必填：明确执行范围边界（如目标地址、端口段 22001-23000、候选集），用于起跑自检防漂移"),
		"preconditions":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：必须先核验的前置条件"},
		"inputs":             map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "可选：可直接复用的上游产物 [{path, provenance, valid, note}]"},
		"deliverable":        str("必填：本轮最小交付（含有效阴性结论）"),
		"stop_conditions":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：满足即停止并交回的条件"},
		"max_tool_calls":     intp("可选：执行层硬探测工具调用上限（写回工具不计入）"),
		"after_intents":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "可选：必须全部结束后才可领取的前置意图 id 列表"},
		"resource_locks":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "可选：资源读写锁 [{resource, mode: 'shared'|'exclusive'}]；清理临时通道用 exclusive，消费者用 shared"},
		"asset_ids": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer"},
			"description": "本方向要测试的目标资产 id（尽量传，0/1/多个）；是 list_assets 返回的资产 id，不是探索节点 id。仅纯全局侦察无具体资产时留空。",
		},
		"parent_ids": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer"},
			"description": "上游证据锚点 id（可选，0/1/多个）：严格只填已存在的 fact/finding 节点 id；如需接力上游意图请用 continue_intent_id。",
		},
		"priority": intp("优先级 0-10，默认 5"),
	}
	topProps := map[string]any{
		"intents": map[string]any{
			"type":        "array",
			"description": "【优先用这个】要新增的探索方向数组，按顺序处理。每个元素支持 summary/brief/continue_intent_id/scope/hypothesis/max_tool_calls/after_intents/resource_locks/asset_ids/parent_ids/priority。返回 ids 与数组等长、同序。",
			"items":       obj(intentProps, "summary", "scope", "deliverable"),
		},
		"resolved_hint_ids": map[string]any{
			"type":        "array",
			"description": "可选：本轮已转化为意图或已处理的 hint 节点 id，顺手标为 resolved 避免重复注入。",
			"items":       map[string]any{"type": "integer", "minimum": 1},
		},
	}
	for key, value := range intentProps {
		topProps[key] = value
	}
	return t.writeExpTool("add_intent", "生成【探索方向】写入 frontier，并连入探索链路。意图是开放的探索方向，不是固定类型——用 summary 一句话自由描述要探索/验证/利用什么。\n"+
		"★优先批量：一轮筛出的多个新方向放进 intents 数组一次提交（比逐条调用省往返）。返回 ids 数组，与 intents 等长同序（失败项 id=0，详情见 errors）。单条则省略 intents 直接给顶层 summary。",
		obj(topProps),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents         []intentItem      `json:"intents"`
				ResolvedHintIDs []json.RawMessage `json:"resolved_hint_ids"`
				intentItem                        // 单条模式：顶层 summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			var hintIDs []int64
			for _, raw := range a.ResolvedHintIDs {
				id := pid(raw)
				n, err := t.ts.GetNode(id)
				if id <= 0 || err != nil || n == nil || n.Kind != db.KindHint {
					return actool.Errorf("resolved_hint_ids 必须是本任务的 hint 节点"), nil
				}
				hintIDs = append(hintIDs, id)
			}
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, standalone, err := t.addOneIntentMode(it, true)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				if standalone && t.dispatchAdHoc != nil {
					if derr := t.dispatchAdHoc(id); derr != nil {
						_, _ = t.ts.CompareAndSetIntentState(id, "open", "blocked")
						errs[strconv.Itoa(i)] = "独立执行启动失败: " + derr.Error()
						continue
					}
				}
				ids[i] = id
				createdAny = true
			}

			// 注意(F23/F24)：MainAgent add_intent 不再调用全任务 resumeTask() 唤醒旧队列。
			// paused/done 任务上的直投意图已在上面经 standalone + dispatchAdHoc 独立执行；
			// running 任务上的普通意图则由常规 worker 池自动领取。
			hintError := ""
			if createdAny && len(errs) == 0 && len(hintIDs) > 0 {
				if err := t.ts.ResolveHints(hintIDs, "已转化为本轮创建的意图"); err != nil {
					hintError = err.Error()
				}
			} else if len(hintIDs) > 0 {
				hintError = "部分意图未创建，提示保持 active"
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				result := fmt.Sprintf("intent created: %d", ids[0])
				if hintError != "" {
					result += "\n提示未关闭：" + hintError
				}
				return actool.Text(result), nil
			}
			out := map[string]any{"ids": ids}
			if hintError != "" {
				out["hints_not_resolved"] = hintError
			}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "列出本任务的目标节点及其状态（open/met），用于判断是否达成。",
		obj(map[string]any{}),
		func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
			g, err := t.ts.ListByKind(db.KindGoal, 100)
			if err != nil {
				return actool.Errorf("目标读取失败: " + err.Error()), nil
			}
			if err = t.ts.AttachGoalAssessments(ctx, g); err != nil {
				return actool.Errorf("目标判定读取失败: " + err.Error()), nil
			}
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "当你判断某个发现/事实证明了某个目标达成时调用：把证据节点连到目标节点，并标记目标 met。",
		obj(map[string]any{
			"goal_id":     idp("目标节点 id"),
			"evidence_id": idp("证明它的发现/事实节点 id"),
			"reason":      str("为什么这个证据满足该目标"),
		}, "goal_id", "evidence_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id 和 evidence_id 必填"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id 必须是本任务的目标节点（关联任务目标只读）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id 必须是本任务或直接关联任务的事实/漏洞节点"), nil
			}
			if err := t.ts.AssessGoal(ctx, db.GoalAssessmentInput{GoalID: goal, EvidenceID: ev, Method: db.GoalModelAssessed, Actor: "agent:" + t.worker, Reason: a.Reason}); err != nil {
				return actool.Errorf("目标证明保存失败: " + err.Error()), nil
			}
			// 每标记一个目标 met，就检查本任务是否【所有目标】都已 met；若是，自动判定
			// 任务完成（置 GoalMet），无需再依赖模型显式调 goal_met。
			if allMet, err := t.ts.AllGoalsMet(); err == nil {
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("全部目标已达成（最后由 goal %d 触发）", goal)
					return actool.Text(fmt.Sprintf("goal %d marked met；本任务所有目标均已达成，任务自动判定完成", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【立即结束整个任务】——仅当你确认任务的【全部目标都已真正达成、整体收官】时才调（注意是任务【整体】完成；仅仅达成了其中某一个目标/某一个 flag/某一个漏洞【不算】——那种情况用 prove_goal 标记该目标即可）。⚠️它不是用来“结束本轮规划”的：本轮没有新意图要派、或在等 worker 产出，都【直接结束本轮即可，不要调本工具】（0 个意图是完全正常的）。正常判定优先用 prove_goal 逐个证明目标；goal_met 也会检查全部目标均已达成，不能绕过逐项判定。",
		obj(map[string]any{"reason": str("达成理由（必须是目标真正达成的证据，不能是“本轮无新方向”这类结束本轮的理由）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			if t.ts == nil {
				return actool.Errorf("缺少任务上下文"), nil
			}
			met, err := t.ts.AllGoalsMet()
			if err != nil {
				return actool.Errorf("目标状态读取失败: " + err.Error()), nil
			}
			if !met {
				return actool.Errorf("尚有未证明的目标，请先使用 prove_goal 提交证据"), nil
			}
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "登记候选发现，用 evidence 提供命令输出、日志等可核对材料；登记不授予机器验证结论。任务上下文传当前 intent_id。返回的 finding_id 是独立记录 ID，finding_node_id 是探索节点 ID（第一行保留该节点编号）。", obj(map[string]any{
		"vulnclass": str("漏洞类别"), "name": str("漏洞名称"), "severity": str("critical|high|medium|low"), "summary": str("发现摘要"),
		"intent_id": idp("当前任务的意图 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "受影响资产 id"},
		"evidence":         str("证据/PoC 文本"),
		"evidence_hint_id": idp("可选：本任务中对应此漏洞的提示节点 ID，自动携带其结构化 traffic_refs；不能引用继承提示或其他漏洞的提示"),
		"traffic_refs": map[string]any{"type": "array", "description": "可选；HTTP/HTTPS 漏洞先检索并逐条核实请求/响应确实支持漏洞结论，再按复现顺序填写真实 ID。TCP 等非 HTTP 漏洞、未采集或找不到确切记录时省略或传 []，不阻止上报；可在 evidence 说明原因并提供其他可验证证据。不要猜测 ID、按域名/时间推定关联或仅为补包重复探测。用途 baseline 正常对照 / proof 漏洞证明 / verification 补充验证 / supporting 辅助证据。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search 返回的真实流量 ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("该流量如何支持漏洞结论")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding 需要任务上下文；平台对话请通过 add_task_hint 向对应任务交接漏洞，并在提示中携带已有的 traffic_refs，由任务 Agent 登记。已登记漏洞可用 bind_finding_traffic 补绑。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "已关闭，可在页面人工关联" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id 必须为有效的提示节点 ID；无交接提示时省略"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		iid := pid(a.IntentID)
		if t.workerIntentID > 0 {
			if iid > 0 && iid != t.workerIntentID {
				return actool.Errorf(fmt.Sprintf("写回 intent_id 必须为当前执行意图 #%d", t.workerIntentID)), nil
			}
			iid = t.workerIntentID
		}
		key, err := t.ts.ObservedEvidenceKey(ctx, iid, a.Evidence)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: iid, VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs), Scope: t.contract.Scope, EvidenceKey: key, Deduplicate: true}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("流量证据存储不可用；未登记漏洞"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if recorded.Created && t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if recorded.Created && t.notify != nil {
			t.notify()
		}
		t.updateWrites(func(w *WriteCounts) {
			if recorded.Created {
				w.Findings++
			}
			w.EvidenceIDs = append(w.EvidenceIDs, recorded.NodeID)
			if recorded.NewEvidence {
				w.ResearchGain.Evidence++
			}
		})
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string              `json:"evidence_status"`
			EvidenceNote   string              `json:"evidence_note,omitempty"`
			ResearchGain   kernel.ResearchGain `json:"research_gain"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if recorded.NewEvidence {
			result.ResearchGain.Evidence = 1
		}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "漏洞已保存，未绑定流量。TCP/无包情形可正常继续；若已有核实的 HTTP 流量，请用可用的 bind_finding_traffic 或漏洞页面补绑，再完成证据交接。不要重复创建漏洞。"
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "漏洞已保存。Agent 自动绑定流量已关闭，可在页面人工关联流量。"
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem 是 record_fact 批量/单条的一条事实，支持结构化来源、批次、断点与交付态 (F08/F25/F26/F27/F28)。
type factItem struct {
	Summary               string                 `json:"summary"`
	Detail                string                 `json:"detail"`
	Evidence              string                 `json:"evidence"`   // 一行关键证据（命令+关键输出行），支撑结论、便于事后核对
	Confidence            string                 `json:"confidence"` // observed（直接看到）| inferred（据现象推断）
	ClaimType             string                 `json:"claim_type,omitempty"`
	Provenance            string                 `json:"provenance,omitempty"`
	UnverifiedAssumptions []string               `json:"unverified_assumptions,omitempty"`
	ReentryWhen           string                 `json:"reentry_when,omitempty"`
	DeliveryStatus        kernel.DeliveryStatus  `json:"delivery_status,omitempty"`
	Checkpoint            *kernel.Checkpoint     `json:"checkpoint,omitempty"`
	Batches               []kernel.EvidenceBatch `json:"batches,omitempty"`
	IntentID              json.RawMessage        `json:"intent_id"`
	AssetIDs              []json.RawMessage      `json:"asset_ids"`
}

// recordOneFact 写一条 fact 节点并连到意图（intent→yields→fact）。defaultIntent 为
// 批量时的默认意图（本条未给 intent_id 时用）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	id, _, err := t.recordOneFactWithGain(context.Background(), it, defaultIntent)
	return id, err
}

func (t *ToolSet) recordOneFactWithGain(ctx context.Context, it factItem, defaultIntent int64) (int64, kernel.ResearchGain, error) {
	gain := kernel.ResearchGain{}
	if strings.TrimSpace(it.Summary) == "" {
		return 0, gain, fmt.Errorf("summary 不能为空")
	}
	if err := kernel.ValidateFactRangeConsistency(it.Summary, it.Detail); err != nil {
		return 0, gain, err
	}
	if err := kernel.ValidateFactBatches(it.Batches); err != nil {
		return 0, gain, err
	}
	if err := ValidateScopeBatches(t.contract.Scope, it.Batches); err != nil {
		return 0, gain, err
	}
	payload := map[string]any{"summary": it.Summary, "scope": t.contract.Scope}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	conf, epistemicNote := kernel.NormalizeFactEpistemics(it.Summary, it.Detail, it.Confidence, it.UnverifiedAssumptions)
	if it.ClaimType == "inference" || it.ClaimType == "hypothesis_update" || it.Provenance == "guess_wordlist" || it.Provenance == "history_log" {
		conf = "inferred"
	}
	if conf == "observed" && t.workerIntentID > 0 {
		key, err := t.ts.ObservedEvidenceKey(ctx, t.workerIntentID, it.Evidence)
		if err != nil {
			return 0, gain, err
		}
		if key == "" {
			conf = "inferred"
			epistemicNote = "尚缺本次工具结果中的可核对引用，保留为推断"
		} else {
			payload["evidence_key"] = key
		}
	}
	if conf != "" {
		payload["confidence"] = conf
	}
	if epistemicNote != "" {
		payload["epistemic_note"] = epistemicNote
	}
	if ct := strings.TrimSpace(it.ClaimType); ct != "" {
		payload["claim_type"] = ct
	}
	if prov := strings.TrimSpace(it.Provenance); prov != "" {
		payload["provenance"] = prov
	}
	if len(it.UnverifiedAssumptions) > 0 {
		payload["unverified_assumptions"] = it.UnverifiedAssumptions
	}
	if re := strings.TrimSpace(it.ReentryWhen); re != "" {
		payload["reentry_when"] = re
	}
	if it.DeliveryStatus != "" {
		payload["delivery_status"] = string(it.DeliveryStatus)
	}
	if it.Checkpoint != nil {
		payload["checkpoint"] = it.Checkpoint
	}
	if len(it.Batches) > 0 {
		payload["batches"] = it.Batches
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if t.workerIntentID > 0 {
		if intent > 0 && intent != t.workerIntentID {
			return 0, gain, fmt.Errorf("写回 intent_id 必须为当前执行意图 #%d", t.workerIntentID)
		}
		intent = t.workerIntentID
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, gain, fmt.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	var parents []int64
	if intent > 0 {
		parents = []int64{intent}
	}
	id, created, newEvidence, err := t.ts.RecordFactLinked(payload, t.worker, pidList(it.AssetIDs), parents)
	if err != nil {
		return 0, gain, err
	}
	if newEvidence {
		gain.Evidence = 1
	}
	t.updateWrites(func(w *WriteCounts) {
		if created {
			w.Facts++
		}
		w.EvidenceIDs = append(w.EvidenceIDs, id)
		w.ResearchGain.Evidence += gain.Evidence
		if it.DeliveryStatus != "" && it.DeliveryStatus != kernel.DeliveryComplete {
			w.DeliveryStatus = it.DeliveryStatus
		}
		if it.Checkpoint != nil {
			cp := *it.Checkpoint
			w.Checkpoint = &cp
		}
	})
	return id, gain, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "把探索【事实/结论】写入探索图，连到产生它的意图（intent_id）。用于记录正向结论、有效阴性覆盖、阻塞依据和可接力断点。\n"+
		"⚠️一次探索的多个观察要【汇总成一条事实】，不要拆成多条：summary=总结性一句话，detail=相关细节；facts 数组仅用于彼此独立无法归并的多条结论。\n"+
		"⚠️严谨性要求：\n"+
		"  · evidence=支撑本结论的【一行】关键证据（命令+关键输出），不要粘大段原文。\n"+
		"  · confidence=observed（直接看到且已核实）| inferred（据现象推断/尚未做排除性对照）。局部正常观测不能直接写为排除性根因 observed。\n"+
		"  · 闭区间与唯一覆盖数须严格一致（闭区间 A–B 共 B-A+1 个对象；区分请求数与唯一端口数），无效批次不得核销覆盖。\n"+
		"  · 中间事实写 record_fact；最终交付状态与断点交接统一提交 finish_work(delivery_status, summary, evidence_ids, checkpoint)。",
		obj(map[string]any{
			"facts":                  map[string]any{"type": "array", "description": "【有多条彼此独立结论时用】事实数组，元素字段同下方顶层字段；省略 intent_id 则用顶层 intent_id。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}},
			"summary":                str("对本次探索结论的【总结性一句话】（是对 detail 的概括）"),
			"intent_id":              idp("产生本事实的意图 id（你领到的意图；批量时作为各条默认）"),
			"detail":                 str("本事实的相关细节：把这次探索的多个观察事实都写进这里"),
			"evidence":               str("【一行】关键证据：命令 + 最能证明结论的那一两行输出。务必简洁，不要粘大段输出（细节放 detail）。"),
			"confidence":             str("observed（输出里直接看到且已核实）| inferred（据现象推断）。"),
			"claim_type":             str("可选：observation | inference | negative_coverage | hypothesis_update"),
			"provenance":             str("可选：target_response | source_code | derived_diff | guess_wordlist | history_log"),
			"unverified_assumptions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：尚未排除的候选解释或待对照前提"},
			"reentry_when":           str("可选：何种新条件出现时才值得重入本方向"),
			"delivery_status":        str("可选：complete | partial_checkpoint | precondition_blocked | handed_back"),
			"checkpoint":             map[string]any{"type": "object", "description": "可选：未完成时的结构化断点 {completed_scope, remaining_scope, next_action, valid_artifacts, invalid_artifacts}"},
			"batches":                map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "可选：vestrail.batch.v1 结构化批次结果数组"},
			"asset_ids":              map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "相关资产 id（可选，0/1/多个）：该事实涉及哪些资产"},
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 单条模式 + 批量默认 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 顶层 intent_id = 批量默认

			ids := make([]int64, len(items))
			errs := map[string]string{}
			gain := kernel.ResearchGain{}
			for i, it := range items {
				id, delta, err := t.recordOneFactWithGain(ctx, it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				gain.Evidence += delta.Evidence
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				metadata, _ := json.Marshal(map[string]any{"id": ids[0], "research_gain": gain})
				return actool.Text(fmt.Sprintf("fact recorded: %d\n%s", ids[0], metadata)), nil
			}
			out := map[string]any{"ids": ids, "research_gain": gain}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint 挂一条 hint 节点(active/human)到探索图,可锚定资产,返回 id。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent 自动绑定流量已关闭，未保存携带 traffic_refs 的提示；可在系统设置开启，或仅交接文字")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// 唤醒 planner 不在此处逐条做——由 addHint 在整批写完后统一触发一次（带上提示文本），
	// 避免一次 add_hint 多条提示逐条刷屏 planner 的触发行。
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal 挂一条 goal 节点(open)到探索图:连到任务根(origin fact,rel spawns)。
// origin 取 t.worker(缺省 system):goals 拆解器写入的记 "goals"、主 agent 运行时记
// "human"。唤醒 planner 由 setGoals 在整批写完后统一做(见下),这里只负责落库。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	of, err := t.ts.OriginFactID()
	if err != nil {
		return 0, err
	}
	var parents []int64
	if of > 0 {
		parents = []int64{of}
	}
	return t.ts.AddNodeLinked(db.KindGoal, payload, 0, "open", origin, nil, parents, db.RelSpawns)
}

// setGoals 给【本任务】新增探索目标(goal 节点)。既是目标拆解器的提交工具,也是主
// agent 运行时补目标的工具——同一个受管工具,可在 web 端改描述/schema、按 agent 绑定。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"给【本任务】新增探索目标(goal)。目标=最终可交付/可核验的结果,不是攻击步骤或侦察动作。\n"+
			"★优先批量:多个目标放进 goals 数组一次提交,返回 ids 与之等长同序(失败项 id=0,详情见 errors)。单条则省略 goals 直接给顶层 text。\n"+
			"vulnclass 可选:对应漏洞类(如 SQLi/IDOR),业务逻辑类目标留空。目标是否达成由系统判定标记 met,本工具只负责新增。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【优先用这个】要新增的目标数组,按顺序处理。每个元素:text(必填,一个独立可验证的最终目标)+ vulnclass(可选)。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}},
			"text":      str("[单条] 一个独立可验证的最终目标"),
			"vulnclass": str("[单条] 对应漏洞类(若明确),如 SQLi/IDOR;业务逻辑目标可留空"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals 未启用: ExplorationStore 未初始化"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 单条模式:顶层 text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}
			if t.worker == "goals" {
				payloads := make([]map[string]any, 0, len(items))
				for _, item := range items {
					if strings.TrimSpace(item.Text) == "" {
						return actool.Errorf("text 不能为空"), nil
					}
					payloads = append(payloads, map[string]any{"text": strings.TrimSpace(item.Text), "vulnclass": strings.TrimSpace(item.VulnClass)})
				}
				ids, err := t.ts.CompleteBootstrap(payloads)
				if err != nil {
					return actool.Errorf("初始目标保存失败: " + err.Error()), nil
				}
				return jsonResult(map[string]any{"ids": ids})
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 唤醒 planner(整批一次)。优先 notifyGoal:一次 set_goals 记一条「人新增了
				// N 个目标:…」触发,不逐条刷屏;拆解器/worker 无此回调 → 退回纯 notify(拆解器
				// round-0 连 notify 也没接,即无操作,因为此时 planner 尚未启动)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 主 agent 运行时新增目标 → 把已完成/暂停的任务拉回 running 继续跑(终态门会
				// 吞掉普通 notify,必须显式复活)。仅 mainagent 接了此回调;拆解器/worker 为 nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 单条:保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint 落一条操作约束到 task_constraints。origin 取 t.worker(缺省 system):
// 拆解器写 "goals"、主 agent 写 "human"。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 默认按禁止处理:未标注类型时更保守
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type 必须是 allow 或 deny")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints 给【本任务】新增操作约束(allow=允许做什么 / deny=禁止做什么)。既是目标
// 拆解器 round-0 抽约束的提交工具,也是主 agent 运行时补约束的工具——同一受管工具,可在 web
// 端改描述/schema、按 agent 绑定。约束会被注入 planner/worker 的系统提示以约束探索边界。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"给【本任务】新增操作约束,用来框定探索边界:type=allow(允许做的操作)或 deny(禁止做的操作)。\n"+
			"约束=对『可以/不可以做哪些操作』的规定(如『仅测当前端口,不扫其他端口』『禁止对生产库做写操作』『只允许被动侦察』),不是目标、也不是攻击步骤。\n"+
			"★优先批量:多条放进 constraints 数组一次提交,返回 ids 与之等长同序(失败项 id=0,详情见 errors)。单条则省略 constraints 直接给顶层 text/type。\n"+
			"只登记任务目标/描述里【明确写出】的约束,不要臆造;拿不准类型时用 deny(更保守)。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【优先用这个】要新增的约束数组,按顺序处理。每个元素:text(必填,一条约束)+ type(allow|deny)。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}},
			"text":        str("[单条] 一条操作约束的内容"),
			"type":        str("[单条] allow(允许)或 deny(禁止);缺省按 deny 处理"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints 未启用: ExplorationStore 未初始化"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 单条模式:顶层 text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 单条:保持简单返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "把人类/主 agent 的战略提示挂到探索图，规划者下次生成意图时会读到它。\n"+
		"★优先批量：多条提示放进 hints 数组一次提交（比逐条调用省往返）。返回 ids 数组，与 hints 等长同序（失败项 id=0，详情见 errors）。单条则省略 hints 直接给顶层 text。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【优先用这个】要新增的提示数组，按顺序处理。每个元素字段同下方顶层字段（text/asset_ids/traffic_refs）。返回 ids 与本数组等长、同序。", "items": obj(map[string]any{"text": str("提示内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[单条] 提示内容，如'重点挖认证后接口'"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "锚定的资产 id（可选，0/1/多个）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 单条模式：顶层 text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 唤醒 planner（整批一次）。优先 notifyHint：一次 add_hint 记一条「人新增了
				// N 条战略提示：…」触发，让 planner 明确"本轮由新增 hint 触发"并看到提示内容；
				// 未接该回调时退回纯 notify（bare wake，hint 仍折在图里供其自行读取）。
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "终止一条正在运行的意图(work)。用于叫停跑偏/无意义的探索；被终止的意图标记为 stopped，不再自动重领。先用 get_worker_output 看看它在干嘛再决定。",
		obj(map[string]any{"intent_id": idp("要终止的意图 id（= work 句柄）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 发送终止信号", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("停做 X、聚焦 Y"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "给一条正在运行的意图(work)实时注入纠偏指令，不打断它、不丢已有进展：worker 会在下一步动作前收到你的指令并据此调整。用于'别再走 X、聚焦 Y'这类【意图内】纠偏；若方向整个错了应改用 kill_work 再下新意图。建议先用 get_worker_output 看它在干嘛。",
		obj(map[string]any{
			"intent_id": idp("要纠偏的意图 id（= work 句柄）"),
			"message":   str("给 worker 的纠偏指令，明确让它停止什么、转向什么"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message 必填"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 注入纠偏指令（下一步生效）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or截至中止时的) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "取本任务或直接关联任务某条意图(work)的最终输出结论。关联任务结果带 source_task_id/inherited=true 且只读。正常结束返回其总结；被终止(stopped)/异常的 work 返回其截至中止时的最后输出。",
		obj(map[string]any{"intent_id": idp("意图 id（= work 句柄）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id 不属于本任务或其直接关联任务"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（该 work 尚无任何输出）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（该 work 尚无任何输出）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"查看某条意图(work)的【执行过程】（provenance=history_log，区别于 get_worker_output 只给最终结论）。三种用法：\n"+
			"① 只传 intent_id → **默认按 tail 返回最新步骤**（按时间正序输出，带 total_steps / has_older / next_after_step_id）；传 after_step_id 仅取新增步骤不重复；传 order='head' 看起跑历史；\n"+
			"② intent_id + q → 只返回命中关键字的步骤摘要；\n"+
			"③ intent_id + step_ids → 返回这些步骤的完整内容(detail)；一次最多取 5 个。\n"+
			"不含思考(thinking)步骤。支持直接关联任务的历史 trace；其结果带 source_task_id/inherited=true 且只读。",
		obj(map[string]any{
			"intent_id":     idp("意图 id（= work 句柄）"),
			"order":         str("窗口方向：tail（默认，返回最新步骤）| head（从最旧起跑步骤开始）"),
			"after_step_id": intp("增量游标：只返回 step_id 大于该值的新增步骤（持续跟进在跑 Worker 时优先用）"),
			"q":             str("关键字：只返回摘要/完整输出命中它的步骤（可选；与 step_ids 互斥）"),
			"step_ids":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "要取完整内容的 step_id（来自①/②返回；一次最多取 5 个，多传只返回前 5 个，其余在 omitted_step_ids 里列出）"},
			"limit":         intp("摘要流/检索的返回上限（可选）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID    json.RawMessage   `json:"intent_id"`
				Order       string            `json:"order"`
				AfterStepID int64             `json:"after_step_id"`
				Q           string            `json:"q"`
				StepIDs     []json.RawMessage `json:"step_ids"`
				Limit       int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id 不属于本任务或其直接关联任务"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "provenance": "history_log", "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"每次最多取 %d 个步骤的完整内容，本次已返回前 %d 个（%v），未取的 %d 个为 %v。"+
							"若这些内容已足够定位，则无需再取剩余步骤；确需继续时，用这些 step_id 再调一次。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ② keyword search within one work
			if strings.TrimSpace(a.Q) != "" {
				acts, err := t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				result := map[string]any{"intent_id": id, "provenance": "history_log", "steps": traceSteps(acts)}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ① windowed tail/head/cursor stream (F10)
			page, err := t.ts.ActivityTraceWindowWithSources(db.ActivityTraceQuery{
				NodeID:      id,
				Order:       a.Order,
				AfterStepID: a.AfterStepID,
				Limit:       a.Limit,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{
				"intent_id":          id,
				"provenance":         "history_log",
				"steps":              traceSteps(page.Steps),
				"total_steps":        page.TotalSteps,
				"has_older":          page.HasOlder,
				"has_newer":          page.HasNewer,
				"next_after_step_id": page.NextAfterStepID,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【通常不推荐使用，因为系统中已经给了大部分信息了】在【本任务其他 work 的执行过程】里按关键字(q)检索——用于找回某个 worker 见过、却没写进 fact 的东西（某路径/token/报错等）。"+
			"已自动排除你自己这条意图的步骤（那些本就在你上下文里）。"+
			"只返回命中步骤的摘要(summary≤100字)，每条带 intent_id；据此再用 get_worker_trace(intent_id, step_ids=[...]) 取完整内容。",
		obj(map[string]any{
			"q":     str("关键字（在所有 work 步骤的摘要+完整输出里搜）"),
			"limit": intp("返回上限，默认 100（可选）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q 必填"), nil
			}
			// 排除调用者自身这条意图的步骤（worker 的自有 trace 已在其上下文里）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【通常不推荐使用，因为系统中已经给了大部分信息了】列出本任务里【已跑过的 work（意图）】索引：intent_id + 一句话方向(summary) + 状态。"+
			"你(worker)看不到探索图，用它来发现有哪些 work 值得翻看——再用 get_worker_trace(intent_id) 看其步骤、get_worker_trace(intent_id, step_ids=[...]) 取详情。"+
			"只列已执行的(running/done/exhausted/blocked/stopped)，不含还没跑的 open。注意：你的任务边界仍是你领到的那条意图，看别的 work 只为复用观察/避免重复劳动。",
		obj(map[string]any{
			"q":     str("按 summary 关键字过滤（可选）"),
			"limit": intp("返回上限，默认 50（可选）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(), t.resolveHint(),
		// report_finding：规划态势研判时若自身已确证漏洞，可直接登记（与 worker 同工具）。
		t.addFinding(),
		// list_companies：查看企业列表 + scope + 资产数（拿 company_id / 理解归属范围）。
		t.listCompanies(),
		// list_assets：规划时按 DSL 检索全资产库（配合 list_untested_assets 的"范围内未测"视角，
		// 补上"按域名/指纹/端口/状态码等条件在整库里查"的能力）。
		t.listAssets(),
		// add_company_scope：规划时可把域名/IP/CIDR/ICP/关键词纳入某公司的资产范围（自动认领命中资产）。
		t.addCompanyScope(),
		// add_task_scope：主动把整根域/整公司/某子域/IP 纳入本任务测试范围(覆盖度分母)。
		t.addTaskScope(),
		// list_untested_assets：按需查本任务范围内未测资产(类型+分页)，自行决定补测。
		t.listUntestedAssets(),
		t.listCoverageChecks(), t.recordCoverageCheck(),
	}
}
