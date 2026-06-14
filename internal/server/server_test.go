package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kumagaias/tailagent/internal/model"
	storesqlite "github.com/kumagaias/tailagent/internal/storage/sqlite"
)

func TestDashboardAndEmbeddedUI(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})

	for _, path := range []string{"/", "/app.css", "/app.js", "/favicon.svg"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var dashboard model.Dashboard
	if err := json.NewDecoder(rec.Body).Decode(&dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Agents != 0 || dashboard.Projects != 0 {
		t.Fatalf("unexpected dashboard: %#v", dashboard)
	}
}

func TestAgentsAPICachesCodexStatusProbe(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}

	calls := 0
	originalProbe := probeCodexStatusFunc
	probeCodexStatusFunc = func(context.Context, string) (*model.AgentAccount, *model.AgentUsage) {
		calls++
		remaining := 42
		return &model.AgentAccount{Login: "codex@example.com", Plan: "team"}, &model.AgentUsage{RemainingPct: &remaining}
	}
	t.Cleanup(func() { probeCodexStatusFunc = originalProbe })

	app := New(store, model.Settings{WorkspaceRoot: t.TempDir(), MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var agents []model.Agent
		if err := json.NewDecoder(rec.Body).Decode(&agents); err != nil {
			t.Fatal(err)
		}
		if len(agents) != 1 || agents[0].Usage == nil || agents[0].Usage.RemainingPct == nil || *agents[0].Usage.RemainingPct != 42 {
			t.Fatalf("Codex usage was not enriched: %#v", agents)
		}
	}
	if calls != 1 {
		t.Fatalf("Codex status probe calls = %d, want 1", calls)
	}
}

func TestAgentsAPIRejectsDuplicateType(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	body := `{"type":"Codex"}`

	for i, wantStatus := range []int{http.StatusOK, http.StatusBadRequest} {
		req := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(body))
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("request %d status = %d, want %d: %s", i+1, rec.Code, wantStatus, rec.Body.String())
		}
		if i == 1 && !strings.Contains(rec.Body.String(), "Codex agent has already been added") {
			t.Fatalf("duplicate error = %s", rec.Body.String())
		}
	}
}

func TestAgentLoginAndLogoutAPI(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}

	originalLogin := startCodexLoginFunc
	originalLogout := logoutCodexFunc
	originalOpenLoginURL := openLoginURLFunc
	startCodexLoginFunc = func(*Server) (string, error) {
		return "https://auth.example.test/login", nil
	}
	logoutCalls := 0
	logoutCodexFunc = func(context.Context, string) error {
		logoutCalls++
		return nil
	}
	openLoginURLCalls := 0
	openLoginURLFunc = func(url string) error {
		openLoginURLCalls++
		if url != "https://auth.example.test/login" {
			return fmt.Errorf("unexpected login URL: %s", url)
		}
		return nil
	}
	t.Cleanup(func() {
		startCodexLoginFunc = originalLogin
		logoutCodexFunc = originalLogout
		openLoginURLFunc = originalOpenLoginURL
	})

	app := New(store, model.Settings{WorkspaceRoot: t.TempDir(), MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+strconv.FormatInt(agent.ID, 10)+"/login", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body.String())
	}
	var login struct {
		AuthURL       string `json:"auth_url"`
		BrowserOpened bool   `json:"browser_opened"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	if login.AuthURL != "https://auth.example.test/login" {
		t.Fatalf("auth_url = %q", login.AuthURL)
	}
	if !login.BrowserOpened {
		t.Fatal("browser_opened = false, want true")
	}
	if openLoginURLCalls != 1 {
		t.Fatalf("open login URL calls = %d, want 1", openLoginURLCalls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agents/"+strconv.FormatInt(agent.ID, 10)+"/logout", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d: %s", rec.Code, rec.Body.String())
	}
	if logoutCalls != 1 {
		t.Fatalf("logout calls = %d, want 1", logoutCalls)
	}
}

func TestStartCodexLoginCancelsExistingSessionBeforeRetry(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*"initialize"*) printf '%s\n' '{"id":1,"result":{}}' ;;
		*"account/login/start"*) printf '%s\n' '{"id":2,"result":{"type":"chatgpt","authUrl":"https://auth.example.test/login","loginId":"login-id"}}' ;;
	esac
done
`), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	app := &Server{settings: model.Settings{WorkspaceRoot: t.TempDir()}}
	firstURL, err := app.startCodexLogin()
	if err != nil {
		t.Fatalf("first login start failed: %v", err)
	}
	if firstURL == "" {
		t.Fatal("first login URL is empty")
	}
	secondURL, err := app.startCodexLogin()
	if err != nil {
		t.Fatalf("second login start failed: %v", err)
	}
	if secondURL != firstURL {
		t.Fatalf("second login URL = %q, want %q", secondURL, firstURL)
	}
	app.cancelCodexLogin()
}

