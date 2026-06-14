package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumagaias/tailagent/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "tailagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSaveAgentRejectsDuplicateType(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	first := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &first); err != nil {
		t.Fatal(err)
	}

	duplicate := model.Agent{Type: "Codex"}
	err := store.SaveAgent(ctx, &duplicate)
	if err == nil || !strings.Contains(err.Error(), "Codex agent has already been added") {
		t.Fatalf("duplicate agent error = %v", err)
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
}

func TestSaveAgentRejectsChangingToExistingType(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	codex := model.Agent{Type: "Codex"}
	claude := model.Agent{Type: "Claude"}
	if err := store.SaveAgent(ctx, &codex); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, &claude); err != nil {
		t.Fatal(err)
	}

	claude.Type = "Codex"
	if err := store.SaveAgent(ctx, &claude); err == nil {
		t.Fatal("changing an agent to an existing type was accepted")
	}
}

func TestSaveAgentAcceptsCopilot(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	agent := model.Agent{Type: "Copilot"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.CommandPath != "gh" {
		t.Fatalf("command path = %q, want gh", agent.CommandPath)
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Type != "Copilot" {
		t.Fatalf("agents = %#v, want one Copilot agent", agents)
	}
}

func TestSaveAgentUsesDefaultCommands(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
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
			agent := model.Agent{Type: tt.agentType}
			if err := store.SaveAgent(ctx, &agent); err != nil {
				t.Fatal(err)
			}
			if agent.CommandPath != tt.command {
				t.Fatalf("command path = %q, want %q", agent.CommandPath, tt.command)
			}
		})
	}
}

func TestMigrateKiroCommandPathUsesOfficialCLIName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailagent.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agent := model.Agent{Type: "Kiro", CommandPath: "kiro"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].CommandPath != "kiro-cli" {
		t.Fatalf("Kiro command path = %q, want kiro-cli", agents[0].CommandPath)
	}
}

