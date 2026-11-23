package kernel

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// ArtifactRef identifies an upstream artifact together with its evidence provenance (F07/F08).
type ArtifactRef struct {
	Path       string `json:"path"`
	Provenance string `json:"provenance"` // target_response | source_code | derived_diff | guess_wordlist | history_log
	Valid      bool   `json:"valid"`
	Note       string `json:"note,omitempty"`
}

// ResourceLock expresses reader-writer coordination across intents (F22).
type ResourceLock struct {
	Resource string `json:"resource"`
	Mode     string `json:"mode"` // "shared" | "exclusive"
}

// IntentContract holds optional structured execution contract fields persisted in
// exploration_nodes.payload alongside legacy summary/brief fields.
type IntentContract struct {
	Hypothesis     string         `json:"hypothesis,omitempty"`
	Preconditions  []string       `json:"preconditions,omitempty"`
	Scope          string         `json:"scope,omitempty"`
	Inputs         []ArtifactRef  `json:"inputs,omitempty"`
	Deliverable    string         `json:"deliverable,omitempty"`
	StopConditions []string       `json:"stop_conditions,omitempty"`
	MaxToolCalls   int            `json:"max_tool_calls,omitempty"`
	AfterIntents   []int64        `json:"after_intents,omitempty"`
	ResourceLocks  []ResourceLock `json:"resource_locks,omitempty"`
}

const (
	MaxIntentSummaryIndexRunes = 160
	MaxIntentSummaryInputRunes = 240
)

// SplitSummaryAndBrief enforces separation between the compact index summary and
// detailed execution brief when callers stuff long handoffs into summary (F07).
func SplitSummaryAndBrief(summary, brief string) (indexSummary, fullBrief string) {
	summary = strings.TrimSpace(summary)
	brief = strings.TrimSpace(brief)
	runes := []rune(summary)
	if len(runes) <= MaxIntentSummaryInputRunes {
		return summary, brief
	}
	indexSummary = string(runes[:MaxIntentSummaryIndexRunes]) + "…"
	if brief == "" {
		fullBrief = summary
	} else {
		fullBrief = summary + "\n\n" + brief
	}
	return indexSummary, fullBrief
}

var intentTargetRE = regexp.MustCompile(`https?://[a-zA-Z0-9._~:/?&=%#@+\-]+|[a-zA-Z0-9_.-]+:[0-9]{1,5}(?:/[a-zA-Z0-9._~/?&=%#@+\-]*)?|/[a-zA-Z0-9_./%?&=+\-]+`)

// NormalizedIntentText strips whitespace and punctuation and folds case so
// cosmetic edits cannot bypass deduplication (F15).
func NormalizedIntentText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
}

func IntentGrams(text string) map[string]bool {
	runes := []rune(NormalizedIntentText(text))
	out := map[string]bool{}
	if len(runes) == 1 {
		out[string(runes)] = true
	}
	for i := 1; i < len(runes); i++ {
		out[string(runes[i-1:i+1])] = true
	}
	return out
}

func IntentSimilarity(a, b string) float64 {
	x, y := IntentGrams(a), IntentGrams(b)
	intersection := 0
	for gram := range x {
		if y[gram] {
			intersection++
		}
	}
	union := len(x) + len(y) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func IntentTargets(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range intentTargetRE.FindAllString(text, -1) {
		if u, err := url.Parse(token); err == nil && u.Host != "" {
			u.Host = strings.ToLower(u.Host)
			if u.Path == "" {
				u.Path = "/"
			}
			token = u.String()
		}
		out[token] = true
	}
	return out
}

// DedupCandidate captures the canonical fields compared during intent deduplication.
type DedupCandidate struct {
	ID       int64
	Summary  string
	Brief    string
	Scope    string
	AssetIDs []int64
}

// CheckIntentOverlap compares two intents without letting trivial brief punctuation
// changes short-circuit deduplication (F15).
func CheckIntentOverlap(in, existing DedupCandidate) (overlap bool, threshold float64) {
	// Distinct explicit non-empty scopes represent different slices of work.
	inScope := NormalizedIntentText(in.Scope)
	oldScope := NormalizedIntentText(existing.Scope)
	if inScope != "" && oldScope != "" && inScope != oldScope {
		return false, 1
	}

	x := IntentTargets(in.Summary + " " + in.Brief)
	y := IntentTargets(existing.Summary + " " + existing.Brief)
	targetMatch := false
	for target := range x {
		if y[target] {
			targetMatch = true
			break
		}
	}
	if len(x) > 0 && len(y) > 0 && !targetMatch {
		return false, 1
	}

	assetMatch := false
	for _, a := range in.AssetIDs {
		for _, b := range existing.AssetIDs {
			if a == b {
				assetMatch = true
				break
			}
		}
		if assetMatch {
			break
		}
	}

	// Only treat differing briefs as distinct work when their normalized text
	// genuinely diverges (similarity < 0.85) — never on punctuation/whitespace tweaks.
	inBrief := NormalizedIntentText(in.Brief)
	oldBrief := NormalizedIntentText(existing.Brief)
	if inBrief != "" && oldBrief != "" && inBrief != oldBrief {
		if IntentSimilarity(in.Brief, existing.Brief) < 0.85 {
			return false, 1
		}
	}

	if targetMatch || assetMatch {
		return true, 0.65
	}
	if len(x) == 0 && len(y) == 0 && len(in.AssetIDs) == 0 && len(existing.AssetIDs) == 0 {
		return true, 0.80
	}
	return false, 1
}

// HasFreshParentEvidence reports whether newParents includes at least one node ID
// strictly newer than the existing completed intent (and not already in its inputs),
// proving new evidence arrived after existing finished (F15).
func HasFreshParentEvidence(newParents []int64, existingIntentID int64, existingParents map[int64]bool) bool {
	for _, p := range newParents {
		if p > existingIntentID && !existingParents[p] {
			return true
		}
	}
	return false
}
