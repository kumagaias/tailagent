package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"
)

const codexLoginTimeout = 10 * time.Minute

var startCodexLoginFunc = (*Server).startCodexLogin
var logoutCodexFunc = logoutCodex
var openLoginURLFunc = openURLInBrowser

type codexLoginSession struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	loginID  string
	authURL  string
	cancel   context.CancelFunc
	finished chan struct{}
}

func (s *Server) agentLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.requireCodexAgent(r.Context(), r); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	authURL, err := startCodexLoginFunc(s)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errCodexLoginInProgress) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	browserOpened := openLoginURLFunc(authURL) == nil
	writeJSON(w, http.StatusOK, map[string]any{"auth_url": authURL, "browser_opened": browserOpened})
}

func (s *Server) agentLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.requireCodexAgent(r.Context(), r); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.cancelCodexLogin()
	if err := logoutCodexFunc(r.Context(), s.settings.WorkspaceRoot); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.invalidateCodexStatusCache()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireCodexAgent(ctx context.Context, r *http.Request) error {
	id, err := idParam(r)
	if err != nil {
		return errors.New("invalid agent ID")
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if agent.ID != id {
			continue
		}
		if agent.Type != "Codex" {
			return fmt.Errorf("%s login is not supported", agent.Type)
		}
		return nil
	}
	return errors.New("agent not found")
}

var errCodexLoginInProgress = errors.New("Codex login is already in progress")

func (s *Server) startCodexLogin() (string, error) {
	s.codexAuthMu.Lock()
	if s.codexLogin != nil {
		session := s.codexLogin
		s.codexLogin = nil
		s.codexAuthMu.Unlock()
		session.cancel()
		waitForCodexLoginSession(session)
		s.codexAuthMu.Lock()
		if s.codexLogin != nil {
			s.codexAuthMu.Unlock()
			return "", errCodexLoginInProgress
		}
	}
	defer s.codexAuthMu.Unlock()
	if _, err := exec.LookPath("codex"); err != nil {
		return "", errors.New("Codex command not found")
	}
	s.invalidateCodexStatusCache()

	ctx, cancel := context.WithTimeout(context.Background(), codexLoginTimeout)
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	if s.settings.WorkspaceRoot != "" {
		cmd.Dir = s.settings.WorkspaceRoot
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("open Codex stdout: %w", err)
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start Codex login: %w", err)
	}

	var stderrBuf bytes.Buffer
	if stderr != nil {
		go func() { _, _ = io.Copy(&stderrBuf, stderr) }()
	}
	fail := func(err error) (string, error) {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
		return "", fmt.Errorf("start Codex login: %s", probeErrorText(err, stderrBuf.String()))
	}

	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	if err := sendProbe(enc, 1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "tailagent", "title": "tailagent", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return fail(err)
	}
	if _, err := readProbeResult(scanner, 1); err != nil {
		return fail(err)
	}
	if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return fail(err)
	}
	if err := sendProbe(enc, 2, "account/login/start", map[string]any{"type": "chatgpt"}); err != nil {
		return fail(err)
	}
	raw, err := readProbeResult(scanner, 2)
	if err != nil {
		return fail(err)
	}
	var response struct {
		Type    string `json:"type"`
		AuthURL string `json:"authUrl"`
		LoginID string `json:"loginId"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fail(err)
	}
	if response.Type != "chatgpt" || response.AuthURL == "" || response.LoginID == "" {
		return fail(errors.New("Codex returned an invalid login response"))
	}

	session := &codexLoginSession{
		cmd: cmd, stdin: stdin, loginID: response.LoginID, authURL: response.AuthURL,
		cancel: cancel, finished: make(chan struct{}),
	}
	s.codexLogin = session
	go s.waitForCodexLogin(session, scanner)
	return response.AuthURL, nil
}

func (s *Server) waitForCodexLogin(session *codexLoginSession, scanner *bufio.Scanner) {
	defer func() {
		_ = session.stdin.Close()
		session.cancel()
		_ = session.cmd.Wait()
		close(session.finished)
		s.codexAuthMu.Lock()
		if s.codexLogin == session {
			s.codexLogin = nil
		}
		s.codexAuthMu.Unlock()
		s.invalidateCodexStatusCache()
	}()

	for scanner.Scan() {
		var message struct {
			Method string `json:"method"`
			Params struct {
				LoginID string `json:"loginId"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) == nil &&
			message.Method == "account/login/completed" &&
			(message.Params.LoginID == "" || message.Params.LoginID == session.loginID) {
			return
		}
	}
}

func (s *Server) cancelCodexLogin() {
	s.codexAuthMu.Lock()
	session := s.codexLogin
	s.codexLogin = nil
	s.codexAuthMu.Unlock()
	if session == nil {
		return
	}
	session.cancel()
	waitForCodexLoginSession(session)
}

func waitForCodexLoginSession(session *codexLoginSession) {
	select {
	case <-session.finished:
	case <-time.After(time.Second):
	}
}

func logoutCodex(ctx context.Context, dir string) error {
	if _, err := exec.LookPath("codex"); err != nil {
		return errors.New("Codex command not found")
	}
	ctx, cancel := context.WithTimeout(ctx, agentStatusTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	if dir != "" {
		cmd.Dir = dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex logout: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	if err := sendProbe(enc, 1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "tailagent", "title": "tailagent", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return err
	}
	if _, err := readProbeResult(scanner, 1); err != nil {
		return errors.New(probeErrorText(err, stderr.String()))
	}
	if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	if err := sendProbe(enc, 2, "account/logout", nil); err != nil {
		return err
	}
	if _, err := readProbeResult(scanner, 2); err != nil {
		return errors.New(probeErrorText(err, stderr.String()))
	}
	return nil
}
