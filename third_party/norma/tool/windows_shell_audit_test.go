package tool

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestAuditFix_WindowsShellPromptAndErrorPropagation(t *testing.T) {
	if p := bashPromptForPlatform("windows", "powershell"); !strings.Contains(p, "Do NOT use '&&'") {
		t.Fatalf("powershell 5.1 prompt must warn against &&: %s", p)
	}
	if p := bashPromptForPlatform("windows", "pwsh"); !strings.Contains(p, "PowerShell 7") || !strings.Contains(p, "do NOT use bash heredocs") {
		t.Fatalf("pwsh prompt mismatch: %s", p)
	}
	if p := bashPromptForPlatform("linux", ""); !strings.Contains(p, "bash") {
		t.Fatalf("unix prompt mismatch: %s", p)
	}
	if runtime.GOOS != "windows" {
		return
	}
	bash := NewBash()
	tc := &ToolContext{WorkingDir: t.TempDir()}

	// Non-terminating cmdlet error followed by an echo must now fail (is_error=true)
	inBad, _ := json.Marshal(map[string]any{
		"command": "Get-ChildItem 'nonexistent_path_vestrail_audit_xyz'; Write-Output 'TAIL_SHOULD_NOT_RUN'",
	})
	res, err := bash.Call(context.Background(), inBad, tc)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected non-terminating error to set is_error=true, got: %s", res.Flatten())
	}
	if strings.Contains(res.Flatten(), "TAIL_SHOULD_NOT_RUN") {
		t.Fatalf("tail command executed after non-terminating error: %s", res.Flatten())
	}

	// Explicitly handled error via try/catch must still succeed cleanly
	inHandled, _ := json.Marshal(map[string]any{
		"command": "try { Get-ChildItem 'nonexistent_path_vestrail_audit_xyz' | Out-Null } catch { Write-Output 'HANDLED_OK' }",
	})
	res2, err := bash.Call(context.Background(), inHandled, tc)
	if err != nil {
		t.Fatal(err)
	}
	if res2.IsError || !strings.Contains(res2.Flatten(), "HANDLED_OK") {
		t.Fatalf("expected handled try/catch to succeed, isErr=%v out=%s", res2.IsError, res2.Flatten())
	}
}
