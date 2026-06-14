package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/kumagaias/tailagent/internal/agent"
	"github.com/kumagaias/tailagent/internal/model"
	storesqlite "github.com/kumagaias/tailagent/internal/storage/sqlite"
)

type Manager struct {
	store            *storesqlite.Store
	mu               sync.Mutex
	active           map[int64]*exec.Cmd
	stopped          map[int64]bool
	pendingApprovals map[int64]chan string
	slots            chan struct{}
	approvalTimeout  time.Duration
}

func NewManager(store *storesqlite.Store, maxConcurrent, approvalTimeoutSeconds int) *Manager {
	if maxConcurrent < 1 {
		maxConcurrent = 2
	}
	if approvalTimeoutSeconds < 1 {
		approvalTimeoutSeconds = 300
	}
	return &Manager{
		store:            store,
		active:           make(map[int64]*exec.Cmd),
		stopped:          make(map[int64]bool),
		pendingApprovals: make(map[int64]chan string),
		slots:            make(chan struct{}, maxConcurrent),
		approvalTimeout:  time.Duration(approvalTimeoutSeconds) * time.Second,
	}
}

func (m *Manager) Start(ctx context.Context, taskID, agentID int64, extra string) (model.Run, error) {
	t, p, ms, a, err := m.store.GetTaskRunContext(ctx, taskID, agentID)
	if err != nil {
		return model.Run{}, err
	}
	if _, err := os.Stat(p.FolderPath); err != nil {
		return model.Run{}, fmt.Errorf("project folder is unavailable: %w", err)
	}
	ad, err := agent.New(a.Type, a.CommandPath)
	if err != nil {
		return model.Run{}, err
	}
	if err := ad.Validate(ctx); err != nil {
		return model.Run{}, fmt.Errorf("%s is unavailable: %w", a.Type, err)
	}
	commandPath := resolvedCommandPath(a)
	prompt := agent.BuildPrompt(a, p, ms, t, extra)
	r := model.Run{ProjectID: p.ID, MilestoneID: ms.ID, TaskID: t.ID, AgentID: a.ID, Status: "queued", Instruction: prompt, WorkingDirectory: p.FolderPath, TraceID: newTraceID()}
	if err := m.store.CreateRun(ctx, &r); err != nil {
		return model.Run{}, err
	}
	if err := m.store.SetTaskStatus(ctx, t.ID, "in_progress"); err != nil {
		ended := time.Now().UTC()
		_ = m.store.FinishRun(ctx, r.ID, "error", -1, err.Error(), ended)
		return model.Run{}, fmt.Errorf("set task in progress: %w", err)
	}
	_ = m.store.AddTrace(ctx, &model.TraceEvent{TraceID: r.TraceID, RunID: r.ID, EventType: "agent_run", Status: "queued", Message: "Agent run queued", Attributes: map[string]any{"agent": a.Type, "task": t.Title}})
	if a.Type == "Codex" {
		go m.executeCodex(r, commandPath, prompt)
	} else {
		go m.execute(r, ad, prompt)
	}
	return r, nil
}

func resolvedCommandPath(a model.Agent) string {
	if a.CommandPath != "" {
		return a.CommandPath
	}
	return agent.DefaultCommand(a.Type)
}

func newTraceID() string { return fmt.Sprintf("%x-%x", time.Now().UnixNano(), os.Getpid()) }

