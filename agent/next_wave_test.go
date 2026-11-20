package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/guard"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type waveProvider struct {
	requests []llm.CompletionRequest
	run      func(llm.CompletionRequest, int) []llm.StreamEvent
}

func (p *waveProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.requests = append(p.requests, req)
	events := []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 1200, CacheReadTokens: 900}}, {Type: llm.SEThinkingDelta, Text: "chain thought"}, {Type: llm.SETextDelta, Text: "fixture complete"}, {Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 20}}}
	if p.run != nil {
		events = p.run(req, len(p.requests))
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
}
func (p *waveProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	a := llm.NewAccumulator()
	for e, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		a.Add(e)
	}
	return a.Message(), a.StopReason, a.Usage, nil
}
func waveGraph(t *testing.T) (*db.DB, *db.ExplorationStore, int64) {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, e := d.CreateTaskWithOptions("synthetic next wave", "read-only fixture", db.TaskCreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	return d, d.Exploration(task.ExplorationID), task.ID
}
func waveIntent(t *testing.T, ts *db.ExplorationStore, payload map[string]any) *db.Node {
	t.Helper()
	id, e := ts.AddIntent(payload, 5, nil, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	n, e := ts.GetNode(id)
	if e != nil {
		t.Fatal(e)
	}
	return n
}

func TestIntentSummaryBriefSeparation(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_INTENT_DEDUP", "1")
	_, ts, _ := waveGraph(t)
	tools := NewToolSet(ts, "planner")
	brief := strings.Repeat("完整验证说明", 900)
	id, e := tools.addOneIntent(intentItem{Summary: "检查本地固定文本", Brief: brief})
	if e != nil {
		t.Fatal(e)
	}
	n, e := ts.GetNode(id)
	if e != nil {
		t.Fatal(e)
	}
	var p map[string]any
	json.Unmarshal(n.Payload, &p)
	if p["brief_original"] != brief || len([]rune(p["brief"].(string))) > 3000 {
		t.Fatal("full brief not preserved")
	}
	index, _ := json.Marshal(compactIntents([]*db.Node{n}, nil, nil))
	if strings.Contains(string(index), "完整验证说明") {
		t.Fatal("brief leaked into index")
	}
	if !strings.Contains(renderIntentTask(n), brief) {
		t.Fatal("worker did not receive full brief")
	}
}

func TestAddIntentDedupGate(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_INTENT_DEDUP", "1")
	_, ts, _ := waveGraph(t)
	tools := NewToolSet(ts, "planner")
	item := intentItem{Summary: "核对 localhost:9876/status 的固定响应字段"}
	id, e := tools.addOneIntent(item)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tools.addOneIntent(item); e == nil {
		t.Fatal("open duplicate accepted")
	}
	ts.SetNodeState(id, "done")
	if _, e = tools.addOneIntent(item); e == nil {
		t.Fatal("done duplicate accepted")
	}
	ts.SetNodeState(id, "exhausted")
	if _, e = tools.addOneIntent(item); e != nil {
		t.Fatalf("exhausted continuation blocked: %v", e)
	}
	other := item
	other.Summary = strings.Replace(item.Summary, "9876", "9877", 1)
	if _, e = tools.addOneIntent(other); e != nil {
		t.Fatalf("different endpoint blocked: %v", e)
	}
}

func TestResolveHintAndWorkerHintFilter(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_DATA_CONTRACT", "1")
	d, ts, _ := waveGraph(t)
	tools := NewToolSet(ts, "planner")
	assetID, e := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("hint-%d.invalid", ts.ID())})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Assets().DeleteByIDs([]int64{assetID}) })
	relevant, e := ts.AddNode(db.KindHint, map[string]any{"text": strings.Repeat("目标提示", 100)}, 1, "active", "fixture", []int64{assetID})
	if e != nil {
		t.Fatal(e)
	}
	global, e := ts.AddNode(db.KindHint, map[string]any{"text": "全局提示"}, 1, "active", "fixture", nil)
	if e != nil {
		t.Fatal(e)
	}
	in := waveIntent(t, ts, map[string]any{"summary": "fixture", "asset_ids": []int64{assetID}})
	views, _, _, e := tools.hintViews(in, 10)
	if e != nil || len(views) != 2 {
		t.Fatalf("views=%v err=%v", views, e)
	}
	if views[0]["id"] != relevant || len([]rune(views[0]["text"].(string))) > 200 {
		t.Fatal("relevance order or text budget")
	}
	_, other, _ := waveGraph(t)
	foreign, e := other.AddNode(db.KindHint, map[string]any{"text": "foreign"}, 1, "active", "fixture", nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = ts.ResolveHints([]int64{global, foreign}, "invalid batch"); e == nil {
		t.Fatal("cross-task hint accepted")
	}
	node, _ := ts.GetNode(global)
	if node.State == "consumed" {
		t.Fatal("partial resolution")
	}
	raw, _ := json.Marshal(map[string]any{"hint_ids": []int64{global}, "reason": "已处理"})
	res, e := tools.resolveHint().Call(t.Context(), raw, nil)
	if e != nil || res.IsError {
		t.Fatalf("resolve failed: %v %v", res, e)
	}
	views, resolved, _, e := tools.hintViews(in, 10)
	if e != nil || resolved != 1 || len(views) != 1 {
		t.Fatalf("views=%v resolved=%d err=%v", views, resolved, e)
	}
}

