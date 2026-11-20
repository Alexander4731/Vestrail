package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/Alexander4731/Vestrail/db"
)

var intentCreationLocks sync.Map
var intentTargetRE = regexp.MustCompile(`https?://[a-zA-Z0-9._~:/?&=%#@+\-]+|[a-zA-Z0-9_.-]+:[0-9]{1,5}(?:/[a-zA-Z0-9._~/?&=%#@+\-]*)?|/[a-zA-Z0-9_./%?&=+\-]+`)

func intentCreationLock(id int64) *sync.Mutex {
	lock, _ := intentCreationLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func normalizedIntentText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
}

func intentGrams(text string) map[string]bool {
	runes := []rune(normalizedIntentText(text))
	out := map[string]bool{}
	if len(runes) == 1 {
		out[string(runes)] = true
	}
	for i := 1; i < len(runes); i++ {
		out[string(runes[i-1:i+1])] = true
	}
	return out
}

func intentSimilarity(a, b string) float64 {
	x, y := intentGrams(a), intentGrams(b)
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

func intentTargets(text string) map[string]bool {
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

func intentsOverlap(in intentItem, existing *db.Node) (bool, float64) {
	var old intentItem
	if json.Unmarshal(existing.Payload, &old) != nil {
		return false, 1
	}
	// Different explicit execution briefs and endpoints represent different work,
	// even when their short index titles happen to be similar.
	if in.Brief != "" && old.Brief != "" && strings.TrimSpace(in.Brief) != strings.TrimSpace(old.Brief) {
		return false, 1
	}
	x, y := intentTargets(in.Summary+" "+in.Brief), intentTargets(old.Summary+" "+old.Brief)
	match := false
	for target := range x {
		if y[target] {
			match = true
		}
	}
	if len(x) > 0 && len(y) > 0 && !match {
		return false, 1
	}
	for _, a := range pidList(in.AssetIDs) {
		for _, b := range pidList(old.AssetIDs) {
			if a == b {
				match = true
			}
		}
	}
	if match {
		return true, 0.65
	}
	if len(x) == 0 && len(y) == 0 && len(in.AssetIDs) == 0 && len(old.AssetIDs) == 0 {
		return true, 0.80
	}
	return false, 1
}

func (t *ToolSet) checkIntentDuplicate(in intentItem) error {
	open, err := t.ts.Frontier(100)
	if err != nil {
		return err
	}
	running, err := t.ts.IntentsInState("running")
	if err != nil {
		return err
	}
	for _, existing := range append(open, running...) {
		overlap, threshold := intentsOverlap(in, existing)
		var old intentItem
		_ = json.Unmarshal(existing.Payload, &old)
		if overlap && intentSimilarity(in.Summary, old.Summary) >= threshold {
			return fmt.Errorf("重复意图 #%d 已为 %s；复用其结果或说明不同目标/机理", existing.ID, existing.State)
		}
	}
	if len(pidList(in.ParentIDs)) > 0 {
		return nil
	}
	recent, err := t.ts.ListByKind(db.KindIntent, 300)
	if err != nil {
		return err
	}
	checked := 0
	for _, existing := range recent {
		if existing.State != "done" {
			continue
		}
		checked++
		overlap, threshold := intentsOverlap(in, existing)
		var old intentItem
		_ = json.Unmarshal(existing.Payload, &old)
		if overlap && intentSimilarity(in.Summary, old.Summary) >= max(0.72, threshold) {
			return fmt.Errorf("意图 #%d 已完成；新机理须引用新事实 parent_ids 并说明差异", existing.ID)
		}
		if checked >= 30 {
			break
		}
	}
	return nil
}
