package run

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/kumagaias/tailagent/internal/model"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexSession struct {
	manager  *Manager
	run      model.Run
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writeMu  sync.Mutex
	started  time.Time
	thread   string
	timedOut bool
}

func (m *Manager) executeCodex(r model.Run, commandPath, prompt string) {
	m.slots <- struct{}{}
	defer func() { <-m.slots }()

	cmd := exec.Command(commandPath, "app-server", "--listen", "stdio://")
	cmd.Dir = r.WorkingDirectory
	cmd.Env = append(os.Environ(), "TAILAGENT_RUN_ID="+fmt.Sprint(r.ID), "TAILAGENT_TRACE_ID="+r.TraceID)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.failStart(r, err)
		return
	}
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
	if err := cmd.Start(); err != nil {
		m.failStart(r, err)
		return
	}

	session := &codexSession{manager: m, run: r, cmd: cmd, stdin: stdin, started: started}
	m.mu.Lock()
	m.active[r.ID] = cmd
	m.mu.Unlock()
	_ = m.store.StartRun(context.Background(), r.ID, cmd.Process.Pid, started)
	_ = m.store.AddTrace(context.Background(), &model.TraceEvent{
		TraceID: r.TraceID, RunID: r.ID, EventType: "agent_run", Status: "running",
		Message: "Codex app-server started", StartedAt: started,
		Attributes: map[string]any{"pid": cmd.Process.Pid, "protocol": "app-server"},
	})

	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		m.capture(r, "stderr", stderr)
	}()

	if err := session.send(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "tailagent", "title": "tailagent", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		session.finish("error", -1, err.Error())
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	finished := false
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			_ = m.store.AddLog(context.Background(), r.ID, "stderr", "invalid app-server message: "+err.Error())
			continue
		}
		done, err := session.handle(message, prompt)
		if err != nil {
			session.finish("error", -1, err.Error())
			finished = true
			break
		}
		if done {
			finished = true
			break
		}
	}
	if !finished {
		errText := ""
		if err := scanner.Err(); err != nil {
			errText = err.Error()
		} else {
			errText = "Codex app-server closed before the turn completed"
		}
		session.finish("error", -1, errText)
	}

	_ = stdin.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	_ = cmd.Wait()
	stderrWG.Wait()
	m.mu.Lock()
	delete(m.active, r.ID)
	delete(m.stopped, r.ID)
	m.mu.Unlock()
}

func (s *codexSession) handle(message rpcMessage, prompt string) (bool, error) {
	if message.Error != nil {
		return false, fmt.Errorf("Codex app-server error %d: %s", message.Error.Code, message.Error.Message)
	}
	if len(message.ID) > 0 && message.Method == "" {
		var id int
		if err := json.Unmarshal(message.ID, &id); err != nil {
			return false, nil
		}
		switch id {
		case 1:
			if err := s.notify("initialized", map[string]any{}); err != nil {
				return false, err
			}
			return false, s.send(2, "thread/start", map[string]any{
				"cwd":               s.run.WorkingDirectory,
				"sandbox":           "workspace-write",
				"approvalPolicy":    "on-request",
				"approvalsReviewer": "user",
				"ephemeral":         true,
				"serviceName":       "tailagent",
			})
		case 2:
			var result struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if err := json.Unmarshal(message.Result, &result); err != nil {
				return false, err
			}
			if result.Thread.ID == "" {
				return false, errors.New("Codex app-server returned no thread ID")
			}
			s.thread = result.Thread.ID
			return false, s.send(3, "turn/start", map[string]any{
				"threadId":          s.thread,
				"input":             []map[string]any{{"type": "text", "text": prompt}},
				"cwd":               s.run.WorkingDirectory,
				"approvalPolicy":    "on-request",
				"approvalsReviewer": "user",
			})
		}
		return false, nil
	}

	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		return false, s.handleApproval(message)
	case "item/commandExecution/outputDelta":
		var params struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.Delta != "" {
			_ = s.manager.store.AddLog(context.Background(), s.run.ID, "stdout", params.Delta)
		}
	case "item/completed":
		s.handleCompletedItem(message.Params)
	case "turn/completed":
		return true, s.handleTurnCompleted(message.Params)
	}
	return false, nil
}