func TestRunScriptAtomicSandboxAndReview(t *testing.T) {
	dir := t.TempDir()
	tool := NewRunScriptTool()
	raw := json.RawMessage(`{"path":"nested/check.py","content":"import os\nprint(os.environ['WAVE_FIXTURE'])"}`)
	review, ok := tool.(guard.ReviewDescriber)
	if !ok {
		t.Fatal("review descriptor missing")
	}
	body, external, e := review.ReviewInput(raw)
	if e != nil || !external || !strings.Contains(string(body), "WAVE_FIXTURE") {
		t.Fatalf("review=%s %v %v", body, external, e)
	}
	for _, path := range []string{"../escape.py", filepath.Join(dir, "absolute.py")} {
		bad, _ := json.Marshal(map[string]string{"path": path, "content": "print('bad')"})
		res, e := tool.Call(t.Context(), bad, &actool.ToolContext{WorkingDir: dir})
		if e != nil || !res.IsError {
			t.Fatalf("escaped path: %s", path)
		}
	}
	mgr, e := actool.NewManager("wave-" + transcript.NewSessionID())
	if e != nil {
		t.Fatal(e)
	}
	defer mgr.Cleanup()
	res, e := tool.Call(t.Context(), raw, &actool.ToolContext{WorkingDir: dir, Env: []string{"WAVE_FIXTURE=atomic-ok"}, Tasks: mgr, OutputDir: filepath.Join(dir, "output"), MaxOutputChars: 1000})
	if e != nil || res.IsError || !strings.Contains(res.Flatten(), "atomic-ok") {
		t.Fatalf("run=%s err=%v", res.Flatten(), e)
	}
	if data, e := os.ReadFile(filepath.Join(dir, "nested/check.py")); e != nil || !strings.Contains(string(data), "WAVE_FIXTURE") {
		t.Fatal("script not saved")
	}
	bad := json.RawMessage(`{"path":"blocked.sh","content":"rm -rf /"}`)
	decision := tool.CheckPermissions(t.Context(), bad, permission.Context{})
	if decision.Behavior != permission.Deny {
		t.Fatalf("destructive floor absent: %+v", decision)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	sleep := json.RawMessage(`{"path":"wait.py","content":"import time\ntime.sleep(30)","timeout_ms":1000}`)
	res, e = tool.Call(ctx, sleep, &actool.ToolContext{WorkingDir: dir, Tasks: mgr})
	if e != nil || !res.IsError {
		t.Fatalf("cancel ignored: %+v %v", res, e)
	}
	for _, task := range mgr.List() {
		if task.Status == actool.TaskRunning {
			t.Fatal("canceled process still running")
		}
	}
}

func TestPlannerHardDropsBashWhenFactsExist(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_STICKY_PLANNER", "1")
	t.Setenv("VESTRAIL_OPT_PLANNER_SLIM", "1")
	_, ts, id := waveGraph(t)
	ts.AddNode(db.KindFact, map[string]any{"summary": "fixture fact"}, 1, "confirmed", "fixture", nil)
	prov := &waveProvider{}
	p := NewPlanner(prov, "fixture", t.TempDir(), nil, 200000, 5)
	defer p.Close()
	if _, _, e := p.Plan(t.Context(), id, nil, ts, "fixture", nil, nil); e != nil {
		t.Fatal(e)
	}
	for _, tool := range prov.requests[0].Tools {
		if tool.Name == "Bash" {
			t.Fatal("Bash advertised with facts")
		}
	}
	st := p.epochFor(ts.ID())
	for _, tool := range st.boundTools {
		if tool.Name() == "Bash" {
			t.Fatal("Bash still available via deferred search")
		}
	}
}

func TestWorkerLaneForkPreservesCachePrefixAndRollsOver(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_LANE_WORKER", "1")
	_, ts, id := waveGraph(t)
	prov := &waveProvider{}
	store := transcript.NewStore(t.TempDir())
	w := NewWorker(prov, "fixture", t.TempDir(), store, 200000, 5)
	root := waveIntent(t, ts, map[string]any{"summary": "read fixture", "brief": "only inspect generated data"})
	if _, _, e := w.Execute(t.Context(), "work#1", id, nil, ts, root, nil, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	ts.SetNodeState(root.ID, "done")
	child := waveIntent(t, ts, map[string]any{"summary": "next fixture", "brief": "check a different generated value", "continue_intent_id": root.ID, "lane_root_id": root.ID, "lane_depth": 2})
	if _, _, e := w.Execute(t.Context(), "work#1", id, nil, ts, child, nil, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	a, b := prov.requests[0], prov.requests[1]
	if !reflect.DeepEqual(a.System, b.System) || !reflect.DeepEqual(a.Tools, b.Tools) || !a.PreserveReasoningHistory || !b.PreserveReasoningHistory {
		t.Fatal("source system/tools policy changed")
	}
	if !reflect.DeepEqual(a.Messages, b.Messages[:len(a.Messages)]) {
		t.Fatal("source request prefix changed")
	}
	last := b.Messages[len(b.Messages)-1].Text()
	if !strings.Contains(last, fmt.Sprintf("当前执行意图 #%d", child.ID)) || strings.Contains(last, "全局探索态势") {
		t.Fatalf("unexpected fork input: %s", last)
	}
	cs, e := readWorkerContext(store.MainPath(WorkerSessionID(ts.ID(), child.ID)) + ".lane.json")
	if e != nil || cs.RootID != root.ID || cs.Depth != 2 {
		t.Fatalf("context=%+v err=%v", cs, e)
	}
	recs, e := store.Load(store.MainPath(WorkerSessionID(ts.ID(), child.ID)))
	if e != nil {
		t.Fatal(e)
	}
	_, usage := transcript.Messages(recs)
	if usage.InputTokens != 1200 {
		t.Fatalf("seed usage double counted: %+v", usage)
	}
	// A resumed own transcript must never fork again or duplicate the source history.
	before := len(prov.requests)
	if _, _, e := w.Execute(t.Context(), "work#1", id, nil, ts, child, nil, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	if len(prov.requests) != before+1 || len(prov.requests[before].Messages) <= len(b.Messages) {
		t.Fatal("own history was replaced")
	}
	ts.SetNodeState(child.ID, "done")
	w.model = "changed-model"
	grand := waveIntent(t, ts, map[string]any{"summary": "clean handoff", "continue_intent_id": child.ID, "lane_root_id": root.ID, "lane_depth": 3})
	if _, _, e := w.Execute(t.Context(), "work#1", id, nil, ts, grand, nil, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	lastReq := prov.requests[len(prov.requests)-1]
	if len(lastReq.Messages) != 2 {
		t.Fatalf("rollover kept old messages: %d", len(lastReq.Messages))
	}
	gn, _ := ts.GetNode(grand.ID)
	if IntentLaneRoot(gn) != grand.ID {
		t.Fatal("effective lane did not reset")
	}
}

func TestWorkerWritebackRejectsSourceIntent(t *testing.T) {
	_, ts, id := waveGraph(t)
	source := waveIntent(t, ts, map[string]any{"summary": "source"})
	current := waveIntent(t, ts, map[string]any{"summary": "current"})
	tools := NewToolSet(ts, "work#1")
	tools.SetTaskID(id)
	tools.workerIntentID = current.ID
	if _, e := tools.recordOneFact(factItem{Summary: "fixture", IntentID: json.RawMessage(fmt.Sprint(source.ID))}, 0); e == nil {
		t.Fatal("source fact accepted")
	}
	fid, e := tools.recordOneFact(factItem{Summary: "fixture"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	ids, _ := ts.FactsYielded(current.ID)
	if len(ids) != 1 || ids[0] != fid {
		t.Fatal("default owner missing")
	}
	raw, _ := json.Marshal(map[string]any{"summary": "fixture", "intent_id": source.ID})
	res, e := tools.addFinding().Call(t.Context(), raw, nil)
	if e != nil || !res.IsError {
		t.Fatal("source finding accepted")
	}
}

func TestPlannerStickyEpochDeltaAndDigestRollover(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_STICKY_PLANNER", "1")
	_, ts, id := waveGraph(t)
	prov := &waveProvider{}
	p := NewPlanner(prov, "fixture", t.TempDir(), transcript.NewStore(t.TempDir()), 200000, 5)
	defer p.Close()
	in := waveIntent(t, ts, map[string]any{"summary": "active"})
	hint, _ := ts.AddNode(db.KindHint, map[string]any{"text": "old hint"}, 1, "active", "fixture", nil)
	plan := func(goal string) {
		t.Helper()
		if _, _, e := p.Plan(t.Context(), id, nil, ts, goal, nil, nil); e != nil {
			t.Fatal(e)
		}
	}
	plan("same goal")
	st := p.epochFor(ts.ID())
	session := st.session
	bound := st.tools
	st.tools.GoalMet = true
	st.tools.Reason = "stale"
	ts.SetNodeState(in.ID, "done")
	ts.ResolveHints([]int64{hint}, "processed")
	plan("same goal")
	if st.session != session || st.tools != bound || st.roundsInEpoch != 2 {
		t.Fatal("epoch was recreated")
	}
	if st.tools.GoalMet || st.tools.Reason != "" {
		t.Fatal("stale goal result reused")
	}
	req := prov.requests[1]
	input := req.Messages[len(req.Messages)-1].Text()
	if !strings.Contains(input, "增量态势") || !strings.Contains(input, `"state":"resolved"`) || !strings.Contains(input, `"state":"done"`) {
		t.Fatalf("existing changes lost: %s", input)
	}
	if !req.PreserveReasoningHistory {
		t.Fatal("epoch reasoning policy changed")
	}
	plan("edited goal")
	if st.session == session || st.roundsInEpoch != 1 || st.epoch != 2 {
		t.Fatal("goal edit did not roll")
	}
	for range 7 {
		plan("edited goal")
	}
	if st.roundsInEpoch != 8 {
		t.Fatal("wrong round count")
	}
	plan("edited goal")
	if st.epoch != 3 || st.roundsInEpoch != 1 {
		t.Fatal("8 round guard not applied")
	}
	st.lastInputTok = 48000
	plan("edited goal")
	if st.epoch != 4 {
		t.Fatal("input guard not applied")
	}
}

func TestCaptureSessionPromptsCountUsageOnce(t *testing.T) {
	prov := &waveProvider{}
	s := newAgentSession(agentcore.Options{Provider: prov, Tools: []actool.CoreTool{}, DisableBackgroundTasks: true})
	defer s.Close()
	for range 2 {
		var activities []db.Activity
		if _, _, e := captureRunSession(t.Context(), s, nil, "fixture", func(a db.Activity) { activities = append(activities, a) }); e != nil {
			t.Fatal(e)
		}
		assertCapturedResultUsage(t, activities, 1200, 20, 900, 0)
	}
	if s.Usage().InputTokens != 2400 {
		t.Fatalf("session usage=%+v", s.Usage())
	}
}

func TestIntentLaneDepthHandoff(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_INTENT_DEDUP", "0")
	t.Setenv("VESTRAIL_OPT_LANE_WORKER", "1")
	_, ts, _ := waveGraph(t)
	root := waveIntent(t, ts, map[string]any{"summary": "root"})
	ts.SetNodeState(root.ID, "done")
	tools := NewToolSet(ts, "planner")
	current := root
	for depth := 2; depth <= 5; depth++ {
		in := intentItem{Summary: fmt.Sprintf("unique step %d", depth), ContinueIntentID: json.RawMessage(fmt.Sprint(current.ID))}
		id, e := tools.addOneIntent(in)
		if e != nil {
			t.Fatal(e)
		}
		n, _ := ts.GetNode(id)
		lane := readIntentLane(n)
		if depth < 5 && (lane.Root != root.ID || lane.Depth != depth) {
			t.Fatalf("depth=%d lane=%+v", depth, lane)
		}
		if depth == 5 && (lane.Continue != 0 || lane.Handoff != current.ID || lane.HandoffRoot != root.ID) {
			t.Fatalf("handoff=%+v", lane)
		}
		ts.SetNodeState(id, "done")
		current = n
	}
}

func TestConcurrentIntentDedup(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_INTENT_DEDUP", "1")
	_, ts, _ := waveGraph(t)
	var count atomic.Int64
	done := make(chan struct{}, 8)
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			tools := NewToolSet(ts, "planner")
			if _, e := tools.addOneIntent(intentItem{Summary: "read localhost:19876/fixture stable value"}); e == nil {
				count.Add(1)
			}
		}()
	}
	for range 8 {
		<-done
	}
	if count.Load() != 1 {
		t.Fatalf("duplicates created=%d", count.Load())
	}
}

func TestPlannerEpochLastInputTokNotCumulative(t *testing.T) {
	for _, callsPerRound := range []int{1, 2} {
		t.Run(fmt.Sprintf("calls_per_round_%d", callsPerRound), func(t *testing.T) {
			t.Setenv("VESTRAIL_OPT_STICKY_PLANNER", "1")
			_, graph, taskID := waveGraph(t)
			provider := &waveProvider{run: func(_ llm.CompletionRequest, call int) []llm.StreamEvent {
				events := []llm.StreamEvent{
					{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 20000}},
				}
				if callsPerRound == 2 && call%2 == 1 {
					return append(events,
						llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: fmt.Sprintf("todo-%d", call), ToolName: "TodoWrite"},
						llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{"todos":[]}`},
						llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: llm.Usage{OutputTokens: 10}},
					)
				}
				return append(events,
					llm.StreamEvent{Type: llm.SETextDelta, Text: "fixture complete"},
					llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 10}},
				)
			}}
			planner := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 5)
			defer planner.Close()
			for range 4 {
				if _, _, err := planner.Plan(t.Context(), taskID, nil, graph, "same goal", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			state := planner.epochFor(graph.ID())
			if state.epoch != 1 || state.roundsInEpoch != 4 || state.lastInputTok != 20000 {
				t.Fatalf("epoch=%d rounds=%d last input=%d", state.epoch, state.roundsInEpoch, state.lastInputTok)
			}
			if state.session.LastInputTokens() != 20000 || state.session.Usage().InputTokens != 4*callsPerRound*20000 {
				t.Fatalf("last input=%d cumulative=%d", state.session.LastInputTokens(), state.session.Usage().InputTokens)
			}
		})
	}
}

func TestPlannerDeltaOmitsUnchangedHeavyFields(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_STICKY_PLANNER", "1")
	t.Setenv("VESTRAIL_OPT_DATA_CONTRACT", "1")
	_, graph, taskID := waveGraph(t)
	goalID, err := graph.AddGoal(map[string]any{"text": "verify generated fixture"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.AddGoal(map[string]any{"text": "second fixture remains open"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.AddNode(db.KindHint, map[string]any{"text": "unchanged hint"}, 1, "active", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.AddDigest(map[string]any{"body": strings.Repeat("unchanged digest ", 100)}, nil); err != nil {
		t.Fatal(err)
	}
	evidenceID, err := graph.AddNode(db.KindFact, map[string]any{
		"summary": "generated fixture verified", "evidence": "local synthetic result", "confidence": "observed",
	}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &waveProvider{}
	planner := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 5)
	defer planner.Close()
	plan := func() string {
		t.Helper()
		if _, _, err := planner.Plan(t.Context(), taskID, nil, graph, "same top-level goal", nil, nil); err != nil {
			t.Fatal(err)
		}
		request := provider.requests[len(provider.requests)-1]
		return request.Messages[len(request.Messages)-1].Text()
	}
	first := plan()
	for _, key := range []string{"goals", "hints", "cold_digests"} {
		if !strings.Contains(first, `"`+key+`":`) {
			t.Fatalf("initial overview missing %s: %s", key, first)
		}
	}
	second := plan()
	for _, key := range append([]string{"task", "recent_done_intents"}, plannerHashTrackedKeys...) {
		if strings.Contains(second, `"`+key+`":`) {
			t.Fatalf("unchanged field %s resent: %s", key, second)
		}
	}
	state := planner.epochFor(graph.ID())
	session := state.session
	if _, err := graph.AddNode(db.KindHint, map[string]any{"text": "new fixture hint"}, 1, "active", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(graph, "planner")
	tools.SetTaskID(taskID)
	proof, err := json.Marshal(map[string]any{"goal_id": goalID, "evidence_id": evidenceID, "reason": "synthetic fixture meets this goal"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.proveGoal().Call(t.Context(), proof, nil)
	if err != nil || result.IsError {
		t.Fatalf("prove_goal failed: %s, %v", result.Flatten(), err)
	}
	third := plan()
	if !strings.Contains(third, `"hints":`) || !strings.Contains(third, "new fixture hint") ||
		!strings.Contains(third, `"goals":`) || !strings.Contains(third, `"state":"met"`) {
		t.Fatalf("changed hints/goals missing: %s", third)
	}
	for _, key := range []string{"task", "recent_done_intents", "cold_digests", "related_tasks"} {
		if strings.Contains(third, `"`+key+`":`) {
			t.Fatalf("unrelated field %s resent: %s", key, third)
		}
	}
	if state.epoch != 1 || state.roundsInEpoch != 3 || state.session != session {
		t.Fatalf("subgoal proof destroyed session: epoch=%d rounds=%d", state.epoch, state.roundsInEpoch)
	}
	if len(plan()) >= len(first) {
		t.Fatal("unchanged delta was not smaller than full overview")
	}
}

func TestAddIntentBatchSchemaExposesBriefAndContinue(t *testing.T) {
	schema := NewToolSet(nil, "").addIntent().InputSchema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing top-level properties: %v", schema)
	}
	intents := properties["intents"].(map[string]any)
	items := intents["items"].(map[string]any)
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing batch item properties: %v", items)
	}
	for _, key := range []string{"summary", "brief", "continue_intent_id", "asset_ids", "parent_ids", "priority"} {
		if _, exists := itemProperties[key]; !exists {
			t.Fatalf("batch item missing %s", key)
		}
		if !reflect.DeepEqual(itemProperties[key], properties[key]) {
			t.Fatalf("batch and single schema differ for %s", key)
		}
	}
	required, ok := items["required"].([]string)
	if !ok || !reflect.DeepEqual(required, []string{"summary"}) {
		t.Fatalf("batch summary not required: %v", items["required"])
	}
}

type overviewQueryCounter struct {
	overviews   atomic.Int64
	legacyHints atomic.Int64
}

func (counter *overviewQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "GROUP BY kind") {
		counter.overviews.Add(1)
	}
	if strings.Contains(data.SQL, "ORDER BY id DESC LIMIT $3") {
		// pgx may prepend execution options to the user arguments.
		for _, argument := range data.Args {
			if kind, ok := argument.(string); ok && kind == db.KindHint {
				counter.legacyHints.Add(1)
				break
			}
		}
	}
	return ctx
}

func (*overviewQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestWorkerLaneForkSkipsOverviewQuery(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_LANE_WORKER", "1")
	t.Setenv("VESTRAIL_OPT_DATA_CONTRACT", "1")
	_, baseGraph, taskID := waveGraph(t)
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	counter := &overviewQueryCounter{}
	config.Tracer = counter
	connection := stdlib.OpenDB(*config)
	defer connection.Close()
	tracedDB := &db.DB{DB: connection}
	graph := tracedDB.Exploration(baseGraph.ID())
	provider := &waveProvider{}
	worker := NewWorker(provider, "fixture", t.TempDir(), transcript.NewStore(t.TempDir()), 200000, 5)
	root := waveIntent(t, graph, map[string]any{"summary": "generated source fixture"})
	if _, _, err := worker.Execute(t.Context(), "work#1", taskID, nil, graph, root, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if counter.overviews.Load() != 1 || counter.legacyHints.Load() != 0 {
		t.Fatalf("root overview=%d legacy hints=%d", counter.overviews.Load(), counter.legacyHints.Load())
	}
	if err := graph.SetNodeState(root.ID, "done"); err != nil {
		t.Fatal(err)
	}
	child := waveIntent(t, graph, map[string]any{
		"summary": "generated child fixture", "continue_intent_id": root.ID, "lane_root_id": root.ID, "lane_depth": 2,
	})
	counter.overviews.Store(0)
	if _, _, err := worker.Execute(t.Context(), "work#1", taskID, nil, graph, child, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	state, err := readWorkerContext(worker.tx.MainPath(WorkerSessionID(graph.ID(), child.ID)) + ".lane.json")
	if err != nil || state.RootID != root.ID {
		t.Fatalf("fork not established: %v, %v", state, err)
	}
	request := provider.requests[len(provider.requests)-1]
	if counter.overviews.Load() != 0 || strings.Contains(request.Messages[len(request.Messages)-1].Text(), "全局探索态势") {
		t.Fatalf("fork redundantly queried/injected overview: queries=%d", counter.overviews.Load())
	}
	tools := NewToolSet(graph, "planner")
	if problem, exists := tools.graphOverviewData()["error"]; exists {
		t.Fatal(problem)
	}
	if counter.legacyHints.Load() != 0 {
		t.Fatal("optimized planner still queried legacy hints")
	}
	t.Setenv("VESTRAIL_OPT_DATA_CONTRACT", "0")
	tools.graphOverviewData()
	tools.workerOverviewData(child)
	if counter.legacyHints.Load() != 2 {
		t.Fatalf("legacy hint paths not preserved: %d", counter.legacyHints.Load())
	}
}

func TestPlannerDeltaClearsRemovedHeavyFields(t *testing.T) {
	_, graph, _ := waveGraph(t)
	tools := NewToolSet(graph, "planner")
	previous := map[string]any{
		"coverage": map[string]any{"completed": 1, "total": 2},
		"hints":    []string{"old hint"},
	}
	reordered := map[string]any{
		"coverage": map[string]any{"total": 2, "completed": 1},
		"hints":    []string{"old hint"},
	}
	if !reflect.DeepEqual(snapshotFieldHashes(previous), snapshotFieldHashes(reordered)) {
		t.Fatal("map insertion order changed field hashes")
	}
	current := map[string]any{"frontier_open": int64(0)}
	delta, err := tools.plannerDeltaData(nil, nil, snapshotFieldHashes(previous), snapshotFieldHashes(current), current)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"coverage", "hints"} {
		if value, exists := delta[key]; !exists || value != nil {
			t.Fatalf("removed field %s was not explicitly cleared: %v", key, delta)
		}
	}
	for _, key := range []string{"coverage", "hints"} {
		nullValue := map[string]any{key: nil}
		delta, err := tools.plannerDeltaData(nil, nil, snapshotFieldHashes(nullValue), snapshotFieldHashes(current), current)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := delta[key]; !exists {
			t.Fatalf("removed null field %s was not cleared", key)
		}
	}
}