func (m *Manager) execute(r model.Run, ad agent.Adapter, prompt string) {
	m.slots <- struct{}{}
	defer func() { <-m.slots }()
	ctx := context.Background()
	name, args := ad.Command(prompt)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.WorkingDirectory
	cmd.Env = append(os.Environ(), "TAILAGENT_RUN_ID="+fmt.Sprint(r.ID), "TAILAGENT_TRACE_ID="+r.TraceID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.failStart(r, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.failStart(r, err)
		return
	}
	started := time.Now().UTC()
	if err = cmd.Start(); err != nil {
		m.failStart(r, err)
		return
	}
	m.mu.Lock()
	m.active[r.ID] = cmd
	m.mu.Unlock()
	_ = m.store.StartRun(ctx, r.ID, cmd.Process.Pid, started)
	_ = m.store.AddTrace(ctx, &model.TraceEvent{TraceID: r.TraceID, RunID: r.ID, EventType: "agent_run", Status: "running", Message: "Agent process started", StartedAt: started, Attributes: map[string]any{"pid": cmd.Process.Pid}})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.capture(r, "stdout", stdout) }()
	go func() { defer wg.Done(); m.capture(r, "stderr", stderr) }()
	waitErr := cmd.Wait()
	wg.Wait()
	m.mu.Lock()
	delete(m.active, r.ID)
	wasStopped := m.stopped[r.ID]
	delete(m.stopped, r.ID)
	m.mu.Unlock()
	ended := time.Now().UTC()
	exitCode := 0
	status := "success"
	message := "Agent run completed"
	errText := ""
	if wasStopped {
		status = "cancelled"
		message = "Agent run cancelled"
		exitCode = -1
		if waitErr != nil {
			errText = waitErr.Error()
		}
	} else if waitErr != nil {
		status = "error"
		message = "Agent run failed"
		errText = waitErr.Error()
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	_ = m.store.FinishRun(ctx, r.ID, status, exitCode, errText, ended)
	duration := ended.Sub(started).Milliseconds()
	_ = m.store.AddTrace(ctx, &model.TraceEvent{TraceID: r.TraceID, RunID: r.ID, EventType: "agent_run", Status: status, Message: message, StartedAt: started, EndedAt: &ended, DurationMS: &duration, Attributes: map[string]any{"exit_code": exitCode, "error": errText}})
}

func (m *Manager) failStart(r model.Run, err error) {
	ended := time.Now().UTC()
	_ = m.store.FinishRun(context.Background(), r.ID, "error", -1, err.Error(), ended)
	_ = m.store.AddTrace(context.Background(), &model.TraceEvent{TraceID: r.TraceID, RunID: r.ID, EventType: "system_event", Status: "error", Message: "Failed to start agent: " + err.Error()})
}

func (m *Manager) capture(r model.Run, stream string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		text := scanner.Text()
		_ = m.store.AddLog(context.Background(), r.ID, stream, text)
		_ = m.store.AddTrace(context.Background(), &model.TraceEvent{TraceID: r.TraceID, RunID: r.ID, EventType: stream, Status: "running", Message: redact(text)})
	}
	if err := scanner.Err(); err != nil {
		_ = m.store.AddLog(context.Background(), r.ID, "stderr", "log capture: "+err.Error())
	}
}

func redact(s string) string {
	// Avoid retaining common secret assignment formats in local traces.
	for _, key := range []string{"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "AWS_SECRET_ACCESS_KEY=", "GITHUB_TOKEN="} {
		if i := indexFold(s, key); i >= 0 {
			return s[:i] + key + "[REDACTED]"
		}
	}
	return s
}

func indexFold(s, sub string) int {
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			a, b := s[i+j], sub[j]
			if a >= 'a' && a <= 'z' {
				a -= 32
			}
			if b >= 'a' && b <= 'z' {
				b -= 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func (m *Manager) Stop(runID int64) error {
	m.mu.Lock()
	cmd := m.active[runID]
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("run is not active")
	}
	m.mu.Lock()
	m.stopped[runID] = true
	m.mu.Unlock()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		m.mu.Lock()
		delete(m.stopped, runID)
		m.mu.Unlock()
		return err
	}
	_ = m.store.SetRunStatus(context.Background(), runID, "cancelled")
	return nil
}

func (m *Manager) ResolveApproval(approvalID int64, decision string) error {
	m.mu.Lock()
	ch := m.pendingApprovals[approvalID]
	m.mu.Unlock()
	if ch == nil {
		return errors.New("approval is no longer waiting for a Codex response")
	}
	select {
	case ch <- decision:
		return nil
	default:
		return errors.New("approval decision was already delivered")
	}
}
