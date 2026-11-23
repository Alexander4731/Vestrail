package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapturePreviewArchivesExactRawOutput(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Repeat("raw diagnostic \r\n", 100)
	preview := "100 valid records"
	got := CapturePreview(&ToolContext{OutputDir: dir, MaxOutputChars: 1000}, raw, preview)
	if !strings.HasPrefix(got, preview) || !strings.Contains(got, "<persisted-output>") {
		t.Fatal("missing concise preview and archive pointer")
	}
	files, err := filepath.Glob(filepath.Join(dir, "output-*.txt"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archives=%v err=%v", files, err)
	}
	archived, err := os.ReadFile(files[0])
	if err != nil || string(archived) != raw {
		t.Fatal("raw output changed")
	}
	if CaptureOnce(&ToolContext{OutputDir: dir, MaxOutputChars: 1000}, got) != got {
		t.Fatal("preview captured twice")
	}
	if got := CapturePreview(&ToolContext{MaxOutputChars: len(raw) + 1}, raw, preview); got != raw {
		t.Fatal("raw output lost without an archive")
	}
}