func (s *codexSession) handleApproval(message rpcMessage) error {
	var params map[string]any
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return err
	}
	operation := "Codex permission request"
	requestType := "permission"
	if command, ok := params["command"].(string); ok && command != "" {
		operation = command
		requestType = "command"
	} else if message.Method == "item/fileChange/requestApproval" {
		operation = "Apply file changes"
		requestType = "file_change"
	} else if permissions, ok := params["permissions"]; ok {
		raw, _ := json.Marshal(permissions)
		operation = string(raw)
		requestType = "permissions"
	}
	reason, _ := params["reason"].(string)
	approval := model.Approval{
		RunID: s.run.ID, AgentID: s.run.AgentID, ProjectID: s.run.ProjectID, TaskID: s.run.TaskID,
		RequestType: requestType, Operation: operation, Reason: reason, Risk: approvalRisk(params),
		ExpiresAt: time.Now().UTC().Add(s.manager.approvalTimeout),
	}
	if err := s.manager.store.CreateApproval(context.Background(), &approval); err != nil {
		return err
	}
	decisionCh := make(chan string, 1)
	s.manager.mu.Lock()
	s.manager.pendingApprovals[approval.ID] = decisionCh
	s.manager.mu.Unlock()
	defer func() {
		s.manager.mu.Lock()
		delete(s.manager.pendingApprovals, approval.ID)
		s.manager.mu.Unlock()
	}()

	_ = s.manager.store.SetRunStatus(context.Background(), s.run.ID, "waiting_approval")
	_ = s.manager.store.AddTrace(context.Background(), &model.TraceEvent{
		TraceID: s.run.TraceID, RunID: s.run.ID, EventType: "approval_request", Status: "waiting_approval",
		Message: operation, Attributes: map[string]any{"approval_id": approval.ID, "request_type": requestType, "risk": approval.Risk},
	})

	decision := <-decisionCh
	result := map[string]any{}
	if message.Method == "item/permissions/requestApproval" {
		if decision == "allowed" {
			result["permissions"] = params["permissions"]
			result["scope"] = "turn"
		} else {
			result["permissions"] = map[string]any{}
		}
	} else if decision == "allowed" {
		result["decision"] = "accept"
	} else if decision == "expired" {
		result["decision"] = "cancel"
	} else {
		result["decision"] = "decline"
	}
	if err := s.respond(message.ID, result); err != nil {
		return err
	}
	if decision == "expired" {
		s.timedOut = true
		return s.manager.store.SetRunStatus(context.Background(), s.run.ID, "timeout")
	}
	return s.manager.store.SetRunStatus(context.Background(), s.run.ID, "running")
}

func approvalRisk(params map[string]any) string {
	if network, ok := params["networkApprovalContext"]; ok && network != nil {
		return "high"
	}
	if permissions, ok := params["permissions"].(map[string]any); ok {
		if network, ok := permissions["network"].(map[string]any); ok && network["enabled"] == true {
			return "high"
		}
	}
	return "medium"
}

func (s *codexSession) handleCompletedItem(raw json.RawMessage) {
	var params struct {
		Item map[string]any `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	itemType, _ := params.Item["type"].(string)
	status, _ := params.Item["status"].(string)
	message := itemType
	eventType := "tool_call"
	switch itemType {
	case "agentMessage":
		eventType = "agent_message"
		message, _ = params.Item["text"].(string)
		if message != "" {
			_ = s.manager.store.AddLog(context.Background(), s.run.ID, "assistant", message)
		}
	case "commandExecution":
		message, _ = params.Item["command"].(string)
	case "fileChange":
		eventType = "file_change"
		message = "File changes applied"
	}
	if status == "" {
		status = "success"
	} else {
		switch status {
		case "inProgress":
			status = "running"
		case "completed":
			status = "success"
		case "failed":
			status = "error"
		case "declined":
			status = "denied"
		}
	}
	_ = s.manager.store.AddTrace(context.Background(), &model.TraceEvent{
		TraceID: s.run.TraceID, RunID: s.run.ID, EventType: eventType, Status: status,
		Message: redact(message), Attributes: params.Item,
	})
}

func (s *codexSession) handleTurnCompleted(raw json.RawMessage) error {
	var params struct {
		Turn struct {
			Status     string `json:"status"`
			DurationMS *int64 `json:"durationMs"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	status := "success"
	exitCode := 0
	errText := ""
	if s.timedOut {
		status, exitCode, errText = "timeout", -1, "approval timed out"
	} else {
		switch params.Turn.Status {
		case "failed":
			status, exitCode = "error", 1
			if params.Turn.Error != nil {
				errText = params.Turn.Error.Message
			}
		case "interrupted":
			status, exitCode = "cancelled", -1
		}
	}
	s.finish(status, exitCode, errText)
	return nil
}

func (s *codexSession) finish(status string, exitCode int, errText string) {
	ended := time.Now().UTC()
	_ = s.manager.store.FinishRun(context.Background(), s.run.ID, status, exitCode, errText, ended)
	duration := ended.Sub(s.started).Milliseconds()
	message := "Codex run completed"
	if status == "error" {
		message = "Codex run failed"
	} else if status == "cancelled" {
		message = "Codex run cancelled"
	} else if status == "timeout" {
		message = "Codex run timed out waiting for approval"
	}
	_ = s.manager.store.AddTrace(context.Background(), &model.TraceEvent{
		TraceID: s.run.TraceID, RunID: s.run.ID, EventType: "agent_run", Status: status,
		Message: message, StartedAt: s.started, EndedAt: &ended, DurationMS: &duration,
		Attributes: map[string]any{"exit_code": exitCode, "error": errText, "thread_id": s.thread},
	})
}

func (s *codexSession) send(id int, method string, params any) error {
	return s.write(map[string]any{"id": id, "method": method, "params": params})
}

func (s *codexSession) notify(method string, params any) error {
	return s.write(map[string]any{"method": method, "params": params})
}

func (s *codexSession) respond(id json.RawMessage, result any) error {
	var rawID any
	if err := json.Unmarshal(id, &rawID); err != nil {
		return err
	}
	return s.write(map[string]any{"id": rawID, "result": result})
}

func (s *codexSession) write(message any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = s.stdin.Write(payload)
	return err
}
