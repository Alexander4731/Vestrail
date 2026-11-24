package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxInspectedScripts     = 3
	maxInspectedScriptBytes = 32 * 1024
)

type inspectedScript struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// reScriptInvocation extracts script file references executed via common
// interpreters in Bash/PowerShell commands (e.g. `python p_secondary.py`,
// `node ./scan.cjs`, `pwsh -File run.ps1`, `bash test.sh`).
var reScriptInvocation = regexp.MustCompile(`(?i)(?:^|[;&|\s(])(?:python3?|py|node|bun|deno|pwsh|powershell|bash|sh|ruby|perl)(?:\.exe)?(?:\s+-[a-z0-9-]+)*\s+['"]?([./\\a-zA-Z0-9_:-]+\.(?:py|js|cjs|mjs|ts|ps1|sh|rb|pl|bat|cmd))['"]?(?:$|[;&|\s)])`)

// reCdDir extracts an explicit `cd <dir>` prefix inside a shell command so
// commands like `cd D:\...\i1292; python p_secondary.py` resolve relative to
// the target directory as well as the agent's working directory.
var reCdDir = regexp.MustCompile(`(?i)(?:^|[;&|]\s*)cd\s+(?:/d\s+)?(?:"([^"]+)"|'([^']+)'|([^\s;&|]+))`)

// enrichBashDescriptionWithScripts inspects a Bash tool review payload and,
// when the command runs local script files inside the agent's working tree,
// attaches their contents under `referenced_scripts` so both rule matching and
// the LLM fallback judge see the actual code about to execute.
// The enriched payload is used ONLY for authorization/audit, never for execution.
func enrichBashDescriptionWithScripts(workingDir string, description json.RawMessage) json.RawMessage {
	if workingDir == "" || len(description) == 0 {
		return description
	}
	var payload map[string]any
	if err := json.Unmarshal(description, &payload); err != nil {
		return description
	}
	cmd, _ := payload["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return description
	}
	scripts := collectReferencedScripts(workingDir, cmd)
	if len(scripts) == 0 {
		return description
	}
	payload["referenced_scripts"] = scripts
	enriched, err := json.Marshal(payload)
	if err != nil {
		return description
	}
	return enriched
}

func collectReferencedScripts(workingDir, cmd string) []inspectedScript {
	matches := reScriptInvocation.FindAllStringSubmatch(cmd, -1)
	if len(matches) == 0 {
		return nil
	}
	absWorkDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil
	}
	baseDirs := []string{absWorkDir}
	if cdm := reCdDir.FindStringSubmatch(cmd); len(cdm) > 0 {
		dir := firstNonEmpty(cdm[1], cdm[2], cdm[3])
		if dir != "" {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(absWorkDir, dir)
			}
			if absCD, err := filepath.Abs(dir); err == nil && isWithinDir(absWorkDir, absCD) {
				baseDirs = append([]string{absCD}, baseDirs...)
			}
		}
	}
	seen := map[string]bool{}
	var out []inspectedScript
	for _, m := range matches {
		if len(out) >= maxInspectedScripts {
			break
		}
		candidate := strings.TrimSpace(m[1])
		if candidate == "" {
			continue
		}
		resolved := resolveScriptInRoots(absWorkDir, baseDirs, candidate)
		if resolved == "" || seen[resolved] {
			continue
		}
		seen[resolved] = true
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		raw, err := os.ReadFile(resolved)
		if err != nil {
			continue
		}
		truncated := false
		if len(raw) > maxInspectedScriptBytes {
			raw = raw[:maxInspectedScriptBytes]
			truncated = true
		}
		rel, err := filepath.Rel(absWorkDir, resolved)
		if err != nil {
			rel = candidate
		}
		out = append(out, inspectedScript{
			Path:      filepath.ToSlash(rel),
			Content:   string(raw),
			Truncated: truncated,
		})
	}
	return out
}

func resolveScriptInRoots(boundary string, bases []string, candidate string) string {
	if filepath.IsAbs(candidate) {
		abs, err := filepath.Abs(candidate)
		if err == nil && isWithinDir(boundary, abs) {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
		return ""
	}
	for _, base := range bases {
		joined := filepath.Join(base, candidate)
		abs, err := filepath.Abs(joined)
		if err != nil || !isWithinDir(boundary, abs) {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return ""
}

func isWithinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
