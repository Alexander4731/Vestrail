package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAuditFix_PromptSeedPreservesEditsAndRetries(t *testing.T) {
	pg, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := "prompt_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	agent, err := pg.CreateAgent(key, "Prompt fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAgent(key)

	if err := pg.SeedPromptIfEmpty(agent.ID, "invalid\x00template"); err == nil {
		t.Fatal("invalid template did not fail")
	}
	if tmpl, by, err := pg.CurrentPromptWithAuthor(agent.ID); err != nil || tmpl != "" || by != "" {
		t.Fatalf("failed seed left an active prompt: (%q, %q, %v)", tmpl, by, err)
	}

	const builtin = "original fixture"
	seedResults := make(chan error, 6)
	for range cap(seedResults) {
		go func() { seedResults <- pg.SeedPromptIfEmpty(agent.ID, builtin) }()
	}
	for range cap(seedResults) {
		if err := <-seedResults; err != nil {
			t.Error(err)
		}
	}
	if tmpl, by, err := pg.CurrentPromptWithAuthor(agent.ID); err != nil || tmpl != builtin || by != "system" {
		t.Fatalf("seed did not recover: (%q, %q, %v)", tmpl, by, err)
	}
	// Unmodified system prompt must upgrade cleanly to a newer builtin default (F12).
	const builtinV2 = "upgraded builtin fixture"
	if upgraded, err := pg.SyncBuiltinPrompt(agent.ID, builtinV2); err != nil || !upgraded {
		t.Fatalf("expected unmodified system prompt to upgrade: (%v, %v)", upgraded, err)
	}
	if tmpl, by, err := pg.CurrentPromptWithAuthor(agent.ID); err != nil || tmpl != builtinV2 || by != "system" {
		t.Fatalf("sync did not activate v2: (%q, %q, %v)", tmpl, by, err)
	}
	// Repeating sync with same builtin text is an idempotent no-op.
	if upgraded, err := pg.SyncBuiltinPrompt(agent.ID, builtinV2); err != nil || upgraded {
		t.Fatalf("idempotent sync unexpectedly upgraded: (%v, %v)", upgraded, err)
	}
	const custom = "custom fixture"
	if version, err := pg.SavePrompt(agent.ID, custom, "edited", "ui"); err != nil || version != 3 {
		t.Fatalf("save: (%d, %v), want version 3", version, err)
	}
	if upgraded, err := pg.SyncBuiltinPrompt(agent.ID, "new fixture default"); err != nil || upgraded {
		t.Fatalf("sync replaced or errored on user-edited prompt: (%v, %v)", upgraded, err)
	}
	if tmpl, by, err := pg.CurrentPromptWithAuthor(agent.ID); err != nil || tmpl != custom || by != "ui" {
		t.Fatalf("sync replaced the edited prompt: (%q, %q, %v)", tmpl, by, err)
	}
	history, err := pg.ListPromptVersions(agent.ID)
	if err != nil || len(history) != 3 || history[2].Template != builtin || history[1].Template != builtinV2 {
		t.Fatalf("unexpected prompt history: %+v, %v", history, err)
	}
}
