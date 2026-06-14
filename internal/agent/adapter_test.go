package agent

import (
	"strings"
	"testing"

	"github.com/kumagaias/tailagent/internal/model"
)

func TestDefaultCommandsAndSupportedTypes(t *testing.T) {
	tests := []struct {
		agentType string
		command   string
	}{
		{"Codex", "codex"},
		{"Claude", "claude"},
		{"Kiro", "kiro-cli"},
		{"Copilot", "gh"},
	}
	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			if !IsSupportedType(tt.agentType) {
				t.Fatalf("%s should be supported", tt.agentType)
			}
			if got := DefaultCommand(tt.agentType); got != tt.command {
				t.Fatalf("DefaultCommand(%q) = %q, want %q", tt.agentType, got, tt.command)
			}
		})
	}
	if IsSupportedType("Unknown") {
		t.Fatal("Unknown should not be supported")
	}
	if got := DefaultCommand("Unknown"); got != "" {
		t.Fatalf("DefaultCommand(Unknown) = %q, want empty", got)
	}
}

func TestBuildPromptExplainsExpectedDiagnosticExitCodes(t *testing.T) {
	prompt := BuildPrompt(
		model.Agent{},
		model.Project{},
		model.Milestone{},
		model.Task{},
		"",
	)

	want := "git diff --no-index returns 1 when differences are found"
	if !strings.Contains(prompt, want) {
		t.Fatalf("BuildPrompt() does not contain %q", want)
	}
}

func TestCopilotCommandUsesGitHubCLIEntrypoint(t *testing.T) {
	ad, err := New("Copilot", "")
	if err != nil {
		t.Fatal(err)
	}
	name, args := ad.Command("fix the bug")
	if name != "gh" {
		t.Fatalf("command name = %q, want gh", name)
	}
	if got := strings.Join(args, " "); got != "copilot -p fix the bug" {
		t.Fatalf("command args = %q, want copilot -p fix the bug", got)
	}
}

func TestCopilotCommandAllowsDirectBinary(t *testing.T) {
	ad, err := New("Copilot", "/opt/bin/copilot")
	if err != nil {
		t.Fatal(err)
	}
	name, args := ad.Command("fix the bug")
	if name != "/opt/bin/copilot" {
		t.Fatalf("command name = %q, want /opt/bin/copilot", name)
	}
	if got := strings.Join(args, " "); got != "-p fix the bug" {
		t.Fatalf("command args = %q, want -p fix the bug", got)
	}
}
