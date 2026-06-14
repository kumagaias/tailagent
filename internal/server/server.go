package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kumagaias/tailagent/internal/model"
	runmanager "github.com/kumagaias/tailagent/internal/run"
	storesqlite "github.com/kumagaias/tailagent/internal/storage/sqlite"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	store    *storesqlite.Store
	runs     *runmanager.Manager
	settings model.Settings
	mux      *http.ServeMux

	codexStatusMu    sync.Mutex
	codexStatusCache codexStatusCache
	codexAuthMu      sync.Mutex
	codexLogin       *codexLoginSession
}

func New(store *storesqlite.Store, settings model.Settings) *Server {
	s := &Server{store: store, settings: settings, mux: http.NewServeMux()}
	s.runs = runmanager.NewManager(store, settings.MaxConcurrentAgents, settings.ApprovalTimeoutSecs)
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/dashboard", s.dashboard)
	s.mux.HandleFunc("POST /api/system/select-folder", s.selectFolder)
	s.mux.HandleFunc("GET /api/agents", s.agents)
	s.mux.HandleFunc("POST /api/agents", s.agents)
	s.mux.HandleFunc("PUT /api/agents/{id}", s.agents)
	s.mux.HandleFunc("DELETE /api/agents/{id}", s.agents)
	s.mux.HandleFunc("POST /api/agents/{id}/login", s.agentLogin)
	s.mux.HandleFunc("POST /api/agents/{id}/logout", s.agentLogout)
	s.mux.HandleFunc("GET /api/projects", s.projects)
	s.mux.HandleFunc("POST /api/projects", s.projects)
	s.mux.HandleFunc("PUT /api/projects/{id}", s.projects)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.projects)
	s.mux.HandleFunc("GET /api/milestones", s.milestones)
	s.mux.HandleFunc("POST /api/milestones", s.milestones)
	s.mux.HandleFunc("PUT /api/milestones/{id}", s.milestones)
	s.mux.HandleFunc("GET /api/tasks", s.tasks)
	s.mux.HandleFunc("POST /api/tasks", s.tasks)
	s.mux.HandleFunc("PUT /api/tasks/{id}", s.tasks)
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.tasks)
	s.mux.HandleFunc("GET /api/tasks/{id}/image", s.taskImage)
	s.mux.HandleFunc("PUT /api/tasks/{id}/image", s.taskImage)
	s.mux.HandleFunc("DELETE /api/tasks/{id}/image", s.taskImage)
	s.mux.HandleFunc("GET /api/tasks/{id}/images/{imageID}", s.taskImageByID)
	s.mux.HandleFunc("DELETE /api/tasks/{id}/images/{imageID}", s.taskImageByID)
	s.mux.HandleFunc("GET /api/runs", s.runsAPI)
	s.mux.HandleFunc("POST /api/runs", s.runsAPI)
	s.mux.HandleFunc("POST /api/runs/{id}/stop", s.stopRun)
	s.mux.HandleFunc("GET /api/runs/{id}/logs", s.logs)
	s.mux.HandleFunc("GET /api/approvals", s.approvals)
	s.mux.HandleFunc("POST /api/approvals", s.approvals)
	s.mux.HandleFunc("POST /api/approvals/{id}/decision", s.decide)
	s.mux.HandleFunc("GET /api/chats", s.chats)
	s.mux.HandleFunc("POST /api/chats", s.chats)
	s.mux.HandleFunc("GET /api/chats/{id}/messages", s.chatMessages)
	s.mux.HandleFunc("POST /api/chats/{id}/messages", s.chatMessages)
	s.mux.HandleFunc("POST /api/chats/{id}/tasks", s.confirmChatTasks)
	s.mux.HandleFunc("GET /api/traces", s.traces)
	s.mux.HandleFunc("GET /api/settings", s.settingsAPI)
	s.mux.HandleFunc("PUT /api/settings", s.settingsAPI)
	sub, _ := fs.Sub(webFiles, "web")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

