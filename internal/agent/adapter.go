package agent

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/kumagaias/tailagent/internal/model"
)

type Adapter interface {
	Type() string
	Validate(context.Context) error
	Command(prompt string) (string, []string)
}

type CLIAdapter struct {
	agentType   string
	commandPath string
}

var defaultCommands = map[string]string{
	"Codex":   "codex",
	"Claude":  "claude",
	"Kiro":    "kiro-cli",
	"Copilot": "gh",
}

func New(agentType, commandPath string) (Adapter, error) {
	if !IsSupportedType(agentType) {
		return nil, fmt.Errorf("unsupported agent type %q", agentType)
	}
	if commandPath == "" {
		commandPath = DefaultCommand(agentType)
	}
	return CLIAdapter{agentType: agentType, commandPath: commandPath}, nil
}

func (a CLIAdapter) Type() string { return a.agentType }

func IsSupportedType(agentType string) bool {
	_, ok := defaultCommands[agentType]
	return ok
}

func DefaultCommand(agentType string) string {
	return defaultCommands[agentType]
}

func (a CLIAdapter) Validate(context.Context) error {
	_, err := exec.LookPath(a.commandPath)
	return err
}

func (a CLIAdapter) Command(prompt string) (string, []string) {
	switch a.agentType {
	case "Codex":
		return a.commandPath, []string{"exec", "--color", "never", prompt}
	case "Claude":
		return a.commandPath, []string{"-p", prompt}
	case "Copilot":
		base := filepath.Base(a.commandPath)
		if base == "gh" || base == "gh.exe" {
			return a.commandPath, []string{"copilot", "-p", prompt}
		}
		return a.commandPath, []string{"-p", prompt}
	default:
		return a.commandPath, []string{prompt}
	}
}

func BuildPrompt(a model.Agent, p model.Project, m model.Milestone, t model.Task, extra string) string {
	return fmt.Sprintf(`You are running inside tailagent.

Agent instruction:
%s

Project:
- Name: %s
- Working directory: %s
- Git root: %s

Milestone:
- Name: %s
- Description: %s

Task:
- ID: %s
- Title: %s
- Label: %s
- Description: %s
- Task instruction: %s
- Acceptance criteria: %s

Additional user instruction:
%s

Execution policy:
- Work only within the project unless an explicit permission request is approved.
- Keep changes focused on the task.
- Run relevant tests before completing.
- Handle expected non-zero statuses from diagnostic commands explicitly (for example, git diff --no-index returns 1 when differences are found).
- Report failures clearly.
`, a.Instruction, p.Name, p.FolderPath, p.GitRoot, m.Name, m.Description, t.DisplayID, t.Title, t.Label, t.Description, t.Instruction, t.AcceptanceCriteria, extra)
}
