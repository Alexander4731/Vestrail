package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCaptureArchivesRawBeforeCleaning(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"oversized progress overwrites", strings.Repeat("old value\r", 2000) + "visible"},
		{"short ANSI and CRLF", "\x1b[32m中文\x1b[0m\r\n完成"},
		{"short progress overwrites", "first\rsecond\rfinal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			dir := t.TempDir()
			got := CaptureOnce(&ToolContext{OutputDir: dir, MaxOutputChars: 100}, raw)
			if !strings.Contains(got, "<persisted-output>") {
				t.Fatalf("cleaned output lost its raw archive pointer: %q", got)
			}
			files, err := filepath.Glob(filepath.Join(dir, "output-*.txt"))
			if err != nil || len(files) != 1 {
				t.Fatalf("expected one archive, got %v, err=%v", files, err)
			}
			stored, err := os.ReadFile(files[0])
			if err != nil || string(stored) != raw {
				t.Fatalf("raw bytes did not survive archive readback: err=%v", err)
			}
			if second := CaptureOnce(&ToolContext{OutputDir: dir, MaxOutputChars: 100}, got); second != got {
				t.Fatal("second capture changed the archived preview")
			}
		})
	}
}

func TestCaptureWithoutArchivePreservesShortRawOutput(t *testing.T) {
	raw := "\x1b[32m中文\x1b[0m\r\nfirst\rfinal"
	if got := Capture(&ToolContext{MaxOutputChars: len(raw)}, raw); got != raw {
		t.Fatalf("no archive available, but original bytes were changed: %q", got)
	}
}

func TestCaptureArchiveFailureIsExplicit(t *testing.T) {
	blockedDir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blockedDir, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"first\rfinal", strings.Repeat("中文\r", 200)} {
		tc := &ToolContext{OutputDir: blockedDir, MaxOutputChars: 50}
		got := CaptureOnce(tc, raw)
		if !strings.Contains(got, "[output-archive-failed]") || strings.Contains(got, "<persisted-output>") {
			t.Fatalf("archive failure was hidden or claimed success: %q", got)
		}
		if !strings.HasPrefix(got, truncate(raw, 50)) {
			t.Fatalf("archive failure must retain the uncleaned raw preview: %q", got)
		}
		if second := CaptureOnce(tc, got); second != got {
			t.Fatal("archive-failure notice was changed by a second capture")
		}
	}
	stored, err := os.ReadFile(blockedDir)
	if err != nil || string(stored) != "keep" {
		t.Fatal("failed archive overwrote the blocking file")
	}
}

func TestCaptureArchiveBoundariesPreserveUTF8AndRawBytes(t *testing.T) {
	for _, limit := range []int{1, 2, 5, 100} {
		dir := t.TempDir()
		raw := "\x1b[32m" + strings.Repeat("中文🙂", 30) + "\x1b[0m\r\n结束"
		got := Capture(&ToolContext{OutputDir: dir, MaxOutputChars: limit}, raw)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8", limit)
		}
		files, _ := filepath.Glob(filepath.Join(dir, "output-*.txt"))
		if len(files) != 1 {
			t.Fatalf("limit %d: missing archive", limit)
		}
		stored, err := os.ReadFile(files[0])
		if err != nil || string(stored) != raw {
			t.Fatalf("limit %d lost raw bytes", limit)
		}
	}
}

func TestCaptureCleanOutputAtLimitDoesNotArchive(t *testing.T) {
	dir := t.TempDir()
	raw := "中文\nunchanged"
	if got := Capture(&ToolContext{OutputDir: dir, MaxOutputChars: len(raw)}, raw); got != raw {
		t.Fatalf("clean output at limit changed: %q", got)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("clean output at limit was unnecessarily archived: %v", err)
	}
}
