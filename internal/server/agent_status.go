package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/kumagaias/tailagent/internal/model"
)

const agentStatusTimeout = 4 * time.Second
const codexStatusCacheTTL = time.Minute
const codexAuthRequiredCacheTTL = 3 * time.Second

var probeCodexStatusFunc = probeCodexStatus

type codexStatusCache struct {
	workspaceRoot string
	account       *model.AgentAccount
	usage         *model.AgentUsage
	expiresAt     time.Time
}

type appServerProbeMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (s *Server) enrichAgents(ctx context.Context, agents []model.Agent) {
	hasCodex := false
	for _, a := range agents {
		if a.Type == "Codex" {
			hasCodex = true
			break
		}
	}
	if !hasCodex {
		return
	}
	account, usage := s.cachedCodexStatus(ctx)
	for i := range agents {
		if agents[i].Type != "Codex" {
			continue
		}
		agents[i].Account = account
		agents[i].Usage = usage
		if agents[i].Status == "ready" && (usage == nil || usage.RemainingPct == nil) {
			agents[i].Status = "not_ready"
			if agents[i].LastError == "" {
				agents[i].LastError = codexRemainingUnavailableText(usage)
			}
		}
	}
}

func codexRemainingUnavailableText(usage *model.AgentUsage) string {
	if usage != nil && usage.UnavailableText != "" {
		return usage.UnavailableText
	}
	return "Codex remaining unavailable"
}

func (s *Server) cachedCodexStatus(ctx context.Context) (*model.AgentAccount, *model.AgentUsage) {
	workspaceRoot := s.settings.WorkspaceRoot
	now := time.Now()

	s.codexStatusMu.Lock()
	defer s.codexStatusMu.Unlock()

	if s.codexStatusCache.workspaceRoot == workspaceRoot && now.Before(s.codexStatusCache.expiresAt) {
		return cloneAgentAccount(s.codexStatusCache.account), cloneAgentUsage(s.codexStatusCache.usage)
	}

	account, usage := probeCodexStatusFunc(ctx, workspaceRoot)
	s.codexStatusCache = codexStatusCache{
		workspaceRoot: workspaceRoot,
		account:       cloneAgentAccount(account),
		usage:         cloneAgentUsage(usage),
		expiresAt:     now.Add(codexStatusCacheDuration(account, usage)),
	}
	return cloneAgentAccount(account), cloneAgentUsage(usage)
}

func codexStatusCacheDuration(account *model.AgentAccount, usage *model.AgentUsage) time.Duration {
	if account != nil && account.Login == "" {
		return codexAuthRequiredCacheTTL
	}
	if usage != nil && isCodexAuthRequiredText(usage.UnavailableText) {
		return codexAuthRequiredCacheTTL
	}
	return codexStatusCacheTTL
}

func (s *Server) invalidateCodexStatusCache() {
	s.codexStatusMu.Lock()
	s.codexStatusCache = codexStatusCache{}
	s.codexStatusMu.Unlock()
}

func cloneAgentAccount(account *model.AgentAccount) *model.AgentAccount {
	if account == nil {
		return nil
	}
	v := *account
	return &v
}

func cloneAgentUsage(usage *model.AgentUsage) *model.AgentUsage {
	if usage == nil {
		return nil
	}
	v := *usage
	if usage.LifetimeTokens != nil {
		lifetimeTokens := *usage.LifetimeTokens
		v.LifetimeTokens = &lifetimeTokens
	}
	if usage.RemainingPct != nil {
		remainingPct := *usage.RemainingPct
		v.RemainingPct = &remainingPct
	}
	if usage.ResetsAt != nil {
		resetsAt := *usage.ResetsAt
		v.ResetsAt = &resetsAt
	}
	return &v
}

func probeCodexStatus(parent context.Context, dir string) (*model.AgentAccount, *model.AgentUsage) {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Codex command not found"}
	}
	ctx, cancel := context.WithTimeout(parent, agentStatusTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	if dir != "" {
		cmd.Dir = dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Unable to open Codex stdin"}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Unable to open Codex stdout"}
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Unable to start Codex"}
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	var stderrBuf bytes.Buffer
	if stderr != nil {
		go func() { _, _ = io.Copy(&stderrBuf, stderr) }()
	}

	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	if err := sendProbe(enc, 1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "tailagent", "title": "tailagent", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Unable to initialize Codex"}
	}
	if _, err := readProbeResult(scanner, 1); err != nil {
		return nil, &model.AgentUsage{UnavailableText: probeErrorText(err, stderrBuf.String())}
	}
	if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, &model.AgentUsage{UnavailableText: "Unable to finish Codex initialization"}
	}

	account, err := readCodexAccount(enc, scanner)
	if err != nil {
		return nil, &model.AgentUsage{UnavailableText: probeErrorText(err, stderrBuf.String())}
	}
	if account != nil && account.Login == "" {
		return account, &model.AgentUsage{UnavailableText: "Codex login required"}
	}
	usage := &model.AgentUsage{}
	if err := readCodexRateLimits(enc, scanner, usage); err != nil {
		usage.UnavailableText = probeErrorText(err, stderrBuf.String())
	}
	if err := readCodexTokenUsage(enc, scanner, usage); err != nil && usage.UnavailableText == "" {
		usage.UnavailableText = probeErrorText(err, stderrBuf.String())
	}
	if usage.LifetimeTokens == nil && usage.RemainingPct == nil && usage.ResetsAt == nil && usage.UnavailableText == "" {
		usage.UnavailableText = "Usage data unavailable"
	}
	return account, usage
}

