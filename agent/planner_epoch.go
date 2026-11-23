package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type plannerEpochState struct {
	mu                                 sync.Mutex
	epoch, roundsInEpoch, lastInputTok int
	systemDigest                       string
	versions                           map[int64]int
	fieldHashes                        map[string]string
	session                            *agentcore.Session
	tools                              *ToolSet
	cleanup                            func()
	boundTools                         []actool.CoreTool
}

func (p *Planner) epochFor(id int64) *plannerEpochState {
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	if p.epochs == nil {
		p.epochs = map[int64]*plannerEpochState{}
	}
	if p.bootTag == "" {
		p.bootTag = transcript.NewSessionID()
	}
	st := p.epochs[id]
	if st == nil {
		st = &plannerEpochState{}
		p.epochs[id] = st
	}
	return st
}

func (st *plannerEpochState) close() {
	if st.session != nil {
		st.session.Close()
		st.session = nil
	}
	if st.cleanup != nil {
		st.cleanup()
		st.cleanup = nil
	}
	st.tools = nil
	st.versions = nil
	st.fieldHashes = nil
	st.roundsInEpoch = 0
	st.lastInputTok = 0
}

// Close releases idle epoch resources after the task loop exits.
func (p *Planner) Close() {
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	for _, st := range p.epochs {
		st.mu.Lock()
		st.close()
		st.mu.Unlock()
	}
	p.epochs = nil
}