func TestAgentLoginRejectsUnsupportedAgent(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := model.Agent{Type: "Claude"}
	if err := store.SaveAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}

	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+strconv.FormatInt(agent.ID, 10)+"/login", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestEnrichAgentsMarksCodexNotReadyUntilRemainingIsAvailable(t *testing.T) {
	originalProbe := probeCodexStatusFunc
	probeCodexStatusFunc = func(context.Context, string) (*model.AgentAccount, *model.AgentUsage) {
		return &model.AgentAccount{Login: "codex@example.com"}, &model.AgentUsage{UnavailableText: "rate limits unavailable"}
	}
	t.Cleanup(func() { probeCodexStatusFunc = originalProbe })

	app := &Server{settings: model.Settings{WorkspaceRoot: t.TempDir()}}
	agents := []model.Agent{{Type: "Codex", Status: "ready"}}
	app.enrichAgents(context.Background(), agents)

	if agents[0].Status != "not_ready" {
		t.Fatalf("agent status = %q, want not_ready", agents[0].Status)
	}
	if agents[0].LastError != "rate limits unavailable" {
		t.Fatalf("agent last error = %q, want rate limits unavailable", agents[0].LastError)
	}
}

func TestEnrichAgentsKeepsCodexReadyWhenRemainingIsAvailable(t *testing.T) {
	originalProbe := probeCodexStatusFunc
	probeCodexStatusFunc = func(context.Context, string) (*model.AgentAccount, *model.AgentUsage) {
		remaining := 31
		return &model.AgentAccount{Login: "codex@example.com"}, &model.AgentUsage{RemainingPct: &remaining}
	}
	t.Cleanup(func() { probeCodexStatusFunc = originalProbe })

	app := &Server{settings: model.Settings{WorkspaceRoot: t.TempDir()}}
	agents := []model.Agent{{Type: "Codex", Status: "ready"}}
	app.enrichAgents(context.Background(), agents)

	if agents[0].Status != "ready" {
		t.Fatalf("agent status = %q, want ready", agents[0].Status)
	}
}

func TestCodexStatusCacheDurationIsShortForAuthRequired(t *testing.T) {
	if got := codexStatusCacheDuration(&model.AgentAccount{AuthMode: "openai"}, nil); got != codexAuthRequiredCacheTTL {
		t.Fatalf("auth-required account cache duration = %s, want %s", got, codexAuthRequiredCacheTTL)
	}
	if got := codexStatusCacheDuration(nil, &model.AgentUsage{UnavailableText: "Codex status unavailable: Codex app-server error -32600: codex account authentication required to read rate limits"}); got != codexAuthRequiredCacheTTL {
		t.Fatalf("auth-required usage cache duration = %s, want %s", got, codexAuthRequiredCacheTTL)
	}
	remaining := 10
	if got := codexStatusCacheDuration(&model.AgentAccount{Login: "codex@example.com"}, &model.AgentUsage{RemainingPct: &remaining}); got != codexStatusCacheTTL {
		t.Fatalf("ready cache duration = %s, want %s", got, codexStatusCacheTTL)
	}
}