func (s *Server) selectFolder(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "darwin" {
		writeError(w, http.StatusNotImplemented, errors.New("folder selection is currently supported on macOS"))
		return
	}
	out, err := exec.CommandContext(
		r.Context(),
		"osascript",
		"-e",
		`POSIX path of (choose folder with prompt "Select a project folder")`,
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Errorf("open folder picker: %w", err))
		return
	}
	path := strings.TrimSuffix(strings.TrimSpace(string(out)), "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path,
		"name": filepath.Base(path),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func idParam(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }
func queryID(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.Dashboard(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListAgents(r.Context())
		if err != nil {
			writeError(w, 500, err)
			return
		}
		s.enrichAgents(r.Context(), v)
		writeJSON(w, 200, v)
		return
	}
	if r.Method == "DELETE" {
		id, err := idParam(r)
		if err == nil {
			err = s.store.DeleteAgent(r.Context(), id)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var v model.Agent
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	if r.Method == "PUT" {
		id, err := idParam(r)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		v.ID = id
	}
	if err := s.store.SaveAgent(r.Context(), &v); err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListProjects(r.Context())
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if r.Method == "DELETE" {
		id, err := idParam(r)
		if err == nil {
			err = s.store.DeleteProject(r.Context(), id)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var v model.Project
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	if r.Method == "PUT" {
		id, err := idParam(r)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		v.ID = id
	}
	if err := s.store.SaveProject(r.Context(), &v); err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) milestones(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListMilestones(r.Context(), queryID(r, "project_id"))
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var v model.Milestone
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	if r.Method == "PUT" {
		id, err := idParam(r)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		v.ID = id
	}
	if err := s.store.SaveMilestone(r.Context(), &v); err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListTasks(r.Context(), queryID(r, "project_id"), queryID(r, "milestone_id"))
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if r.Method == "DELETE" {
		id, err := idParam(r)
		if err == nil {
			err = s.store.DeleteTask(r.Context(), id)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var v model.Task
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	if r.Method == "PUT" {
		id, err := idParam(r)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		v.ID = id
	}
	if err := s.store.SaveTask(r.Context(), &v); err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, v)
}

const (
	maxTaskImageSize  = 20 << 20
	maxTaskImageCount = 5
)

var allowedTaskImageTypes = map[string]bool{
	"image/gif":  true,
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

func (s *Server) taskImage(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		image, err := s.store.GetTaskImage(r.Context(), id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeTaskImage(w, image)
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, maxTaskImageSize*maxTaskImageCount+(1<<20))
		if err := r.ParseMultipartForm(maxTaskImageSize * maxTaskImageCount); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("images must be multipart uploads no larger than 20 MiB each"))
			return
		}
		headers := append(r.MultipartForm.File["images"], r.MultipartForm.File["image"]...)
		if len(headers) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("at least one image is required"))
			return
		}
		if len(headers) > maxTaskImageCount {
			writeError(w, http.StatusBadRequest, errors.New("a task can have at most 5 images"))
			return
		}
		images := make([]model.TaskImage, 0, len(headers))
		for _, header := range headers {
			file, err := header.Open()
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			data, readErr := io.ReadAll(io.LimitReader(file, maxTaskImageSize+1))
			closeErr := file.Close()
			if readErr != nil {
				writeError(w, http.StatusBadRequest, readErr)
				return
			}
			if closeErr != nil {
				writeError(w, http.StatusBadRequest, closeErr)
				return
			}
			if len(data) == 0 || len(data) > maxTaskImageSize {
				writeError(w, http.StatusBadRequest, errors.New("each image must be between 1 byte and 20 MiB"))
				return
			}
			contentType := http.DetectContentType(data)
			if !allowedTaskImageTypes[contentType] {
				writeError(w, http.StatusBadRequest, errors.New("images must be PNG, JPEG, GIF, or WebP"))
				return
			}
			name := filepath.Base(header.Filename)
			if name == "." || name == "" {
				name = "attachment"
			}
			images = append(images, model.TaskImage{
				TaskID: id, Name: name, ContentType: contentType, Data: data,
			})
		}
		if err := s.store.SaveTaskImages(r.Context(), images); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		metadata, err := s.store.ListTaskImages(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]model.TaskImageMeta, len(metadata))
		for i, image := range metadata {
			out[i] = model.TaskImageMeta{
				ID: image.ID, Name: image.Name, ContentType: image.ContentType, Size: image.Size,
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		if err := s.store.DeleteTaskImage(r.Context(), id); err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) taskImageByID(w http.ResponseWriter, r *http.Request) {
	taskID, err := idParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	imageID, err := strconv.ParseInt(r.PathValue("imageID"), 10, 64)
	if err != nil || imageID < 1 {
		writeError(w, http.StatusBadRequest, errors.New("invalid image id"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		image, err := s.store.GetTaskImageByID(r.Context(), taskID, imageID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeTaskImage(w, image)
	case http.MethodDelete:
		if err := s.store.DeleteTaskImageByID(r.Context(), taskID, imageID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusBadRequest, err)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeTaskImage(w http.ResponseWriter, image model.TaskImage) {
	w.Header().Set("Content-Type", image.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(image.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": image.Name}))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.Data)
}
func (s *Server) runsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListRuns(r.Context(), 250)
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var req struct {
		TaskID      int64  `json:"task_id"`
		AgentID     int64  `json:"agent_id"`
		Instruction string `json:"instruction"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	v, err := s.runs.Start(r.Context(), req.TaskID, req.AgentID, req.Instruction)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 202, v)
}
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err == nil {
		err = s.runs.Stop(id)
	}
	if err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"stopped": true})
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	v, err := s.store.Logs(r.Context(), id)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListApprovals(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var v model.Approval
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	if v.Risk == "" {
		v.Risk = "medium"
	}
	if v.ExpiresAt.IsZero() {
		v.ExpiresAt = time.Now().UTC().Add(time.Duration(s.settings.ApprovalTimeoutSecs) * time.Second)
	}
	run, err := s.store.GetRun(r.Context(), v.RunID)
	if err != nil {
		writeError(w, 400, errors.New("invalid run_id"))
		return
	}
	v.AgentID, v.ProjectID, v.TaskID = run.AgentID, run.ProjectID, run.TaskID
	if err = s.store.CreateApproval(r.Context(), &v); err != nil {
		writeError(w, 400, err)
		return
	}
	_ = s.store.SetRunStatus(r.Context(), run.ID, "waiting_approval")
	_ = s.store.AddTrace(r.Context(), &model.TraceEvent{TraceID: run.TraceID, RunID: run.ID, EventType: "approval_request", Status: "waiting_approval", Message: v.Operation, Attributes: map[string]any{"approval_id": v.ID, "risk": v.Risk}})
	writeJSON(w, 201, v)
}
func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	var req struct {
		Decision string `json:"decision"`
	}
	if err = decode(r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Decision != "allowed" && req.Decision != "denied" {
		writeError(w, 400, errors.New("decision must be allowed or denied"))
		return
	}
	current, err := s.findApproval(r.Context(), id)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if current.Status != "pending" {
		writeError(w, 400, errors.New("approval is not pending"))
		return
	}
	if current.AgentType == "Codex" {
		if err := s.runs.ResolveApproval(id, req.Decision); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	v, err := s.store.DecideApproval(r.Context(), id, req.Decision)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	run, _ := s.store.GetRun(r.Context(), v.RunID)
	_ = s.store.SetRunStatus(r.Context(), v.RunID, "running")
	_ = s.store.AddTrace(r.Context(), &model.TraceEvent{TraceID: run.TraceID, RunID: v.RunID, EventType: "approval_decision", Status: req.Decision, Message: "Approval " + req.Decision, Attributes: map[string]any{"approval_id": id}})
	writeJSON(w, 200, v)
}

func (s *Server) chats(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.ListChats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	var v model.Chat
	if err := decode(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.CreateChat(r.Context(), &v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request) {
	chatID, err := idParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if r.Method == "GET" {
		v, err := s.store.ListChatMessages(r.Context(), chatID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := s.attachChatTaskProposals(r.Context(), v); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	var req struct {
		Content string `json:"content"`
		AgentID int64  `json:"agent_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	chat, err := s.store.GetChat(r.Context(), chatID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agentID := chat.AgentID
	if req.AgentID != 0 {
		agentID = req.AgentID
		if req.AgentID != chat.AgentID {
			if err := s.store.UpdateChatAgent(r.Context(), chatID, req.AgentID); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
	}
	msg, err := s.store.AddChatMessage(r.Context(), chatID, nil, req.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	history, err := s.chatTranscript(r.Context(), chatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	run, err := s.runs.Start(r.Context(), chat.TaskID, agentID, history)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.AttachChatMessageRun(r.Context(), msg.ID, run.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	messages, err := s.store.ListChatMessages(r.Context(), chatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.attachChatTaskProposals(r.Context(), messages); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, messages)
}

func (s *Server) chatTranscript(ctx context.Context, chatID int64) (string, error) {
	chat, err := s.store.GetChat(ctx, chatID)
	if err != nil {
		return "", err
	}
	messages, err := s.store.ListChatMessages(ctx, chatID)
	if err != nil {
		return "", err
	}
	milestones, err := s.store.ListMilestones(ctx, chat.ProjectID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Continue this chat using the full task context above.

This chat belongs to project %q (project_id=%d).
When the user asks to create one or more tasks, do not modify project files and do not claim the tasks were created.
Instead, explain the proposal briefly and finish with exactly one fenced JSON block in this format:

`+"```tailagent_tasks"+`
{"tasks":[{"title":"Required","description":"","milestone_id":0,"instruction":"","acceptance_criteria":""}]}
`+"```"+`

Use milestone_id 0 for no milestone. Available non-default milestones:
`, chat.ProjectName, chat.ProjectID)
	for _, milestone := range milestones {
		if !milestone.IsDefaultNone {
			fmt.Fprintf(&b, "- %d: %s\n", milestone.ID, milestone.Name)
		}
	}
	b.WriteString("\nChat history:\n")
	for _, msg := range messages {
		b.WriteString("\nUser:\n")
		b.WriteString(msg.Content)
		b.WriteByte('\n')
		if len(msg.Logs) == 0 {
			continue
		}
		reply := chatReplyLines(msg.Logs, 40)
		if reply != "" {
			b.WriteString("\nAssistant:\n")
			b.WriteString(reply)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func (s *Server) attachChatTaskProposals(ctx context.Context, messages []model.ChatMessage) error {
	for i := range messages {
		if tasks, ok := parseChatTaskProposal(messages[i].Logs); ok {
			if err := s.store.SaveChatTaskProposal(ctx, messages[i].ID, tasks); err != nil {
				return err
			}
		}
		proposal, err := s.store.GetChatTaskProposal(ctx, messages[i].ID)
		if err != nil {
			return err
		}
		messages[i].TaskProposal = proposal
	}
	return nil
}

func parseChatTaskProposal(logs []model.RunLog) ([]model.TaskProposalItem, bool) {
	reply := chatReplyLines(logs, 400)
	const marker = "```tailagent_tasks"
	start := strings.LastIndex(reply, marker)
	if start < 0 {
		return nil, false
	}
	body := reply[start+len(marker):]
	end := strings.Index(body, "```")
	if end < 0 {
		return nil, false
	}
	var payload struct {
		Tasks []model.TaskProposalItem `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body[:end])), &payload); err != nil || len(payload.Tasks) == 0 || len(payload.Tasks) > 20 {
		return nil, false
	}
	for i := range payload.Tasks {
		payload.Tasks[i].Title = strings.TrimSpace(payload.Tasks[i].Title)
		if payload.Tasks[i].Title == "" {
			return nil, false
		}
	}
	return payload.Tasks, true
}

func (s *Server) confirmChatTasks(w http.ResponseWriter, r *http.Request) {
	chatID, err := idParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		MessageID int64 `json:"message_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.MessageID == 0 {
		writeError(w, http.StatusBadRequest, errors.New("message_id is required"))
		return
	}
	tasks, err := s.store.ConfirmChatTaskProposal(r.Context(), chatID, req.MessageID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, tasks)
}

func chatReplyLines(logs []model.RunLog, max int) string {
	var assistant []model.RunLog
	for _, log := range logs {
		if log.Stream == "assistant" {
			assistant = append(assistant, log)
		}
	}
	if len(assistant) > 0 {
		return latestLogLines(assistant, max)
	}
	return latestLogLines(logs, max)
}

func latestLogLines(logs []model.RunLog, max int) string {
	var lines []string
	for _, log := range logs {
		for _, line := range strings.Split(log.Content, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	if max > 0 && len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return strings.Join(lines, "\n")
}

func (s *Server) findApproval(ctx context.Context, id int64) (model.Approval, error) {
	items, err := s.store.ListApprovals(ctx, "")
	if err != nil {
		return model.Approval{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return model.Approval{}, sql.ErrNoRows
}
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.ListTraces(r.Context(), queryID(r, "project_id"), queryID(r, "agent_id"), r.URL.Query().Get("status"), r.URL.Query().Get("event_type"), r.URL.Query().Get("search"), 500)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) settingsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, err := s.store.Settings(r.Context(), s.settings)
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var v model.Settings
	if err := decode(r, &v); err != nil {
		writeError(w, 400, err)
		return
	}
	v.DatabasePath = s.store.DBPath()
	if v.MaxConcurrentAgents < 1 || v.ApprovalTimeoutSecs < 1 {
		writeError(w, 400, errors.New("timeout and max concurrency must be positive"))
		return
	}
	if err := s.store.SaveSettings(r.Context(), v); err != nil {
		writeError(w, 500, err)
		return
	}
	s.settings = v
	s.invalidateCodexStatusCache()
	writeJSON(w, 200, v)
}

func (s *Server) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, err := s.store.ExpireApprovals(ctx)
			if err != nil {
				slog.Error("expire approvals", "error", err)
				continue
			}
			for _, a := range expired {
				_ = s.runs.ResolveApproval(a.ID, "expired")
				run, err := s.store.GetRun(ctx, a.RunID)
				if err != nil {
					continue
				}
				_ = s.store.SetRunStatus(ctx, a.RunID, "timeout")
				_ = s.store.AddTrace(ctx, &model.TraceEvent{TraceID: run.TraceID, RunID: a.RunID, EventType: "approval_decision", Status: "timeout", Message: "Approval timed out and was denied", Attributes: map[string]any{"approval_id": a.ID}})
			}
			if err := s.cleanupStaleWaitingApprovalRuns(ctx, 5*time.Second); err != nil {
				slog.Error("cleanup stale waiting approvals", "error", err)
			}
		}
	}
}

func (s *Server) cleanupStaleWaitingApprovalRuns(ctx context.Context, grace time.Duration) error {
	runs, err := s.store.ListRuns(ctx, 500)
	if err != nil {
		return err
	}
	approvals, err := s.store.ListApprovals(ctx, "")
	if err != nil {
		return err
	}
	pendingByRun := map[int64]bool{}
	latestDecisionByRun := map[int64]time.Time{}
	for _, approval := range approvals {
		if approval.Status == "pending" {
			pendingByRun[approval.RunID] = true
			continue
		}
		if approval.DecidedAt == nil {
			continue
		}
		if latestDecisionByRun[approval.RunID].Before(*approval.DecidedAt) {
			latestDecisionByRun[approval.RunID] = *approval.DecidedAt
		}
	}
	now := time.Now().UTC()
	for _, run := range runs {
		if run.Status != "waiting_approval" || pendingByRun[run.ID] {
			continue
		}
		lastActivity := run.CreatedAt
		if run.StartedAt != nil {
			lastActivity = *run.StartedAt
		}
		if decidedAt := latestDecisionByRun[run.ID]; !decidedAt.IsZero() {
			lastActivity = decidedAt
		}
		if grace > 0 && now.Sub(lastActivity) < grace {
			continue
		}
		if err := s.store.SetRunStatus(ctx, run.ID, "timeout"); err != nil {
			return err
		}
		_ = s.store.AddTrace(ctx, &model.TraceEvent{
			TraceID: run.TraceID, RunID: run.ID, EventType: "approval_decision", Status: "timeout",
			Message:    "Run was waiting for approval, but no pending approval exists",
			Attributes: map[string]any{"stale": true},
		})
	}
	return nil
}

func ListenAddress(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }
func ParsePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 8787, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return v, nil
}