func plannerOptionsDigest(opts agentcore.Options, def DeferredInfo, model string) string {
	names := append([]string(nil), def.Deferred...)
	sort.Strings(names)
	schemas := make([]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		schemas = append(schemas, []any{t.Name(), t.Description(), t.Prompt(), t.InputSchema()})
	}
	// Keep the actual tool order as well as full schemas: the wire prefix must match.
	body, _ := json.Marshal([]any{opts.SystemPrompt, opts.DynamicBoundary, schemas, names, def.ContextRevision, model, opts.EnableWebFetch, opts.EnableWebSearch, opts.WebFetchProxy, opts.WebFetchCACert, opts.WebSearchBackend, opts.BraveSearchAPIKey, opts.TavilySearchAPIKey, opts.DeepSeekSearchBaseURL, opts.DeepSeekSearchAPIKey, opts.DeepSeekSearchModel, opts.WebSearchProxy, opts.Compaction, opts.NonStreaming, opts.MaxTokens})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (st *plannerEpochState) rollover(digest string, final bool) bool {
	return st.session == nil || st.roundsInEpoch >= 8 || st.lastInputTok >= 48000 || st.systemDigest != digest || final || len(st.session.Messages()) == 0
}

var plannerHashTrackedKeys = []string{
	"goals", "hints", "cold_digests", "cold_digests_more",
	"coverage", "asset_counts", "check_coverage", "related_tasks",
}

func snapshotFieldHashes(overview map[string]any) map[string]string {
	hashes := make(map[string]string, len(plannerHashTrackedKeys))
	for _, key := range plannerHashTrackedKeys {
		value, exists := overview[key]
		if !exists {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			continue
		}
		digest := sha256.Sum256(data)
		hashes[key] = hex.EncodeToString(digest[:])
	}
	return hashes
}

func (t *ToolSet) plannerDeltaData(old, current map[int64]int, previousHashes, currentHashes map[string]string, overview map[string]any) (map[string]any, error) {
	changed := []int64{}
	removed := []int64{}
	for id, v := range current {
		if prev, ok := old[id]; !ok || prev != v {
			changed = append(changed, id)
		}
	}
	for id := range old {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	out := map[string]any{"removed_node_ids": removed, "changed_total": len(changed)}
	// Active queues and counts stay current on every wake-up.
	for _, key := range []string{"frontier_open", "open_intents", "running_intents", "facts", "findings_total", "hints_resolved_total"} {
		if value, ok := overview[key]; ok {
			if key == "open_intents" || key == "running_intents" {
				if items, ok := value.([]map[string]any); ok {
					slim := make([]map[string]any, 0, len(items))
					for _, item := range items {
						copy := map[string]any{}
						for k, v := range item {
							copy[k] = v
						}
						copy["summary"] = truncRuneSummary(copy["summary"], 120)
						slim = append(slim, copy)
					}
					value = slim
				}
			}
			out[key] = value
		}
	}
	// Send heavy snapshots only when their content changes, including removal.
	for _, key := range plannerHashTrackedKeys {
		if currentHashes[key] == previousHashes[key] {
			continue
		}
		if value, exists := overview[key]; exists {
			out[key] = value
		} else {
			out[key] = nil
		}
	}
	selected := changed
	if len(selected) > 45 {
		out["omitted_changed_node_ids"] = selected[45:]
		selected = selected[:45]
	}
	nodes, err := t.ts.NodesByIDs(selected)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, n := range nodes {
		var payload map[string]any
		_ = json.Unmarshal(n.Payload, &payload)
		if n.Kind == db.KindFact {
			item := kernel.ProjectFactCompact(n.ID, n.State, payload, 200)
			item["kind"] = n.Kind
			items = append(items, item)
			continue
		}
		summary := payload["summary"]
		if summary == nil {
			summary = payload["text"]
		}
		item := map[string]any{"id": n.ID, "kind": n.Kind, "state": n.State, "summary": truncRuneSummary(summary, 200)}
		if n.Kind == db.KindHint && n.State == "consumed" {
			item["state"] = "resolved"
		}
		if n.Kind == db.KindIntent {
			item["summary"] = truncRuneSummary(summary, 120)
		}
		items = append(items, item)
	}
	out["changed_nodes"] = items
	return out, nil
}

func (p *Planner) runEpoch(ctx context.Context, st *plannerEpochState, ts *db.ExplorationStore, tsx *ToolSet, opts agentcore.Options, def DeferredInfo, cleanup func(), digest string, inputLead, inputEnd string, overview map[string]any, triggers []TriggerEvent, emit func(db.Activity)) (bool, string, error, bool) {
	versions, err := ts.ContentVersions()
	if err != nil {
		return false, "", err, false
	}
	currentHashes := snapshotFieldHashes(overview)
	fresh := st.rollover(digest, taskClockFrom(ctx).Final)
	if fresh {
		st.close()
		st.epoch++
		opts.SessionID = fmt.Sprintf("exp%d-planner-%s-ep%d", ts.ID(), p.bootTag, st.epoch)
		st.session = newAgentSession(opts)
		st.boundTools = opts.Tools
		st.tools = tsx
		st.cleanup = cleanup
		st.systemDigest = digest
	} else {
		st.session.UpdateRunOptions(opts)
	}
	st.tools.GoalMet, st.tools.Reason = false, ""
	situation := renderGraphOverview(overview)
	if !fresh {
		delta, e := tsx.plannerDeltaData(st.versions, versions, st.fieldHashes, currentHashes, overview)
		if e != nil {
			st.close()
			return false, "", e, false
		}
		data, _ := json.Marshal(delta)
		situation = "\n\n【本轮增量态势（包括既有节点变更）】：\n" + string(data)
	}
	input := inputLead + renderTriggers(ts, triggers) + situation + inputEnd
	_, _, err = captureRunSession(ctx, st.session, opts.Transcript, input, func(a db.Activity) {
		if emit != nil {
			a.Worker = "planner"
			emit(a)
		}
	})
	if tokens := st.session.LastInputTokens(); tokens > 0 {
		st.lastInputTok = tokens
	}
	met, reason := st.tools.GoalMet, st.tools.Reason
	st.roundsInEpoch++
	st.versions = versions
	st.fieldHashes = currentHashes
	retained := fresh
	if err != nil || ctx.Err() != nil || taskClockFrom(ctx).Final {
		st.close()
	}
	return met, reason, err, retained
}

func (st *plannerEpochState) sessionTools() []actool.CoreTool { return st.boundTools }

func (p *Planner) CloseEpoch(expID int64) {
	p.epochMu.Lock()
	st := p.epochs[expID]
	p.epochMu.Unlock()
	if st != nil {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.close()
	}
}

func providerCacheIdentity(prov any) string {
	if p, ok := prov.(interface{ CacheIdentity() string }); ok {
		return p.CacheIdentity()
	}
	return fmt.Sprintf("%T:%p", prov, prov)
}
