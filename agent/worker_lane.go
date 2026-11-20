package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/intercept"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

type workerContext struct {
	TaskID      int64    `json:"task_id"`
	IntentID    int64    `json:"intent_id"`
	RootID      int64    `json:"root_id"`
	Depth       int      `json:"depth"`
	Directory   string   `json:"directory"`
	Fingerprint string   `json:"fingerprint"`
	System      []string `json:"system"`
	Boundary    int      `json:"boundary"`
	Unlocked    []string `json:"unlocked,omitempty"`
}

type workerLaneRun struct {
	Context *workerContext
	Source  string
	Own     bool
	Note    string
}

func toolFingerprint(static, model string, tools []actool.CoreTool, def DeferredInfo) string {
	items := make([]any, 0, len(tools))
	for _, tool := range tools {
		items = append(items, map[string]any{"name": tool.Name(), "description": tool.Description(), "prompt": tool.Prompt(), "schema": tool.InputSchema()})
	}
	data, _ := json.Marshal([]any{static, model, items, def.Deferred, def.GlobalNames, def.FindingGuidance, def.ContextRevision})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func contextDirectory(workDir string, taskID int64, directory string) bool {
	root, err := filepath.Abs(ensureRunDir(workDir, taskID, 0))
	if err != nil {
		return false
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	dir, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func readWorkerContext(path string) (*workerContext, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state workerContext
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.RootID <= 0 || state.Depth < 1 || state.Depth > 4 || len(state.System) == 0 || state.Boundary < 0 || state.Boundary > len(state.System) || state.Fingerprint == "" {
		return nil, fmt.Errorf("invalid lane metadata")
	}
	return &state, nil
}

func saveWorkerContext(path string, state *workerContext) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".lane-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(state); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (w *Worker) prepareWorkerLane(ctx context.Context, taskID int64, ts *db.ExplorationStore, intent *db.Node, message string) workerLaneRun {
	var run workerLaneRun
	if w.tx == nil || taskID <= 0 {
		return run
	}
	ownID := WorkerSessionID(ts.ID(), intent.ID)
	if _, err := os.Stat(w.tx.MainPath(ownID)); err == nil {
		run.Own = true
		state, err := readWorkerContext(w.tx.MainPath(ownID) + ".lane.json")
		if err == nil && state.TaskID == taskID && state.IntentID == intent.ID && contextDirectory(w.workDir, taskID, state.Directory) {
			run.Context = state
		}
		return run
	}
	lane := readIntentLane(intent)
	if !OptimizationEnabled("VESTRAIL_OPT_LANE_WORKER") || message != "" || lane.Continue <= 0 {
		return run
	}
	run.Note = "接力未建立，将使用独立工作会话。"
	if w.noaEnabledFn != nil && w.noaEnabledFn() {
		run.Note += "NOA 外部状态不参与接力。"
		return run
	}
	if intercept.RecoverySummary(ctx) != "" {
		run.Note += "当前存在执行恢复交底。"
		return run
	}
	if lane.Depth > 4 {
		return run
	}
	upstream, err := ts.GetNode(lane.Continue)
	if err != nil || upstream == nil || (upstream.State != "done" && upstream.State != "exhausted") {
		return run
	}
	root, err := ts.GetNode(lane.Root)
	if err != nil || root == nil || root.Kind != db.KindIntent {
		return run
	}
	source := WorkerSessionID(ts.ID(), lane.Continue)
	info, err := agentcore.InspectTranscript(w.tx, source)
	if err != nil || !info.Exists || info.MessageCount == 0 || info.LastInputTokens > 55000 || info.HasCompactBound || !info.HasWorkingSnapshot {
		return run
	}
	state, err := readWorkerContext(w.tx.MainPath(source) + ".lane.json")
	if err != nil || state.TaskID != taskID || state.IntentID != lane.Continue || state.RootID != lane.Root || !contextDirectory(w.workDir, taskID, state.Directory) {
		return run
	}
	run.Context = state
	run.Source = source
	run.Note = ""
	return run
}

func laneSystemTail(intent *db.Node, runDir string) string {
	var p map[string]any
	_ = json.Unmarshal(intent.Payload, &p)
	return fmt.Sprintf("执行线起点 #%d（只作背景）：%v\n工作目录：%s\n当前唯一任务以最新【当前执行意图】交底为准；同线后续交底可替代起点任务，写回必须使用该交底的 intent_id。", intent.ID, truncRuneSummary(p["summary"], 200), runDir)
}

func currentIntentInput(intent *db.Node, overview string) string {
	return fmt.Sprintf("【当前执行意图 #%d】\n%s\n%s", intent.ID, renderIntentTask(intent), overview)
}

// Canonical order prevents host maps and MCP discovery order from breaking the prefix.
func stableSessionTools(tools []actool.CoreTool, def DeferredInfo) ([]actool.CoreTool, DeferredInfo) {
	tools = append([]actool.CoreTool(nil), tools...)
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	def.Deferred = append([]string(nil), def.Deferred...)
	sort.Strings(def.Deferred)
	def.GlobalNames = append([]string(nil), def.GlobalNames...)
	sort.Strings(def.GlobalNames)
	return tools, def
}
