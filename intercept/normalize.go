package intercept

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// nonExecutableDocExts are pure text/documentation/data file extensions where
// Write/Edit/MultiEdit writes cannot directly execute commands or send HTTP
// requests. Skipping destructive command/endpoint rules on these files prevents
// false positives when writing vulnerability reports or notes mentioning "rm -rf"
// or "/delete".
var nonExecutableDocExts = map[string]bool{
	".md":       true,
	".markdown": true,
	".txt":      true,
	".log":      true,
	".csv":      true,
	".tsv":      true,
	".rst":      true,
}

// reSafetyFilterLine matches lines in a script that declare a defensive regex
// denylist (e.g. `const risky = /delete|reboot|restore/i;`) so Write/Edit does
// not false-positive on the agent's own safety filter and accidentally teach
// the agent to strip safety filters (observed in task #3 activity #105086).
var reSafetyFilterLine = regexp.MustCompile(`(?i)^\s*(?:const|let|var)?\s*(?:risky|deny|denied|block|blocked|blacklist|denylist|skip|ignore|unsafe|danger|dangerous|forbid|forbidden|exclude|filter)[a-z0-9_]*\s*=\s*(?:/[^/\n]+/[gimsuy]*|r?['"][^'"]+['"]|re\.compile\s*\()`)

var rePercentEncoded = regexp.MustCompile(`%[0-9a-fA-F]{2}`)

// NormalizeForMatch returns a normalized companion view of raw tool input:
//   - unescapes JSON `\/` slashes
//   - decodes percent-encoded URL sequences (best-effort)
//   - adds camelCase/PascalCase split tokens (e.g. restoreFactorySettings -> restore_factory_settings)
//
// Matching against both raw and normalized text closes camelCase and URL-encoding
// blind spots while staying 100% compatible with existing regex rules.
func NormalizeForMatch(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.ReplaceAll(raw, `\/`, `/`)
	if strings.Contains(s, "%") {
		s = rePercentEncoded.ReplaceAllStringFunc(s, func(tok string) string {
			if decoded, err := url.PathUnescape(tok); err == nil && len(decoded) == 1 && decoded[0] >= 0x20 && decoded[0] < 0x7f {
				return decoded
			}
			return tok
		})
	}
	snake := splitCamelToSnake(s)
	if snake == s {
		return s
	}
	return s + "\n" + snake
}

// splitCamelToSnake converts camelCase/PascalCase identifiers into snake_case
// (e.g. `restoreFactorySettings` -> `restore_factory_settings`) while leaving
// delimiters (`/`, `?`, quotes, whitespace) intact so path-anchored regexes match.
func splitCamelToSnake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// IsBenignDocWrite reports whether toolName is a file-write tool targeting a
// pure text/markdown/log document that cannot execute commands or send requests.
func IsBenignDocWrite(toolName string, input []byte) bool {
	if toolName != "Write" && toolName != "Edit" && toolName != "MultiEdit" {
		return false
	}
	var payload struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return false
	}
	p := strings.TrimSpace(payload.FilePath)
	if p == "" {
		p = strings.TrimSpace(payload.Path)
	}
	if p == "" {
		return false
	}
	return nonExecutableDocExts[strings.ToLower(filepath.Ext(p))]
}

// SanitizeWriteInputForMatch strips defensive denylist regex lines from
// Write/Edit/MultiEdit content when evaluating static rules, preventing false
// positives on safety filters without blinding real command or HTTP payloads.
func SanitizeWriteInputForMatch(toolName string, input []byte) string {
	raw := string(input)
	if toolName != "Write" && toolName != "Edit" && toolName != "MultiEdit" {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"content", "new_string", "old_string"} {
		if v, ok := m[key].(string); ok && v != "" {
			cleaned := stripSafetyFilterLines(v)
			if cleaned != v {
				m[key] = cleaned
				changed = true
			}
		}
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, item := range edits {
			if em, ok := item.(map[string]any); ok {
				for _, key := range []string{"new_string", "old_string"} {
					if v, ok := em[key].(string); ok && v != "" {
						cleaned := stripSafetyFilterLines(v)
						if cleaned != v {
							em[key] = cleaned
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return string(b)
}

func stripSafetyFilterLines(content string) string {
	if !strings.Contains(content, "=") {
		return content
	}
	lines := strings.Split(content, "\n")
	changed := false
	for i, line := range lines {
		if reSafetyFilterLine.MatchString(line) {
			lines[i] = ""
			changed = true
		}
	}
	if !changed {
		return content
	}
	return strings.Join(lines, "\n")
}
