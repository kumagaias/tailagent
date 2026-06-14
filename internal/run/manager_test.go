package run

import (
	"testing"

	"github.com/kumagaias/tailagent/internal/model"
)

func TestResolvedCommandPathFallsBackToAgentDefault(t *testing.T) {
	if got := resolvedCommandPath(model.Agent{Type: "Codex"}); got != "codex" {
		t.Fatalf("resolved Codex command = %q, want codex", got)
	}
	if got := resolvedCommandPath(model.Agent{Type: "Kiro"}); got != "kiro-cli" {
		t.Fatalf("resolved Kiro command = %q, want kiro-cli", got)
	}
	if got := resolvedCommandPath(model.Agent{Type: "Copilot"}); got != "gh" {
		t.Fatalf("resolved Copilot command = %q, want gh", got)
	}
	if got := resolvedCommandPath(model.Agent{Type: "Codex", CommandPath: "/opt/bin/codex"}); got != "/opt/bin/codex" {
		t.Fatalf("resolved explicit command = %q, want /opt/bin/codex", got)
	}
}
