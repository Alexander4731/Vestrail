package kernel

import "strings"

// DeliveryStatus distinguishes research completion from partial checkpoints,
// precondition blocks, and incomplete handbacks.
type DeliveryStatus string

const (
	DeliveryComplete            DeliveryStatus = "complete"
	DeliveryPartialCheckpoint   DeliveryStatus = "partial_checkpoint"
	DeliveryPreconditionBlocked DeliveryStatus = "precondition_blocked"
	DeliveryHandedBack          DeliveryStatus = "handed_back"
)

// Checkpoint captures exact reusable continuation state for the next intent in a lane.
type Checkpoint struct {
	CompletedScope   string        `json:"completed_scope,omitempty"`
	RemainingScope   string        `json:"remaining_scope,omitempty"`
	NextAction       string        `json:"next_action,omitempty"`
	ValidArtifacts   []ArtifactRef `json:"valid_artifacts,omitempty"`
	InvalidArtifacts []string      `json:"invalid_artifacts,omitempty"`
}

// RunReceipt is the authoritative persisted summary of one intent execution.
type RunReceipt struct {
	TaskID                int64           `json:"task_id"`
	ExplorationID         int64           `json:"exploration_id"`
	IntentID              int64           `json:"intent_id"`
	TerminationCode       TerminationCode `json:"termination_code"`
	DeliveryStatus        DeliveryStatus  `json:"delivery_status"`
	SettledState          string          `json:"settled_state"`
	ToolCalls             int             `json:"tool_calls"`
	FactsWritten          int             `json:"facts_written"`
	AssetsWritten         int             `json:"assets_written"`
	FindingsWritten       int             `json:"findings_written"`
	ValidBatches          []EvidenceBatch `json:"valid_batches,omitempty"`
	Checkpoint            *Checkpoint     `json:"checkpoint,omitempty"`
	Summary               string          `json:"summary,omitempty"`
	ResearchGain          *ResearchGain   `json:"research_gain,omitempty"`
	InputEvidenceVersions map[int64]int   `json:"input_evidence_versions,omitempty"`
}

type ResearchGain struct {
	Evidence  int   `json:"evidence"`
	Coverage  int64 `json:"coverage"`
	Artifacts int   `json:"artifacts"`
}

// HasNetGain reports whether the run produced verified research value regardless
// of whether it settled as done or exhausted (F20).
func (r RunReceipt) HasNetGain() bool {
	if r.ResearchGain != nil {
		return r.ResearchGain.Evidence > 0 || r.ResearchGain.Coverage > 0 || r.ResearchGain.Artifacts > 0
	}
	if r.FindingsWritten > 0 || r.FactsWritten > 0 {
		return true
	}
	for _, b := range r.ValidBatches {
		if b.Valid && b.PreconditionOK && b.UniqueCovered > 0 {
			return true
		}
	}
	return false
}

var incompleteHandbackMarkers = []string{
	"hand this back",
	"hand it back",
	"stop here and hand",
	"have to stop here",
	"must stop here",
	"cannot continue",
	"unable to continue",
	"交回规划者",
	"先交回",
	"暂停在此",
	"未能执行原定",
	"尚未执行验证",
	"未完成验证",
}

// LooksLikeIncompleteHandback detects explicit worker prose saying it stopped
// before completing the assigned verification (F25). Only used when no facts,
// findings, or assets were written back and no explicit delivery_status was set.
func LooksLikeIncompleteHandback(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" {
		return false
	}
	for _, marker := range incompleteHandbackMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