func sendProbe(enc *json.Encoder, id int, method string, params any) error {
	return enc.Encode(map[string]any{"id": id, "method": method, "params": params})
}

func readProbeResult(scanner *bufio.Scanner, id int) (json.RawMessage, error) {
	for scanner.Scan() {
		var msg appServerProbeMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		var got int
		if err := json.Unmarshal(msg.ID, &got); err != nil || got != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("Codex app-server error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return msg.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("Codex app-server closed before returning account status")
}

func readCodexAccount(enc *json.Encoder, scanner *bufio.Scanner) (*model.AgentAccount, error) {
	if err := sendProbe(enc, 2, "account/read", map[string]any{"refreshToken": false}); err != nil {
		return nil, err
	}
	raw, err := readProbeResult(scanner, 2)
	if err != nil {
		return nil, err
	}
	var res struct {
		Account *struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			PlanType string `json:"planType"`
		} `json:"account"`
		RequiresOpenAIAuth bool `json:"requiresOpenaiAuth"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Account == nil {
		if res.RequiresOpenAIAuth {
			return &model.AgentAccount{AuthMode: "openai"}, nil
		}
		return nil, nil
	}
	account := &model.AgentAccount{AuthMode: res.Account.Type, Login: res.Account.Email, Plan: res.Account.PlanType}
	if account.Login == "" && account.AuthMode == "apiKey" {
		account.Login = "API key"
	}
	return account, nil
}

func readCodexRateLimits(enc *json.Encoder, scanner *bufio.Scanner, usage *model.AgentUsage) error {
	if err := sendProbe(enc, 3, "account/rateLimits/read", nil); err != nil {
		return err
	}
	raw, err := readProbeResult(scanner, 3)
	if err != nil {
		return err
	}
	var res struct {
		RateLimits rateLimitSnapshot            `json:"rateLimits"`
		ByLimitID  map[string]rateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	snapshot := res.RateLimits
	if codex, ok := res.ByLimitID["codex"]; ok {
		snapshot = codex
	}
	if snapshot.LimitName != "" {
		usage.LimitName = snapshot.LimitName
	}
	if snapshot.IndividualLimit != nil {
		usage.RemainingPct = &snapshot.IndividualLimit.RemainingPercent
		usage.ResetsAt = unixTimePtr(snapshot.IndividualLimit.ResetsAt)
		return nil
	}
	if snapshot.Primary != nil {
		remaining := 100 - snapshot.Primary.UsedPercent
		if remaining < 0 {
			remaining = 0
		}
		usage.RemainingPct = &remaining
		if snapshot.Primary.ResetsAt != nil {
			usage.ResetsAt = unixTimePtr(*snapshot.Primary.ResetsAt)
		}
	}
	return nil
}

func readCodexTokenUsage(enc *json.Encoder, scanner *bufio.Scanner, usage *model.AgentUsage) error {
	if err := sendProbe(enc, 4, "account/usage/read", nil); err != nil {
		return err
	}
	raw, err := readProbeResult(scanner, 4)
	if err != nil {
		return err
	}
	var res struct {
		Summary struct {
			LifetimeTokens *int64 `json:"lifetimeTokens"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	usage.LifetimeTokens = res.Summary.LifetimeTokens
	return nil
}

type rateLimitSnapshot struct {
	LimitName       string `json:"limitName"`
	IndividualLimit *struct {
		RemainingPercent int   `json:"remainingPercent"`
		ResetsAt         int64 `json:"resetsAt"`
	} `json:"individualLimit"`
	Primary *struct {
		UsedPercent int    `json:"usedPercent"`
		ResetsAt    *int64 `json:"resetsAt"`
	} `json:"primary"`
}

func unixTimePtr(ts int64) *time.Time {
	if ts <= 0 {
		return nil
	}
	t := time.Unix(ts, 0).UTC()
	return &t
}

func probeErrorText(err error, stderr string) string {
	if stderr != "" {
		return "Codex status unavailable: " + firstLine(stderr)
	}
	if err != nil {
		return "Codex status unavailable: " + err.Error()
	}
	return "Codex status unavailable"
}

func isCodexAuthRequiredText(s string) bool {
	return strings.Contains(strings.ToLower(s), "authentication required")
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}