func TestEmbeddedUIRestoresLastView(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`localStorage.getItem(viewStorageKey)`,
		`localStorage.setItem(viewStorageKey,view)`,
		`view:storedView()`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUIExcludesAlreadyAddedAgentTypes(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const agentTypes=["Codex","Claude","Kiro","Copilot"]`,
		`function availableAgentTypes(a={})`,
		`!state.agents.some(existing=>existing.type===type)`,
		`All supported agents have already been added.`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUIAgentFormEditsCommandPath(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const agentCommandDefaults={Codex:"codex",Claude:"claude",Kiro:"kiro-cli",Copilot:"gh"}`,
		`field("Command path","command_path"`,
		`gh, or /absolute/path`,
		`form.command_path.value=agentCommandDefaults[form.type.value]||""`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUIProvidesCodexLoginAndLogout(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`function agentAuthButton(a)`,
		`Logout`,
		`/login`, `{method:"POST"}`,
		`/logout`, `confirmLabel="Delete"`,
		`browser_opened`,
		`Open this login URL manually`,
		`pollAgentLogin(a.id)`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUITaskImageChooserHasWideButton(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jsBody), `<input class="image-input" type="file"`) {
		t.Fatal("task image input does not have its dedicated style class")
	}
	for _, want := range []string{`multiple>`, `zone.ondrop=`, `20*1024*1024`, `>=5`} {
		if !strings.Contains(string(jsBody), want) {
			t.Fatalf("task image picker does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBody), `.image-input::file-selector-button{min-width:180px;`) {
		t.Fatal("app.css does not widen the task image chooser button")
	}
	if !strings.Contains(string(cssBody), `.image-drop-zone.drag-over{`) {
		t.Fatal("app.css does not style the active image drop zone")
	}
}

func TestEmbeddedUIShowsPendingApprovalBadgeInNav(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBody)
	for _, want := range []string{
		`function pendingApprovalCount(){return state.approvals.filter(a=>a.status==="pending").length}`,
		`v==="approvals"&&approvals?`,
		`class="nav-badge"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	if !strings.Contains(css, `.nav-badge{margin-left:auto;min-width:20px;`) {
		t.Fatal("app.css does not style pending approval nav badges")
	}
}

func TestEmbeddedUIAddsChatsBelowApprovalsAndTaskEntryPoint(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`approvals:["Approvals","Review permission requests from agents."],chats:["Chats","Talk with an agent using project or task context."]`,
		`api("/api/chats")`,
		`chats:renderChats`,
		`<button class="primary" id="taskChat"`,
		`$("#taskChat").onclick=()=>chatForm({task:t})`,
		`function renderChats()`,
		`function chatForm({task}={})`,
		`function newChatPanel()`,
		`function wireNewChatPanel()`,
		`task_id:d.task`,
		`function chatAgentOptions(selected)`,
		`<select class="chat-agent-select" name="agent_id" aria-label="Agent">`,
		`form.agent_id.onchange=()=>state.chatDraft.agent=+form.agent_id.value`,
		`JSON.stringify({content,agent_id:agentID})`,
		`task:task?.id||0`,
		`<summary>Show details</summary>`,
		`logs.filter(l=>l.stream==="assistant")`,
		`state.chatDetails[d.dataset.messageId]=d.open`,
		"api(`/api/chats/${chat.id}/messages`,{method:\"POST\"",
		`state.view==="chats"&&state.activeChatID`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
	for _, unwanted := range []string{
		`<select id="chatAgent">`,
		`<select id="chatProject">`,
		`<select id="chatTaskContext">`,
		`function chatHeaderActions()`,
		`<button id="newChat">New chat</button>`,
	} {
		if strings.Contains(js, unwanted) {
			t.Fatalf("app.js unexpectedly contains %q", unwanted)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, want := range []string{
		`.chat-layout{display:grid;grid-template-columns:300px minmax(0,1fr);`,
		`.chat-compose{display:grid;grid-template-columns:1fr minmax(150px,180px) auto;`,
		`.chat-start-compose{width:min(860px,100%);`,
		`.chat-agent-select{align-self:stretch;`,
		`.chat-details{margin-top:12px;`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css does not contain %q", want)
		}
	}
}

func TestEmbeddedUIShowsAgentInstallationGuide(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`function agentInstallGuide(type)`,
		`curl -fsSL https://chatgpt.com/codex/install.sh | sh`,
		`https://developers.openai.com/codex/cli`,
		`curl -fsSL https://claude.ai/install.sh | bash`,
		`https://code.claude.com/docs/en/setup`,
		`curl -fsSL https://cli.kiro.dev/install | bash`,
		`https://kiro.dev/docs/cli/installation/`,
		`Check the <a href="${guide.url}" target="_blank" rel="noreferrer">official installation guide</a>`,
		`$("#agentInstallGuide").innerHTML=agentInstallGuide(form.type.value)`,
		`form.command_path.value=agentCommandDefaults[form.type.value]||""`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBody), `.agent-install-guide{`) {
		t.Fatal("app.css does not style the agent installation guide")
	}
}

func TestEmbeddedUIKeepsChatInputAcrossPollingRenders(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`chatInputs:{new:""}`,
		`${esc(state.chatInputs[chat.id]||"")}`,
		`form.content.oninput=()=>state.chatInputs[chat.id]=form.content.value`,
		`composer.setSelectionRange(...selection)`,
		`state.chatInputs[chat.id]=""`,
		`function wireChatSubmitOnEnter(form)`,
		`e.key==="Enter"&&!e.shiftKey&&!e.isComposing`,
		`form.requestSubmit()`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not preserve chat input with %q", want)
		}
	}
}

func TestChatReplyLinesPrefersAssistantMessages(t *testing.T) {
	logs := []model.RunLog{
		{Stream: "stdout", Content: "command output"},
		{Stream: "assistant", Content: "agent reply"},
	}
	if got := chatReplyLines(logs, 40); got != "agent reply" {
		t.Fatalf("chatReplyLines() = %q, want agent reply", got)
	}

	legacy := []model.RunLog{{Stream: "stdout", Content: "legacy reply"}}
	if got := chatReplyLines(legacy, 40); got != "legacy reply" {
		t.Fatalf("chatReplyLines() legacy fallback = %q, want legacy reply", got)
	}
}

func TestParseChatTaskProposalSupportsOneOrManyTasks(t *testing.T) {
	logs := []model.RunLog{{Stream: "assistant", Content: `I prepared these tasks.

` + "```tailagent_tasks" + `
{"tasks":[{"title":" First ","description":"one"},{"title":"Second","milestone_id":42,"acceptance_criteria":"done"}]}
` + "```"}}
	tasks, ok := parseChatTaskProposal(logs)
	if !ok {
		t.Fatal("proposal was not parsed")
	}
	if len(tasks) != 2 || tasks[0].Title != "First" || tasks[1].MilestoneID != 42 {
		t.Fatalf("unexpected proposal: %#v", tasks)
	}

	invalid := []model.RunLog{{Stream: "assistant", Content: "```tailagent_tasks\n{\"tasks\":[{\"title\":\"\"}]}\n```"}}
	if _, ok := parseChatTaskProposal(invalid); ok {
		t.Fatal("proposal with an empty title was accepted")
	}
}

func TestEmbeddedUIShowsChatTaskProposalConfirmation(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBody)
	for _, want := range []string{
		`function chatTaskProposalHTML(m)`,
		`Review before creating`,
		`class="primary chat-create-tasks"`,
		"api(`/api/chats/${chat.id}/tasks`",
		`message_id:+b.dataset.messageId`,
		"answer.replace(/```tailagent_tasks",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBody), `.chat-task-proposal{margin-top:14px;`) {
		t.Fatal("app.css does not style chat task proposals")
	}
}

func TestEmbeddedUIPositionsNoticesAtBottom(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(body)
	if !strings.Contains(css, ".notice{position:absolute;right:24px;bottom:24px;") {
		t.Fatal("app.css does not position notices at the bottom")
	}
	if strings.Contains(css, ".notice{position:absolute;right:24px;top:") {
		t.Fatal("app.css still positions notices at the top")
	}
}

func TestEmbeddedUIShowsAddCardWhenTodoLaneIsEmpty(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	if !strings.Contains(js, `emptyAdd=!items.length&&s==="todo"`) {
		t.Fatal("app.js does not show the add card based on the ToDo lane contents")
	}
	if strings.Contains(js, `emptyAdd=!list.length&&s==="todo"`) {
		t.Fatal("app.js still requires every task lane to be empty")
	}
}

func TestEmbeddedUIProjectRowsOpenTasksAndExposeEditAction(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBody)
	for _, want := range []string{
		`<tr data-id="${p.id}" tabindex="0">`,
		`<span class="name-action"><b>${esc(p.name)}</b><button class="project-edit"`,
		`r.onclick=()=>openTasks(p.id)`,
		`r.querySelector(".project-edit").onclick=e=>{e.stopPropagation();projectForm(p)}`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	if !strings.Contains(css, `.project-table tbody tr:hover .project-edit,.project-table tbody tr:focus-within .project-edit`) {
		t.Fatal("app.css does not reveal the project edit action on hover or focus")
	}
}

func TestEmbeddedUIMilestoneRowsOpenTasksAndExposeEditAction(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBody)
	for _, want := range []string{
		`<div class="task-header-row">${breadcrumb}</div><table class="milestone-table">`,
		`<a href="#projects" id="milestoneProjectBreadcrumb">Projects</a>`,
		`<tr data-id="${m.id}" tabindex="0">`,
		`<span class="name-action"><b>${esc(m.name)}</b>${m.is_default_none?'<span class="badge">default</span>':` + "`<button class=\"milestone-edit\"",
		`r.onclick=()=>openTasks(m.project_id,m.id)`,
		`if(b)b.onclick=e=>{e.stopPropagation();milestoneForm(m)}`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	if !strings.Contains(css, `.milestone-table tbody tr:hover .milestone-edit,.milestone-table tbody tr:focus-within .milestone-edit`) {
		t.Fatal("app.css does not reveal the milestone edit action on hover or focus")
	}
}

func TestEmbeddedUIListTablesSupportColumnSorting(t *testing.T) {
	jsBody, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBody)
	for _, want := range []string{
		`sorts:{agents:{key:"type",direction:"asc"},projects:{key:"name",direction:"asc"},milestones:{key:"project_name",direction:"asc"}}`,
		`function sortHeader(view,key,label)`,
		`function sortedRows(view,rows,values)`,
		`function wireSortHeaders(view,render)`,
		`sortHeader("agents","type","Agent")`,
		`sortHeader("projects","name","Project")`,
		`sortHeader("milestones","project_name","Project")}${sortHeader("milestones","name","Milestone")`,
		`sort.direction==="asc"?"desc":"asc"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain sortable table support %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, want := range []string{
		`.sort-header{all:unset;display:inline-flex;`,
		`.sort-header.active .sort-arrow{color:var(--blue)}`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css does not contain sortable table styling %q", want)
		}
	}
}

func TestEmbeddedUITaskCardsShowAgentRunLabelsWithoutMilestone(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	if !strings.Contains(js, `taskAgentLabels(t)`) || !strings.Contains(js, `run?badge(run.status):""`) {
		t.Fatal("app.js does not show agent and latest run status labels on task cards")
	}
	if strings.Contains(js, `<div class="meta"><span>${esc(t.project_name)} · ${esc(t.milestone_name)}`) {
		t.Fatal("app.js still shows milestone names on task cards")
	}
}

func TestEmbeddedUITasksExposeAndPreserveLabels(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const taskLabels=["feature","bugfix","refactor","docs","chore"]`,
		`${badge(t.label||"chore")}${badge(agent||"Unassigned")}`,
		`<span>Label</span><div>${badge(t.label||"chore")} ${t.label_auto?`,
		`select("Label","label",taskLabels.map(x=>({value:x,label:x})),t.label||"chore")`,
		`name="label_auto" value="true"`,
		`v.label_auto=v.label_auto==="true"`,
		`label:t.label,label_auto:t.label_auto,status`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBody), `.checkbox-field{display:flex;align-items:center;gap:8px;`) {
		t.Fatal("app.css does not style the task label auto checkbox")
	}
}

func TestEmbeddedUIProjectPrefixAndTaskDisplayID(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`function defaultProjectPrefix(name)`,
		`sortHeader("projects","prefix","Prefix")`,
		`field("Task prefix","prefix",prefixValue,"text",prefixExtra)`,
		`p.id?"readonly":'required minlength="2" maxlength="5" pattern="[A-Za-z0-9]{2,5}"'`,
		`function taskDisplayID(t)`,
		`${esc(taskDisplayID(t))} ${esc(t.title)}`,
		`<span>Task ID</span><b>${esc(taskDisplayID(t))}</b>`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUITaskCardImagesDoNotInterceptDrag(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`<div class="card" draggable="true" data-id="${t.id}">`,
		`<img class="card-image" draggable="false" src="/api/tasks/${t.id}/image" alt="">`,
		`c.ondragstart=e=>{dragged=true;c.classList.add("dragging");e.dataTransfer.effectAllowed="move";e.dataTransfer.setData("text/plain",c.dataset.id)}`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUITaskBreadcrumbShowsSelectedProjectAndNonDefaultMilestone(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`<a href="#projects" id="taskProjectBreadcrumb">Projects</a>`,
		`<span class="task-breadcrumb-current">${project?esc(project.name):"All"}</span>`,
		`showMilestone=milestone&&!milestone.is_default_none`,
		`<a href="#milestones" id="taskMilestoneBreadcrumb">Milestones</a>`,
		`<span class="task-breadcrumb-current">${esc(milestone.name)}</span>`,
		`setView("projects")`,
		`setView("milestones")`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
	if strings.Contains(js, `Projects (${esc(project?.name||"All")})`) {
		t.Fatal("app.js still combines the Projects link and selected project name")
	}
}

func TestEmbeddedUITaskMilestoneFilterIsDisabledForAllProjects(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`if(!state.filters.project)state.filters.milestone=0`,
		`<select id="mf" ${state.filters.project?"":"disabled"}>`,
		`${state.filters.project?state.milestones.filter(m=>m.project_id==state.filters.project)`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUITasksExposeReloadAndRequiredFields(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const reloadButton=` + "`<button class=\"task-reload${state.taskReloading?\" is-loading\":\"\"}\" id=\"reloadTasks\" type=\"button\" aria-label=\"Reload tasks\" title=\"Reload tasks\" ${state.taskReloading?\"disabled\":\"\"}><span aria-hidden=\"true\">⟳</span></button>`",
		`await Promise.all([load(),new Promise(resolve=>setTimeout(resolve,900))])`,
		`<div class="task-header-row">${breadcrumb}${reloadButton}</div>`,
		`$("#reloadTasks").onclick=reloadTasks`,
		`field(` + "`Title ${required}`" + `,"title",t.title||"","text","required")`,
		`select(` + "`Project ${required}`" + `,"project_id",projectOptions(),pid,true,"Select project","required")`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, want := range []string{
		`.task-header-row{display:flex;align-items:center;justify-content:space-between;`,
		`.task-reload{width:40px;height:40px;`,
		`.task-reload.is-loading span{animation:spin .9s linear infinite}`,
		`.required-mark{color:var(--red);`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css does not contain %q", want)
		}
	}
}

func TestEmbeddedUITasksLimitLaneItemsAndExposeShowMore(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const taskPageSize=5`,
		`taskVisible:{}`,
		`function taskVisibleKey(status)`,
		`function showMoreTasks(status,total)`,
		`shown=items.slice(0,visible)`,
		`<button class="lane-more" data-more-status="${s}">もっと見る (${remaining})</button>`,
		`$("#content").querySelectorAll("[data-more-status]").forEach`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}

	cssBody, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	if !strings.Contains(css, `.lane-more{width:100%;margin:8px 0;`) {
		t.Fatal("app.css does not style the show more button")
	}
}

func TestEmbeddedUITaskListHidesLatestLogAndDetailShowsIt(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, forbidden := range []string{
		`const log=latestLogLines([{content:t.latest_log||""}])`,
		`<div class="card-latest-log">${esc(log)}</div>`,
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js still shows latest log on task cards: %q", forbidden)
		}
	}
	for _, want := range []string{
		`function latestRun(t)`,
		`const run=latestRun(t)`,
		`function latestLogLines(logs,max=20)`,
		`<div class="kv"><span>Additional instruction</span><div>${esc(run.instruction||"—")}</div></div>`,
		`<div class="latest-log" id="latestLog" data-run-id="${run.id}">Loading latest log...</div>`,
		`el.textContent=latestLogLines(logs)||"No output yet"`,
		`$("#showLogs").onclick=()=>showLogs(run,t)`,
		`<button class="back-to-task" id="backToTask">Back to task</button>`,
		`$("#backToTask").onclick=()=>taskView(task)`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUIInProgressAndAgentDoneTasksExposeRerunCorrection(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`isAgentDone=t.status==="agent_done"`,
		`isRerunTask=isAgentDone||t.status==="in_progress"`,
		`runLabel=isRerunTask?"Rerun":"Run"`,
		`>${runLabel}</button>`,
		`${isAgentDone?'<button class="primary" id="closeTask">Close</button>':""}`,
		`isRerun=t.status==="agent_done"||t.status==="in_progress"`,
		`instructionLabel=isRerun?"Correction instruction":"Additional instruction"`,
		`submitLabel=isRerun?"Start rerun":"Start run"`,
		`field(instructionLabel,"instruction","","textarea")`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js does not contain %q", want)
		}
	}
}

func TestEmbeddedUIShowsTimeoutAndErrorBadgesInRed(t *testing.T) {
	body, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(body)
	if !strings.Contains(css, `.badge.error,.badge.timeout`) {
		t.Fatal("app.css does not style timeout badges with error badges")
	}
	if !strings.Contains(css, `.badge.error,.badge.timeout,.badge.denied,.badge.expired,.badge.unavailable,.badge.not_ready,.badge.cancelled{color:var(--red)}`) {
		t.Fatal("app.css does not style timeout and error badges red")
	}
}

func TestCleanupStaleWaitingApprovalRuns(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	agent := model.Agent{Type: "Codex"}
	if err := store.SaveAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	staleTask := model.Task{ProjectID: project.ID, Title: "stale", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &staleTask); err != nil {
		t.Fatal(err)
	}
	activeTask := model.Task{ProjectID: project.ID, Title: "active", AssignedAgentID: &agent.ID}
	if err := store.SaveTask(ctx, &activeTask); err != nil {
		t.Fatal(err)
	}
	staleRun := model.Run{ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: staleTask.ID, AgentID: agent.ID, Status: "waiting_approval", WorkingDirectory: project.FolderPath, TraceID: "stale-trace"}
	if err := store.CreateRun(ctx, &staleRun); err != nil {
		t.Fatal(err)
	}
	activeRun := model.Run{ProjectID: project.ID, MilestoneID: project.DefaultMilestoneID, TaskID: activeTask.ID, AgentID: agent.ID, Status: "waiting_approval", WorkingDirectory: project.FolderPath, TraceID: "active-trace"}
	if err := store.CreateRun(ctx, &activeRun); err != nil {
		t.Fatal(err)
	}
	staleApproval := model.Approval{RunID: staleRun.ID, AgentID: agent.ID, ProjectID: project.ID, TaskID: staleTask.ID, RequestType: "command", Operation: "echo stale", Risk: "medium"}
	if err := store.CreateApproval(ctx, &staleApproval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideApproval(ctx, staleApproval.ID, "allowed"); err != nil {
		t.Fatal(err)
	}
	activeApproval := model.Approval{RunID: activeRun.ID, AgentID: agent.ID, ProjectID: project.ID, TaskID: activeTask.ID, RequestType: "command", Operation: "echo active", Risk: "medium"}
	if err := store.CreateApproval(ctx, &activeApproval); err != nil {
		t.Fatal(err)
	}
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	if err := app.cleanupStaleWaitingApprovalRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	gotStale, err := store.GetRun(ctx, staleRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStale.Status != "timeout" {
		t.Fatalf("stale run status = %q, want timeout", gotStale.Status)
	}
	gotActive, err := store.GetRun(ctx, activeRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotActive.Status != "waiting_approval" {
		t.Fatalf("active run status = %q, want waiting_approval", gotActive.Status)
	}
}

func TestDeleteTaskAPI(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(t.Context(), &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Delete via API"}
	if err := store.SaveTask(t.Context(), &task); err != nil {
		t.Fatal(err)
	}
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})

	req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+strconv.FormatInt(task.ID, 10), bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	tasks, err := store.ListTasks(t.Context(), project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("task was not deleted: %#v", tasks)
	}
}

func TestTaskImageAPI(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(t.Context(), &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Attach image"}
	if err := store.SaveTask(t.Context(), &task); err != nil {
		t.Fatal(err)
	}
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})

	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "screenshot.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}

	body.Reset()
	writer = multipart.NewWriter(&body)
	part, err = writer.CreateFormFile("images", "second.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPut, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second upload status = %d: %s", rec.Code, rec.Body.String())
	}
	tasks, err := store.ListTasks(t.Context(), project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || len(tasks[0].Images) != 2 {
		t.Fatalf("task images = %#v", tasks)
	}
	secondID := tasks[0].Images[1].ID
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/images/"+strconv.FormatInt(secondID, 10), nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("get second image status = %d body = %x", rec.Code, rec.Body.Bytes())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("image body = %x, want %x", rec.Body.Bytes(), png)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/images/"+strconv.FormatInt(secondID, 10), nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete second status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTaskImageAPIRejectsMoreThanFiveImages(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(t.Context(), &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Too many screenshots"}
	if err := store.SaveTask(t.Context(), &task); err != nil {
		t.Fatal(err)
	}
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < 6; i++ {
		part, err := writer.CreateFormFile("images", fmt.Sprintf("%d.png", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(png); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTaskImageAPIRejectsNonImage(t *testing.T) {
	store, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := model.Project{Name: "example", FolderPath: t.TempDir()}
	if err := store.SaveProject(t.Context(), &project); err != nil {
		t.Fatal(err)
	}
	task := model.Task{ProjectID: project.ID, Title: "Reject attachment"}
	if err := store.SaveTask(t.Context(), &task); err != nil {
		t.Fatal(err)
	}
	app := New(store, model.Settings{MaxConcurrentAgents: 2, ApprovalTimeoutSecs: 300})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("not an image")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/image", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}