func TestListAgentsNormalizesCapabilityPrefixes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE agents SET capabilities_json=? WHERE id=?`, `["supports_streaming","supported_approval","send_message"]`, agent.ID); err != nil {
		t.Fatal(err)
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(agents[0].Capabilities, ","), "streaming,approval,send_message"; got != want {
		t.Fatalf("capabilities = %q, want %q", got, want)
	}
}

func TestProjectCreatesDefaultMilestone(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	folder := t.TempDir()

	project := model.Project{Name: "example", FolderPath: folder}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	if project.DefaultMilestoneID == 0 {
		t.Fatal("default milestone ID was not assigned")
	}

	milestones, err := store.ListMilestones(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(milestones) != 1 || milestones[0].Name != "None" || !milestones[0].IsDefaultNone {
		t.Fatalf("unexpected milestones: %#v", milestones)
	}
}

func TestProjectPrefixDefaultsToConsonantsAndCannotChange(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "tailagent", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	if project.Prefix != "TLGNT" {
		t.Fatalf("prefix = %q, want TLGNT", project.Prefix)
	}

	project.Prefix = "OTHER"
	project.Name = "renamed"
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	if project.Prefix != "TLGNT" {
		t.Fatalf("prefix changed to %q", project.Prefix)
	}
}

func TestProjectPrefixValidationAndUniqueness(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, prefix := range []string{"A", "ABCDEF", "A_B"} {
		project := model.Project{Name: "example", Prefix: prefix, FolderPath: t.TempDir()}
		if err := store.SaveProject(ctx, &project); err == nil {
			t.Fatalf("prefix %q was accepted", prefix)
		}
	}
	if err := store.SaveProject(ctx, &model.Project{Name: "one", Prefix: "AB12", FolderPath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject(ctx, &model.Project{Name: "two", Prefix: "ab12", FolderPath: t.TempDir()}); err == nil {
		t.Fatal("duplicate prefix was accepted")
	}
}

func TestTasksUseProjectPrefixAndProjectSequence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "tailagent", Prefix: "TAIL", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	first := model.Task{ProjectID: project.ID, Title: "First"}
	second := model.Task{ProjectID: project.ID, Title: "Second"}
	if err := store.SaveTask(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if first.DisplayID != "TAIL-1" || second.DisplayID != "TAIL-2" {
		t.Fatalf("display IDs = %q, %q", first.DisplayID, second.DisplayID)
	}

	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, task := range tasks {
		got[task.DisplayID] = true
	}
	if len(tasks) != 2 || !got["TAIL-1"] || !got["TAIL-2"] {
		t.Fatalf("unexpected tasks: %#v", tasks)
	}
}

func TestExistingProjectsAndTasksMigrateToPrefixedIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailagent.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := model.Project{Name: "legacy", Prefix: "LGCY", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	firstTask := model.Task{ProjectID: project.ID, Title: "First legacy task"}
	secondTask := model.Task{ProjectID: project.ID, Title: "Second legacy task"}
	if err := store.SaveTask(ctx, &firstTask); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(ctx, &secondTask); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET prefix=NULL WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET project_number=NULL WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	displayIDs := map[string]bool{}
	for _, task := range tasks {
		displayIDs[task.DisplayID] = true
	}
	if len(projects) != 1 || projects[0].Prefix != "LGCY" || len(tasks) != 2 || !displayIDs["LGCY-1"] || !displayIDs["LGCY-2"] {
		t.Fatalf("legacy IDs were not migrated: projects=%#v tasks=%#v", projects, tasks)
	}
}

func TestExistingProjectPrefixMigrationAvoidsCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailagent.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := model.Project{Name: "example", Prefix: "XMPL", FolderPath: t.TempDir()}
	second := model.Project{Name: "example", Prefix: "XMPL2", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET prefix=NULL WHERE id IN (?,?)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prefixes := map[string]bool{}
	for _, project := range projects {
		prefixes[project.Prefix] = true
	}
	if len(projects) != 2 || !prefixes["XMPL"] || !prefixes["XMPL2"] {
		t.Fatalf("prefixes were not made unique: %#v", projects)
	}
}

func TestTaskWithoutMilestoneUsesDefault(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}

	task := model.Task{ProjectID: project.ID, Title: "Implement feature"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if task.MilestoneID != project.DefaultMilestoneID {
		t.Fatalf("milestone = %d, want %d", task.MilestoneID, project.DefaultMilestoneID)
	}
}

func TestChatLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	otherAgent := model.Agent{Type: "Claude"}
	if err := store.SaveAgent(ctx, &otherAgent); err != nil {
		t.Fatal(err)
	}
	chat := model.Chat{ProjectID: project.ID, AgentID: agent.ID, Title: "Design review"}
	if err := store.CreateChat(ctx, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.TaskID == 0 || chat.TaskTitle != "Chat: Design review" {
		t.Fatalf("chat did not create a task context: %#v", chat)
	}
	if err := store.UpdateChatAgent(ctx, chat.ID, otherAgent.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetChat(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AgentID != otherAgent.ID || updated.AgentType != "Claude" {
		t.Fatalf("chat agent was not updated: %#v", updated)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: chat.TaskID,
		AgentID: otherAgent.ID, Status: "success", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "chat-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := store.AddLog(ctx, run.ID, "assistant", "assistant reply"); err != nil {
		t.Fatal(err)
	}
	msg, err := store.AddChatMessage(ctx, chat.ID, &run.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID == 0 || msg.RunID == nil || *msg.RunID != run.ID {
		t.Fatalf("message did not retain run id: %#v", msg)
	}
	pending, err := store.AddChatMessage(ctx, chat.ID, nil, "second turn")
	if err != nil {
		t.Fatal(err)
	}
	if pending.RunID != nil {
		t.Fatalf("pending message has a run id: %#v", pending)
	}
	messages, err := store.ListChatMessages(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Run == nil || len(messages[0].Logs) != 1 || messages[0].Logs[0].Content != "assistant reply" || messages[1].Run != nil {
		t.Fatalf("chat message did not include run output: %#v", messages)
	}
}

func TestConfirmChatTaskProposalCreatesMultipleTasksOnce(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	milestone := model.Milestone{ProjectID: project.ID, Name: "Release"}
	if err := store.SaveMilestone(ctx, &milestone); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	chat := model.Chat{ProjectID: project.ID, AgentID: agent.ID, Title: "Plan work"}
	if err := store.CreateChat(ctx, &chat); err != nil {
		t.Fatal(err)
	}
	message, err := store.AddChatMessage(ctx, chat.ID, nil, "create two tasks")
	if err != nil {
		t.Fatal(err)
	}
	proposals := []model.TaskProposalItem{
		{Title: "First task", Description: "Uses None"},
		{Title: "Second task", MilestoneID: milestone.ID, Instruction: "Implement it", AcceptanceCriteria: "Tests pass"},
	}
	if err := store.SaveChatTaskProposal(ctx, message.ID, proposals); err != nil {
		t.Fatal(err)
	}

	created, err := store.ConfirmChatTaskProposal(ctx, chat.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d tasks, want 2", len(created))
	}
	if created[0].ProjectID != project.ID || created[0].MilestoneID != project.DefaultMilestoneID {
		t.Fatalf("first task context = %#v", created[0])
	}
	if created[1].MilestoneID != milestone.ID || created[1].Instruction != "Implement it" || created[1].AcceptanceCriteria != "Tests pass" {
		t.Fatalf("second task = %#v", created[1])
	}
	proposal, err := store.GetChatTaskProposal(ctx, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proposal == nil || proposal.Status != "created" || len(proposal.CreatedTaskIDs) != 2 {
		t.Fatalf("confirmed proposal = %#v", proposal)
	}
	if _, err := store.ConfirmChatTaskProposal(ctx, chat.ID, message.ID); err == nil {
		t.Fatal("duplicate confirmation succeeded")
	}
}

func TestConfirmChatTaskProposalRejectsMilestoneFromAnotherProjectAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "one", FolderPath: t.TempDir()}
	other := model.Project{Name: "two", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject(ctx, &other); err != nil {
		t.Fatal(err)
	}
	foreignMilestone := model.Milestone{ProjectID: other.ID, Name: "Foreign"}
	if err := store.SaveMilestone(ctx, &foreignMilestone); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	chat := model.Chat{ProjectID: project.ID, AgentID: agent.ID, Title: "Plan work"}
	if err := store.CreateChat(ctx, &chat); err != nil {
		t.Fatal(err)
	}
	message, err := store.AddChatMessage(ctx, chat.ID, nil, "create tasks")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChatTaskProposal(ctx, message.ID, []model.TaskProposalItem{
		{Title: "Would otherwise be inserted"},
		{Title: "Invalid milestone", MilestoneID: foreignMilestone.ID},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmChatTaskProposal(ctx, chat.ID, message.ID); err == nil {
		t.Fatal("confirmation with foreign milestone succeeded")
	}
	after, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("partial tasks were created: before=%d after=%d", len(before), len(after))
	}
}

func TestTaskImageLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Task with screenshot"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}

	first := model.TaskImage{
		TaskID: task.ID, Name: "first.png", ContentType: "image/png", Data: []byte("first"),
	}
	if err := store.SaveTaskImage(ctx, first); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ImageName != first.Name || tasks[0].ImageSize != int64(len(first.Data)) {
		t.Fatalf("image metadata was not listed: %#v", tasks)
	}

	second := model.TaskImage{
		TaskID: task.ID, Name: "second.webp", ContentType: "image/webp", Data: []byte("second"),
	}
	if err := store.SaveTaskImage(ctx, second); err != nil {
		t.Fatal(err)
	}
	images, err := store.ListTaskImages(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].Name != first.Name || images[1].Name != second.Name {
		t.Fatalf("unexpected image list: %#v", images)
	}
	got, err := store.GetTaskImage(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != first.Name || got.ContentType != first.ContentType || !bytes.Equal(got.Data, first.Data) {
		t.Fatalf("unexpected first image: %#v", got)
	}

	if err := store.DeleteTaskImageByID(ctx, task.ID, images[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetTaskImage(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != second.Name {
		t.Fatalf("remaining image = %#v", got)
	}
	if err := store.DeleteTaskImages(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTaskImage(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("image was not deleted: %v", err)
	}
}

func TestTaskImagesLimitedToFive(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Five screenshots"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	var images []model.TaskImage
	for i := 0; i < 5; i++ {
		images = append(images, model.TaskImage{
			TaskID: task.ID, Name: fmt.Sprintf("%d.png", i), ContentType: "image/png", Data: []byte{byte(i)},
		})
	}
	if err := store.SaveTaskImages(ctx, images); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTaskImage(ctx, model.TaskImage{
		TaskID: task.ID, Name: "sixth.png", ContentType: "image/png", Data: []byte("six"),
	}); err == nil {
		t.Fatal("expected sixth image to be rejected")
	}
}

func TestOpenMigratesSingleTaskImageSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE projects (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, folder_path TEXT NOT NULL UNIQUE,
 git_root TEXT NOT NULL DEFAULT '', git_remote TEXT NOT NULL DEFAULT '',
 default_milestone_id INTEGER, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE milestones (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'open',
 target_date TEXT, is_default_none INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(project_id, name)
);
CREATE TABLE tasks (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 milestone_id INTEGER NOT NULL REFERENCES milestones(id), title TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'todo',
 assigned_agent_id INTEGER, instruction TEXT NOT NULL DEFAULT '',
 acceptance_criteria TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE task_images (
 task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
 name TEXT NOT NULL, content_type TEXT NOT NULL, size INTEGER NOT NULL,
 data BLOB NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
INSERT INTO projects VALUES(1,'legacy','/tmp/legacy','','',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO milestones VALUES(1,1,'None','', 'open',NULL,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO tasks VALUES(1,1,1,'Legacy task','','todo',NULL,'','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO task_images VALUES(1,'legacy.png','image/png',3,X'010203',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	images, err := store.ListTaskImages(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].ID == 0 || images[0].Name != "legacy.png" {
		t.Fatalf("migrated images = %#v", images)
	}
}

func TestDeleteTaskRemovesImage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Delete image with task"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTaskImage(ctx, model.TaskImage{
		TaskID: task.ID, Name: "shot.png", ContentType: "image/png", Data: []byte("image"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTaskImage(ctx, task.ID); err == nil {
		t.Fatal("task image was not deleted")
	}
}

func TestProjectPathMustBeUnique(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	folder := t.TempDir()
	if err := store.SaveProject(ctx, &model.Project{Name: "one", FolderPath: folder}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProject(ctx, &model.Project{Name: "two", FolderPath: folder}); err == nil {
		t.Fatal("expected duplicate folder error")
	}
}

func TestDefaultMilestoneCannotBeEdited(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	milestone := model.Milestone{ID: project.DefaultMilestoneID, ProjectID: project.ID, Name: "Renamed"}
	if err := store.SaveMilestone(ctx, &milestone); err == nil {
		t.Fatal("expected default milestone edit to fail")
	}
}

func TestListAgentsAndProjectsAfterRunStarted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{
		ProjectID:       project.ID,
		Title:           "Trace test",
		AssignedAgentID: &agent.ID,
	}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID:        project.ID,
		MilestoneID:      project.DefaultMilestoneID,
		TaskID:           task.ID,
		AgentID:          agent.ID,
		Status:           "queued",
		Instruction:      "test",
		WorkingDirectory: project.FolderPath,
		TraceID:          "trace-test",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.StartRun(ctx, run.ID, 1234, started); err != nil {
		t.Fatal(err)
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].LastRun == nil {
		t.Fatalf("agent last run was not loaded: %#v", agents)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].LastRun == nil {
		t.Fatalf("project last run was not loaded: %#v", projects)
	}
}

func TestListTasksIncludesLatestRunAndLog(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{
		ProjectID:       project.ID,
		Title:           "Show latest log",
		AssignedAgentID: &agent.ID,
	}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	first := model.Run{
		ProjectID:        project.ID,
		MilestoneID:      project.DefaultMilestoneID,
		TaskID:           task.ID,
		AgentID:          agent.ID,
		Status:           "success",
		Instruction:      "first",
		WorkingDirectory: project.FolderPath,
		TraceID:          "trace-first",
	}
	if err := store.CreateRun(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := store.AddLog(ctx, first.ID, "stdout", "older log"); err != nil {
		t.Fatal(err)
	}
	latest := model.Run{
		ProjectID:        project.ID,
		MilestoneID:      project.DefaultMilestoneID,
		TaskID:           task.ID,
		AgentID:          agent.ID,
		Status:           "running",
		Instruction:      "latest",
		WorkingDirectory: project.FolderPath,
		TraceID:          "trace-latest",
	}
	if err := store.CreateRun(ctx, &latest); err != nil {
		t.Fatal(err)
	}
	if err := store.AddLog(ctx, latest.ID, "stdout", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	var latestLines []string
	for i := 1; i <= 25; i++ {
		line := fmt.Sprintf("line %02d", i)
		if err := store.AddLog(ctx, latest.ID, "stdout", line); err != nil {
			t.Fatal(err)
		}
		if i > 5 {
			latestLines = append(latestLines, line)
		}
	}

	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].LatestRun == nil || tasks[0].LatestRun.ID != latest.ID || tasks[0].LatestRun.Status != "running" {
		t.Fatalf("latest run was not listed: %#v", tasks[0].LatestRun)
	}
	if tasks[0].LatestLog != strings.Join(latestLines, "\n") {
		t.Fatalf("latest log = %q", tasks[0].LatestLog)
	}
}

func TestSetTaskStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Move me"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskStatus(ctx, task.ID, "in_progress"); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != "in_progress" {
		t.Fatalf("unexpected tasks: %#v", tasks)
	}
	if err := store.SetTaskStatus(ctx, task.ID, "agent_done"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskStatus(ctx, task.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskStatus(ctx, task.ID, "invalid"); err == nil {
		t.Fatal("expected invalid status to fail")
	}
}

func TestOpenMigratesLegacyTaskStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailagent.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	reviewTask := model.Task{ProjectID: project.ID, Title: "Review task"}
	doneTask := model.Task{ProjectID: project.ID, Title: "Done task"}
	if err := store.SaveTask(ctx, &reviewTask); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(ctx, &doneTask); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status='review' WHERE id=?`, reviewTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET status='done' WHERE id=?`, doneTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[int64]string{}
	for _, task := range tasks {
		statuses[task.ID] = task.Status
	}
	if statuses[reviewTask.ID] != "agent_done" || statuses[doneTask.ID] != "closed" {
		t.Fatalf("legacy statuses were not migrated: %#v", statuses)
	}
}

func TestSuccessfulRunMovesTaskToAgentDone(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Complete me", Status: "in_progress", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: task.ID,
		AgentID: agent.ID, Status: "running", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "successful-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, run.ID, "success", 0, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != "agent_done" {
		t.Fatalf("task status = %#v, want agent_done", tasks)
	}
}

func TestFailedRunLeavesTaskInProgress(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Retry me", Status: "in_progress", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: task.ID,
		AgentID: agent.ID, Status: "running", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "failed-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, run.ID, "error", 1, "failed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != "in_progress" {
		t.Fatalf("task status = %#v, want in_progress", tasks)
	}
}

func TestDeleteAgentUnassignsTasks(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Assigned", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAgent(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].AssignedAgentID != nil {
		t.Fatalf("task remained assigned: %#v", tasks)
	}
}

func TestDeleteProjectRemovesChildren(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Child"}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("project was not deleted: %#v", projects)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks were not deleted: %#v", tasks)
	}
}

func TestDeleteTaskRemovesTaskAndRunHistory(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Delete me", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: task.ID,
		AgentID: agent.ID, Status: "success", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "deleted-task-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("task was not deleted: %#v", tasks)
	}
	if _, err := store.GetRun(ctx, run.ID); err == nil {
		t.Fatal("run history was not deleted")
	}
}

func TestDeleteAgentWithActiveRunFails(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Running", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: task.ID,
		AgentID: agent.ID, Status: "running", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "active-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAgent(ctx, agent.ID); err == nil {
		t.Fatal("expected active run to block agent deletion")
	}
}

func TestDeleteTaskWithActiveRunFails(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Running", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	run := model.Run{
		ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: task.ID,
		AgentID: agent.ID, Status: "running", Instruction: "test",
		WorkingDirectory: project.FolderPath, TraceID: "active-task-run",
	}
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTask(ctx, task.ID); err == nil {
		t.Fatal("expected active run to block task deletion")
	}
}

func TestSaveTaskInfersAndPersistsLabels(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}

	autoTask := model.Task{ProjectID: project.ID, Title: "Fix broken login"}
	if err := store.SaveTask(ctx, &autoTask); err != nil {
		t.Fatal(err)
	}
	if autoTask.Label != "bugfix" || !autoTask.LabelAuto {
		t.Fatalf("auto label = %q auto=%v, want bugfix true", autoTask.Label, autoTask.LabelAuto)
	}

	manualTask := model.Task{ProjectID: project.ID, Title: "Fix typo in guide", Label: "docs"}
	if err := store.SaveTask(ctx, &manualTask); err != nil {
		t.Fatal(err)
	}
	if manualTask.Label != "docs" || manualTask.LabelAuto {
		t.Fatalf("manual label = %q auto=%v, want docs false", manualTask.Label, manualTask.LabelAuto)
	}

	tasks, err := store.ListTasks(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.Task{}
	for _, task := range tasks {
		got[task.Title] = task
	}
	if got["Fix broken login"].Label != "bugfix" || !got["Fix broken login"].LabelAuto {
		t.Fatalf("listed auto task = %#v", got["Fix broken login"])
	}
	if got["Fix typo in guide"].Label != "docs" || got["Fix typo in guide"].LabelAuto {
		t.Fatalf("listed manual task = %#v", got["Fix typo in guide"])
	}
}

func TestSaveTaskRejectsInvalidManualLabel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}

	task := model.Task{ProjectID: project.ID, Title: "Work item", Label: "invalid"}
	if err := store.SaveTask(ctx, &task); err == nil {
		t.Fatal("invalid task label was accepted")
	}
}
