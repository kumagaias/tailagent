package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kumagaias/tailagent/internal/agent"
	"github.com/kumagaias/tailagent/internal/model"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

type nullableTime struct {
	Time  time.Time
	Valid bool
}

func (t *nullableTime) Scan(value any) error {
	if value == nil {
		t.Time = time.Time{}
		t.Valid = false
		return nil
	}
	switch value := value.(type) {
	case time.Time:
		t.Time = value
		t.Valid = true
		return nil
	case string:
		return t.scanString(value)
	case []byte:
		return t.scanString(string(value))
	default:
		return fmt.Errorf("unsupported time value type %T", value)
	}
}

func (t *nullableTime) scanString(value string) error {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			t.Time = parsed
			t.Valid = true
			return nil
		}
	}
	return fmt.Errorf("unsupported time format %q", value)
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error   { return s.db.Close() }
func (s *Store) DBPath() string { return s.path }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("projects", "prefix", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("tasks", "project_number", "INTEGER"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("tasks", "label", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("tasks", "label_auto", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("agents", "command_path", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.migrateAgentTypes(); err != nil {
		return err
	}
	if err := s.migrateKiroCommandPath(); err != nil {
		return err
	}
	if err := s.migrateTaskImages(); err != nil {
		return err
	}
	if err := s.migrateTaskDisplayIDs(); err != nil {
		return err
	}
	_, err := s.db.Exec(`
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_prefix
 ON projects(prefix COLLATE NOCASE) WHERE prefix IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_number
 ON tasks(project_id, project_number) WHERE project_number IS NOT NULL;
UPDATE tasks SET status='agent_done' WHERE status='review';
UPDATE tasks SET status='closed' WHERE status='done';
`)
	return err
}

func (s *Store) migrateTaskImages() error {
	rows, err := s.db.Query(`PRAGMA table_info(task_images)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hasID bool
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		hasID = hasID || name == "id"
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasID {
		_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_task_images_task ON task_images(task_id, position, id)`)
		return err
	}
	_, err = s.db.Exec(`
ALTER TABLE task_images RENAME TO task_images_single;
CREATE TABLE task_images (
 id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 name TEXT NOT NULL, content_type TEXT NOT NULL, size INTEGER NOT NULL,
 data BLOB NOT NULL, position INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
INSERT INTO task_images(id,task_id,name,content_type,size,data,position,created_at,updated_at)
 SELECT task_id,task_id,name,content_type,size,data,0,created_at,updated_at FROM task_images_single;
DROP TABLE task_images_single;
CREATE INDEX idx_task_images_task ON task_images(task_id, position, id);
`)
	return err
}

func (s *Store) migrateTaskDisplayIDs() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id,name,COALESCE(prefix,'') FROM projects ORDER BY id`)
	if err != nil {
		return err
	}
	type project struct {
		id     int64
		name   string
		prefix string
	}
	var projects []project
	usedPrefixes := make(map[string]bool)
	for rows.Next() {
		var p project
		if err := rows.Scan(&p.id, &p.name, &p.prefix); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, p)
		if p.prefix != "" {
			usedPrefixes[strings.ToUpper(p.prefix)] = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for i := range projects {
		p := &projects[i]
		if p.prefix == "" {
			p.prefix = availableProjectPrefix(p.name, p.id, usedPrefixes)
			if _, err := tx.Exec(`UPDATE projects SET prefix=? WHERE id=?`, p.prefix, p.id); err != nil {
				return err
			}
			usedPrefixes[p.prefix] = true
		}

		var next int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(project_number),0)+1 FROM tasks WHERE project_id=?`, p.id).Scan(&next); err != nil {
			return err
		}
		taskRows, err := tx.Query(`SELECT id FROM tasks WHERE project_id=? AND project_number IS NULL ORDER BY id`, p.id)
		if err != nil {
			return err
		}
		var taskIDs []int64
		for taskRows.Next() {
			var taskID int64
			if err := taskRows.Scan(&taskID); err != nil {
				taskRows.Close()
				return err
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err := taskRows.Close(); err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			if _, err := tx.Exec(`UPDATE tasks SET project_number=? WHERE id=?`, next, taskID); err != nil {
				return err
			}
			next++
		}
	}
	return tx.Commit()
}

func availableProjectPrefix(name string, projectID int64, used map[string]bool) string {
	base := defaultProjectPrefix(name)
	if len(base) < 2 {
		base += "P"
	}
	if len(base) < 2 {
		base = "P" + strings.ToUpper(strconv.FormatInt(projectID, 36))
	}
	if len(base) > 5 {
		base = base[:5]
	}
	if !used[base] {
		return base
	}
	for suffixNumber := int64(2); ; suffixNumber++ {
		suffix := strings.ToUpper(strconv.FormatInt(suffixNumber, 36))
		prefixLength := 5 - len(suffix)
		if prefixLength < 1 {
			prefixLength = 1
		}
		prefix := base
		if len(prefix) > prefixLength {
			prefix = prefix[:prefixLength]
		}
		candidate := prefix + suffix
		if !used[candidate] {
			return candidate
		}
	}
}

func (s *Store) addColumnIfMissing(table, column, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func (s *Store) migrateAgentTypes() error {
	var sqlText string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='agents'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "'Copilot'") {
		return nil
	}
	_, err := s.db.Exec(`
PRAGMA foreign_keys=OFF;
CREATE TABLE agents_new (
 id INTEGER PRIMARY KEY, type TEXT NOT NULL CHECK(type IN ('Codex','Claude','Kiro','Copilot')),
 command_path TEXT NOT NULL DEFAULT '',
 instruction TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'unavailable',
 capabilities_json TEXT NOT NULL DEFAULT '[]', last_error TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
INSERT INTO agents_new(id,type,command_path,instruction,status,capabilities_json,last_error,created_at,updated_at)
 SELECT id,type,command_path,instruction,status,capabilities_json,last_error,created_at,updated_at FROM agents;
DROP TABLE agents;
ALTER TABLE agents_new RENAME TO agents;
PRAGMA foreign_keys=ON;
`)
	return err
}

func (s *Store) migrateKiroCommandPath() error {
	_, err := s.db.Exec(`UPDATE agents SET command_path='kiro-cli' WHERE type='Kiro' AND command_path='kiro'`)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS agents (
 id INTEGER PRIMARY KEY, type TEXT NOT NULL CHECK(type IN ('Codex','Claude','Kiro','Copilot')),
 command_path TEXT NOT NULL DEFAULT '',
 instruction TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'unavailable',
 capabilities_json TEXT NOT NULL DEFAULT '[]', last_error TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS projects (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, folder_path TEXT NOT NULL UNIQUE,
 git_root TEXT NOT NULL DEFAULT '', git_remote TEXT NOT NULL DEFAULT '',
 default_milestone_id INTEGER, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS milestones (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'open',
 target_date TEXT, is_default_none INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(project_id, name)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_milestones_default
 ON milestones(project_id) WHERE is_default_none = 1;
CREATE TABLE IF NOT EXISTS tasks (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 milestone_id INTEGER NOT NULL REFERENCES milestones(id), title TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'todo',
 label TEXT NOT NULL DEFAULT '', label_auto INTEGER NOT NULL DEFAULT 1,
 assigned_agent_id INTEGER REFERENCES agents(id) ON DELETE SET NULL,
 instruction TEXT NOT NULL DEFAULT '', acceptance_criteria TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS task_images (
 id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 name TEXT NOT NULL, content_type TEXT NOT NULL, size INTEGER NOT NULL,
 data BLOB NOT NULL, position INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id),
 milestone_id INTEGER NOT NULL REFERENCES milestones(id), task_id INTEGER NOT NULL REFERENCES tasks(id),
 agent_id INTEGER NOT NULL REFERENCES agents(id), status TEXT NOT NULL, instruction TEXT NOT NULL,
 working_directory TEXT NOT NULL, process_id INTEGER, exit_code INTEGER, started_at DATETIME,
 ended_at DATETIME, trace_id TEXT NOT NULL UNIQUE, error TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS approvals (
 id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 agent_id INTEGER NOT NULL REFERENCES agents(id), project_id INTEGER NOT NULL REFERENCES projects(id),
 task_id INTEGER NOT NULL REFERENCES tasks(id), request_type TEXT NOT NULL, operation TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT '', risk TEXT NOT NULL DEFAULT 'medium', status TEXT NOT NULL DEFAULT 'pending',
 decision TEXT NOT NULL DEFAULT '', requested_at DATETIME NOT NULL, decided_at DATETIME, expires_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS trace_events (
 id INTEGER PRIMARY KEY, trace_id TEXT NOT NULL, run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 parent_event_id INTEGER REFERENCES trace_events(id), event_type TEXT NOT NULL, status TEXT NOT NULL,
 message TEXT NOT NULL, attributes_json TEXT NOT NULL DEFAULT '{}', started_at DATETIME NOT NULL,
 ended_at DATETIME, duration_ms INTEGER, created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS run_logs (
 id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 stream TEXT NOT NULL, sequence INTEGER NOT NULL, content TEXT NOT NULL, created_at DATETIME NOT NULL,
 UNIQUE(run_id, stream, sequence)
);
CREATE TABLE IF NOT EXISTS chats (
 id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 title TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_messages (
 id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
 run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
 role TEXT NOT NULL CHECK(role IN ('user')),
 content TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_task_proposals (
 message_id INTEGER PRIMARY KEY REFERENCES chat_messages(id) ON DELETE CASCADE,
 tasks_json TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
 created_task_ids_json TEXT NOT NULL DEFAULT '[]',
 created_at DATETIME NOT NULL, confirmed_at DATETIME
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id, milestone_id, status);
CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_trace_time ON trace_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_chats_updated ON chats(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_chat_messages_chat ON chat_messages(chat_id, created_at);
`

func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func capabilities(agentType string) []string {
	base := []string{"streaming", "send_message"}
	if agentType == "Codex" || agentType == "Claude" || agentType == "Copilot" {
		return append(base, "approval", "tool_events")
	}
	return base
}

func normalizeCapabilities(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, cap := range caps {
		cap = strings.TrimPrefix(cap, "supports_")
		cap = strings.TrimPrefix(cap, "supported_")
		out = append(out, cap)
	}
	return out
}

func validateAgent(agentType, commandPath string) (string, string) {
	command := commandPath
	if command == "" {
		command = agent.DefaultCommand(agentType)
	}
	if command == "" {
		return "unavailable", "unsupported agent type"
	}
	if _, err := exec.LookPath(command); err != nil {
		return "unavailable", fmt.Sprintf("%s command was not found", command)
	}
	return "ready", ""
}

func (s *Store) ListAgents(ctx context.Context) ([]model.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id,a.type,a.command_path,a.instruction,a.status,a.capabilities_json,a.last_error,a.created_at,a.updated_at,
       (SELECT MAX(started_at) FROM runs WHERE agent_id=a.id),
       COALESCE((SELECT t.title FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.agent_id=a.id AND r.status IN ('queued','starting','running','waiting_approval') ORDER BY r.id DESC LIMIT 1),'')
FROM agents a ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Agent
	for rows.Next() {
		var a model.Agent
		var caps string
		var last nullableTime
		if err := rows.Scan(&a.ID, &a.Type, &a.CommandPath, &a.Instruction, &a.Status, &caps, &a.LastError, &a.CreatedAt, &a.UpdatedAt, &last, &a.ActiveTask); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(caps), &a.Capabilities)
		a.Capabilities = normalizeCapabilities(a.Capabilities)
		if last.Valid {
			a.LastRun = &last.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SaveAgent(ctx context.Context, a *model.Agent) error {
	if !agent.IsSupportedType(a.Type) {
		return errors.New("agent type must be Codex, Claude, Kiro, or Copilot")
	}
	a.CommandPath = strings.TrimSpace(a.CommandPath)
	if a.CommandPath == "" {
		a.CommandPath = agent.DefaultCommand(a.Type)
	}
	a.Status, a.LastError = validateAgent(a.Type, a.CommandPath)
	caps, _ := json.Marshal(capabilities(a.Type))
	t := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var duplicateID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM agents WHERE type=? AND id<>? LIMIT 1`, a.Type, a.ID).Scan(&duplicateID)
	if err == nil {
		return fmt.Errorf("%s agent has already been added", a.Type)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if a.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO agents(type,command_path,instruction,status,capabilities_json,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			a.Type, a.CommandPath, a.Instruction, a.Status, string(caps), a.LastError, t, t)
		if err != nil {
			return err
		}
		a.ID, _ = res.LastInsertId()
		a.CreatedAt, a.UpdatedAt = t, t
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE agents SET type=?,command_path=?,instruction=?,status=?,capabilities_json=?,last_error=?,updated_at=? WHERE id=?`,
			a.Type, a.CommandPath, a.Instruction, a.Status, string(caps), a.LastError, t, a.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		a.UpdatedAt = t
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.Capabilities = capabilities(a.Type)
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureNoActiveRuns(ctx, tx, `agent_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET assigned_agent_id=NULL,updated_at=? WHERE assigned_agent_id=?`, now(), id); err != nil {
		return err
	}
	if err = deleteRuns(ctx, tx, `agent_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func validateFolder(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("folder: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("folder path is not a directory")
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("folder is not readable: %w", err)
	}
	if _, err = f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return "", fmt.Errorf("folder is not readable: %w", err)
	}
	f.Close()
	return filepath.Clean(abs), nil
}

func gitInfo(ctx context.Context, path string) (string, string) {
	rootBytes, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", ""
	}
	root := strings.TrimSpace(string(rootBytes))
	remoteBytes, _ := exec.CommandContext(ctx, "git", "-C", path, "remote", "get-url", "origin").Output()
	return root, strings.TrimSpace(string(remoteBytes))
}

var projectPrefixPattern = regexp.MustCompile(`^[A-Z0-9]{2,5}$`)

func defaultProjectPrefix(name string) string {
	var consonants, alphanumeric []rune
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			alphanumeric = append(alphanumeric, r)
			if (r < 'A' || r > 'Z') || !strings.ContainsRune("AEIOU", r) {
				consonants = append(consonants, r)
			}
		}
	}
	prefix := consonants
	if len(prefix) < 2 {
		prefix = alphanumeric
	}
	if len(prefix) > 5 {
		prefix = prefix[:5]
	}
	return string(prefix)
}

func normalizeProjectPrefix(prefix, name string) (string, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if prefix == "" {
		prefix = defaultProjectPrefix(name)
	}
	if !projectPrefixPattern.MatchString(prefix) {
		return "", errors.New("prefix must be 2 to 5 letters or numbers")
	}
	return prefix, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT p.id,p.name,COALESCE(p.prefix,''),p.folder_path,p.git_root,p.git_remote,p.default_milestone_id,p.created_at,p.updated_at,
       (SELECT COUNT(*) FROM milestones WHERE project_id=p.id),
       (SELECT COUNT(*) FROM tasks WHERE project_id=p.id),
       (SELECT MAX(started_at) FROM runs WHERE project_id=p.id)
FROM projects p ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Project
	for rows.Next() {
		var p model.Project
		var last nullableTime
		if err := rows.Scan(&p.ID, &p.Name, &p.Prefix, &p.FolderPath, &p.GitRoot, &p.GitRemote, &p.DefaultMilestoneID, &p.CreatedAt, &p.UpdatedAt, &p.MilestoneCount, &p.TaskCount, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			p.LastRun = &last.Time
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveProject(ctx context.Context, p *model.Project) error {
	path, err := validateFolder(p.FolderPath)
	if err != nil {
		return err
	}
	p.FolderPath = path
	if strings.TrimSpace(p.Name) == "" {
		p.Name = filepath.Base(path)
	}
	p.GitRoot, p.GitRemote = gitInfo(ctx, path)
	t := now()
	if p.ID == 0 {
		p.Prefix, err = normalizeProjectPrefix(p.Prefix, p.Name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, `INSERT INTO projects(name,prefix,folder_path,git_root,git_remote,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, p.Name, p.Prefix, path, p.GitRoot, p.GitRemote, t, t)
		if err != nil {
			return err
		}
		p.ID, _ = res.LastInsertId()
		res, err = tx.ExecContext(ctx, `INSERT INTO milestones(project_id,name,description,status,is_default_none,created_at,updated_at) VALUES(?,'None','Tasks without a milestone','open',1,?,?)`, p.ID, t, t)
		if err != nil {
			return err
		}
		p.DefaultMilestoneID, _ = res.LastInsertId()
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET default_milestone_id=? WHERE id=?`, p.DefaultMilestoneID, p.ID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		p.CreatedAt, p.UpdatedAt, p.MilestoneCount = t, t, 1
		return nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(prefix,'') FROM projects WHERE id=?`, p.ID).Scan(&p.Prefix); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET name=?,folder_path=?,git_root=?,git_remote=?,updated_at=? WHERE id=?`, p.Name, path, p.GitRoot, p.GitRemote, t, p.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	p.UpdatedAt = t
	return nil
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureNoActiveRuns(ctx, tx, `project_id=?`, id); err != nil {
		return err
	}
	if err = deleteRuns(ctx, tx, `project_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func ensureNoActiveRuns(ctx context.Context, tx *sql.Tx, where string, arg any) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE `+where+` AND status IN ('queued','starting','running','waiting_approval')`, arg).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return errors.New("stop active runs before deleting this item")
	}
	return nil
}

func deleteRuns(ctx context.Context, tx *sql.Tx, where string, arg any) error {
	query := `SELECT id FROM runs WHERE ` + where
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		for _, statement := range []string{
			`DELETE FROM approvals WHERE run_id=?`,
			`DELETE FROM trace_events WHERE run_id=?`,
			`DELETE FROM run_logs WHERE run_id=?`,
			`DELETE FROM runs WHERE id=?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ListMilestones(ctx context.Context, projectID int64) ([]model.Milestone, error) {
	q := `SELECT m.id,m.project_id,p.name,m.name,m.description,m.status,m.target_date,m.is_default_none,m.created_at,m.updated_at,
 (SELECT COUNT(*) FROM tasks WHERE milestone_id=m.id),(SELECT COUNT(*) FROM tasks WHERE milestone_id=m.id AND status='closed')
 FROM milestones m JOIN projects p ON p.id=m.project_id`
	var rows *sql.Rows
	var err error
	if projectID > 0 {
		rows, err = s.db.QueryContext(ctx, q+` WHERE m.project_id=? ORDER BY m.is_default_none DESC,m.name`, projectID)
	} else {
		rows, err = s.db.QueryContext(ctx, q+` ORDER BY p.name,m.is_default_none DESC,m.name`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Milestone
	for rows.Next() {
		var m model.Milestone
		var date sql.NullString
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.ProjectName, &m.Name, &m.Description, &m.Status, &date, &m.IsDefaultNone, &m.CreatedAt, &m.UpdatedAt, &m.TaskCount, &m.DoneCount); err != nil {
			return nil, err
		}
		if date.Valid {
			m.TargetDate = &date.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SaveMilestone(ctx context.Context, m *model.Milestone) error {
	if m.ProjectID == 0 {
		return errors.New("project_id is required")
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("name is required")
	}
	if m.Status == "" {
		m.Status = "open"
	}
	t := now()
	var date any
	if m.TargetDate != nil && *m.TargetDate != "" {
		date = *m.TargetDate
	}
	if m.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO milestones(project_id,name,description,status,target_date,is_default_none,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?)`, m.ProjectID, m.Name, m.Description, m.Status, date, t, t)
		if err != nil {
			return err
		}
		m.ID, _ = res.LastInsertId()
		m.CreatedAt, m.UpdatedAt = t, t
		return nil
	}
	var locked bool
	if err := s.db.QueryRowContext(ctx, `SELECT is_default_none FROM milestones WHERE id=?`, m.ID).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return errors.New("the None milestone cannot be edited")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE milestones SET name=?,description=?,status=?,target_date=?,updated_at=? WHERE id=?`, m.Name, m.Description, m.Status, date, t, m.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	m.UpdatedAt = t
	return nil
}

func (s *Store) ListTasks(ctx context.Context, projectID, milestoneID int64) ([]model.Task, error) {
	q := `SELECT t.id,CASE WHEN p.prefix IS NOT NULL AND t.project_number IS NOT NULL THEN p.prefix||'-'||t.project_number ELSE CAST(t.id AS TEXT) END,t.project_number,t.project_id,p.name,t.milestone_id,m.name,t.title,t.description,t.label,t.label_auto,t.status,t.assigned_agent_id,COALESCE(a.type,''),t.instruction,t.acceptance_criteria,
	 COALESCE(i.name,''),COALESCE(i.content_type,''),COALESCE(i.size,0),
	 lr.id,lr.project_id,lr.milestone_id,lr.task_id,lr.agent_id,COALESCE(ra.type,''),lr.status,lr.instruction,lr.working_directory,lr.process_id,lr.exit_code,lr.started_at,lr.ended_at,lr.trace_id,lr.error,lr.created_at,
	 COALESCE((SELECT group_concat(content, char(10)) FROM (SELECT content FROM (SELECT content,created_at,sequence,id FROM run_logs WHERE run_id=lr.id ORDER BY created_at DESC,sequence DESC,id DESC LIMIT 20) ORDER BY created_at,sequence,id)),''),
	 t.created_at,t.updated_at
	 FROM tasks t
	 JOIN projects p ON p.id=t.project_id
	 JOIN milestones m ON m.id=t.milestone_id
	 LEFT JOIN agents a ON a.id=t.assigned_agent_id
	 LEFT JOIN task_images i ON i.id=(SELECT id FROM task_images WHERE task_id=t.id ORDER BY position,id LIMIT 1)
	 LEFT JOIN runs lr ON lr.id=(SELECT id FROM runs WHERE task_id=t.id ORDER BY id DESC LIMIT 1)
	 LEFT JOIN agents ra ON ra.id=lr.agent_id
	 WHERE 1=1`
	var args []any
	if projectID > 0 {
		q += ` AND t.project_id=?`
		args = append(args, projectID)
	}
	if milestoneID > 0 {
		q += ` AND t.milestone_id=?`
		args = append(args, milestoneID)
	}
	q += ` ORDER BY t.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		var t model.Task
		var aid sql.NullInt64
		var projectNumber sql.NullInt64
		var runID, runProjectID, runMilestoneID, runTaskID, runAgentID, pid, exit sql.NullInt64
		var runAgentType, runStatus, runInstruction, runWorkingDirectory, runTraceID, runError sql.NullString
		var start, end, runCreated nullableTime
		if err := rows.Scan(&t.ID, &t.DisplayID, &projectNumber, &t.ProjectID, &t.ProjectName, &t.MilestoneID, &t.MilestoneName, &t.Title, &t.Description, &t.Label, &t.LabelAuto, &t.Status, &aid, &t.AssignedAgentType, &t.Instruction, &t.AcceptanceCriteria, &t.ImageName, &t.ImageContentType, &t.ImageSize, &runID, &runProjectID, &runMilestoneID, &runTaskID, &runAgentID, &runAgentType, &runStatus, &runInstruction, &runWorkingDirectory, &pid, &exit, &start, &end, &runTraceID, &runError, &runCreated, &t.LatestLog, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if t.Label == "" {
			t.Label = model.InferTaskLabel(t)
			t.LabelAuto = true
		}
		if projectNumber.Valid {
			t.ProjectNumber = &projectNumber.Int64
		}
		if aid.Valid {
			t.AssignedAgentID = &aid.Int64
		}
		if runID.Valid {
			r := model.Run{
				ID:               runID.Int64,
				ProjectID:        runProjectID.Int64,
				ProjectName:      t.ProjectName,
				MilestoneID:      runMilestoneID.Int64,
				TaskID:           runTaskID.Int64,
				TaskTitle:        t.Title,
				AgentID:          runAgentID.Int64,
				AgentType:        runAgentType.String,
				Status:           runStatus.String,
				Instruction:      runInstruction.String,
				WorkingDirectory: runWorkingDirectory.String,
				TraceID:          runTraceID.String,
				Error:            runError.String,
				CreatedAt:        runCreated.Time,
			}
			if pid.Valid {
				x := int(pid.Int64)
				r.ProcessID = &x
			}
			if exit.Valid {
				x := int(exit.Int64)
				r.ExitCode = &x
			}
			if start.Valid {
				r.StartedAt = &start.Time
			}
			if end.Valid {
				r.EndedAt = &end.Time
			}
			t.LatestRun = &r
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		images, err := s.ListTaskImages(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Images = make([]model.TaskImageMeta, len(images))
		for j, image := range images {
			out[i].Images[j] = model.TaskImageMeta{
				ID: image.ID, Name: image.Name, ContentType: image.ContentType, Size: image.Size,
			}
		}
	}
	return out, nil
}

func (s *Store) SaveTaskImage(ctx context.Context, image model.TaskImage) error {
	return s.SaveTaskImages(ctx, []model.TaskImage{image})
}

func (s *Store) SaveTaskImages(ctx context.Context, images []model.TaskImage) error {
	if len(images) == 0 {
		return errors.New("at least one image is required")
	}
	taskID := images[0].TaskID
	for i := range images {
		if images[i].TaskID == 0 || images[i].TaskID != taskID {
			return errors.New("all images must have the same task_id")
		}
		if images[i].Name == "" || images[i].ContentType == "" || len(images[i].Data) == 0 {
			return errors.New("image name, content type, and data are required")
		}
		images[i].Size = int64(len(images[i].Data))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count, nextPosition int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(position),-1)+1 FROM task_images WHERE task_id=?`, taskID).Scan(&count, &nextPosition); err != nil {
		return err
	}
	if count+len(images) > 5 {
		return errors.New("a task can have at most 5 images")
	}
	t := now()
	for i := range images {
		res, err := tx.ExecContext(ctx, `
INSERT INTO task_images(task_id,name,content_type,size,data,position,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
			taskID, images[i].Name, images[i].ContentType, images[i].Size, images[i].Data, nextPosition+i, t, t)
		if err != nil {
			return err
		}
		images[i].ID, _ = res.LastInsertId()
	}
	return tx.Commit()
}

func (s *Store) ListTaskImages(ctx context.Context, taskID int64) ([]model.TaskImage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,name,content_type,size FROM task_images WHERE task_id=? ORDER BY position,id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []model.TaskImage
	for rows.Next() {
		var image model.TaskImage
		if err := rows.Scan(&image.ID, &image.TaskID, &image.Name, &image.ContentType, &image.Size); err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func (s *Store) GetTaskImage(ctx context.Context, taskID int64) (model.TaskImage, error) {
	var image model.TaskImage
	err := s.db.QueryRowContext(ctx, `SELECT id,task_id,name,content_type,size,data FROM task_images WHERE task_id=? ORDER BY position,id LIMIT 1`, taskID).
		Scan(&image.ID, &image.TaskID, &image.Name, &image.ContentType, &image.Size, &image.Data)
	return image, err
}

func (s *Store) GetTaskImageByID(ctx context.Context, taskID, imageID int64) (model.TaskImage, error) {
	var image model.TaskImage
	err := s.db.QueryRowContext(ctx, `SELECT id,task_id,name,content_type,size,data FROM task_images WHERE task_id=? AND id=?`, taskID, imageID).
		Scan(&image.ID, &image.TaskID, &image.Name, &image.ContentType, &image.Size, &image.Data)
	return image, err
}

func (s *Store) DeleteTaskImageByID(ctx context.Context, taskID, imageID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_images WHERE task_id=? AND id=?`, taskID, imageID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteTaskImages(ctx context.Context, taskID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_images WHERE task_id=?`, taskID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteTaskImage(ctx context.Context, taskID int64) error {
	image, err := s.GetTaskImage(ctx, taskID)
	if err != nil {
		return err
	}
	return s.DeleteTaskImageByID(ctx, taskID, image.ID)
}

func (s *Store) SaveTask(ctx context.Context, t *model.Task) error {
	if t.ProjectID == 0 {
		return errors.New("project_id is required")
	}
	if strings.TrimSpace(t.Title) == "" {
		return errors.New("title is required")
	}
	if t.Status == "" {
		t.Status = "todo"
	}
	if !validTaskStatus(t.Status) {
		return errors.New("invalid task status")
	}
	t.Label = strings.ToLower(strings.TrimSpace(t.Label))
	if t.LabelAuto || t.Label == "" {
		t.Label = model.InferTaskLabel(*t)
		t.LabelAuto = true
	} else if !model.ValidTaskLabel(t.Label) {
		return errors.New("invalid task label")
	}
	if t.MilestoneID == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT default_milestone_id FROM projects WHERE id=?`, t.ProjectID).Scan(&t.MilestoneID); err != nil {
			return err
		}
	}
	var milestoneProject int64
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM milestones WHERE id=?`, t.MilestoneID).Scan(&milestoneProject); err != nil {
		return err
	}
	if milestoneProject != t.ProjectID {
		return errors.New("milestone does not belong to project")
	}
	tm := now()
	var aid any
	if t.AssignedAgentID != nil && *t.AssignedAgentID > 0 {
		aid = *t.AssignedAgentID
	}
	if t.ID == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var prefix sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT prefix FROM projects WHERE id=?`, t.ProjectID).Scan(&prefix); err != nil {
			return err
		}
		var projectNumber any
		if prefix.Valid {
			var next int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(project_number),0)+1 FROM tasks WHERE project_id=?`, t.ProjectID).Scan(&next); err != nil {
				return err
			}
			projectNumber = next
			t.ProjectNumber = &next
			t.DisplayID = fmt.Sprintf("%s-%d", prefix.String, next)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO tasks(project_id,project_number,milestone_id,title,description,label,label_auto,status,assigned_agent_id,instruction,acceptance_criteria,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ProjectID, projectNumber, t.MilestoneID, t.Title, t.Description, t.Label, t.LabelAuto, t.Status, aid, t.Instruction, t.AcceptanceCriteria, tm, tm)
		if err != nil {
			return err
		}
		t.ID, _ = res.LastInsertId()
		if t.DisplayID == "" {
			t.DisplayID = fmt.Sprint(t.ID)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		t.CreatedAt, t.UpdatedAt = tm, tm
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentProjectID int64
	var currentProjectNumber sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,project_number FROM tasks WHERE id=?`, t.ID).Scan(&currentProjectID, &currentProjectNumber); err != nil {
		return err
	}
	t.ProjectNumber = nil
	var projectNumber any
	if currentProjectID == t.ProjectID && currentProjectNumber.Valid {
		projectNumber = currentProjectNumber.Int64
		t.ProjectNumber = &currentProjectNumber.Int64
	}
	var prefix sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT prefix FROM projects WHERE id=?`, t.ProjectID).Scan(&prefix); err != nil {
		return err
	}
	if currentProjectID != t.ProjectID && prefix.Valid {
		var next int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(project_number),0)+1 FROM tasks WHERE project_id=?`, t.ProjectID).Scan(&next); err != nil {
			return err
		}
		projectNumber = next
		t.ProjectNumber = &next
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET project_id=?,project_number=?,milestone_id=?,title=?,description=?,label=?,label_auto=?,status=?,assigned_agent_id=?,instruction=?,acceptance_criteria=?,updated_at=? WHERE id=?`, t.ProjectID, projectNumber, t.MilestoneID, t.Title, t.Description, t.Label, t.LabelAuto, t.Status, aid, t.Instruction, t.AcceptanceCriteria, tm, t.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if prefix.Valid && t.ProjectNumber != nil {
		t.DisplayID = fmt.Sprintf("%s-%d", prefix.String, *t.ProjectNumber)
	} else {
		t.DisplayID = fmt.Sprint(t.ID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.UpdatedAt = tm
	return nil
}

func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureNoActiveRuns(ctx, tx, `task_id=?`, id); err != nil {
		return err
	}
	if err = deleteRuns(ctx, tx, `task_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) SetTaskStatus(ctx context.Context, id int64, status string) error {
	if !validTaskStatus(status) {
		return errors.New("invalid task status")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?,updated_at=? WHERE id=?`, status, now(), id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func validTaskStatus(status string) bool {
	switch status {
	case "todo", "in_progress", "agent_done", "closed":
		return true
	default:
		return false
	}
}

func (s *Store) GetTaskRunContext(ctx context.Context, taskID, agentID int64) (model.Task, model.Project, model.Milestone, model.Agent, error) {
	var t model.Task
	var p model.Project
	var m model.Milestone
	var a model.Agent
	var assigned sql.NullInt64
	var projectNumber sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT t.id,CASE WHEN p.prefix IS NOT NULL AND t.project_number IS NOT NULL THEN p.prefix||'-'||t.project_number ELSE CAST(t.id AS TEXT) END,t.project_number,t.project_id,t.milestone_id,t.title,t.description,t.label,t.label_auto,t.status,t.assigned_agent_id,t.instruction,t.acceptance_criteria,p.name,COALESCE(p.prefix,''),p.folder_path,p.git_root,p.git_remote,m.name,m.description,m.status,m.is_default_none FROM tasks t JOIN projects p ON p.id=t.project_id JOIN milestones m ON m.id=t.milestone_id WHERE t.id=?`, taskID).Scan(&t.ID, &t.DisplayID, &projectNumber, &t.ProjectID, &t.MilestoneID, &t.Title, &t.Description, &t.Label, &t.LabelAuto, &t.Status, &assigned, &t.Instruction, &t.AcceptanceCriteria, &p.Name, &p.Prefix, &p.FolderPath, &p.GitRoot, &p.GitRemote, &m.Name, &m.Description, &m.Status, &m.IsDefaultNone)
	if err != nil {
		return t, p, m, a, err
	}
	if projectNumber.Valid {
		t.ProjectNumber = &projectNumber.Int64
	}
	if t.Label == "" {
		t.Label = model.InferTaskLabel(t)
		t.LabelAuto = true
	}
	p.ID = t.ProjectID
	m.ID = t.MilestoneID
	m.ProjectID = t.ProjectID
	if agentID == 0 && assigned.Valid {
		agentID = assigned.Int64
	}
	if agentID == 0 {
		return t, p, m, a, errors.New("an agent must be assigned")
	}
	var caps string
	err = s.db.QueryRowContext(ctx, `SELECT id,type,command_path,instruction,status,capabilities_json,last_error,created_at,updated_at FROM agents WHERE id=?`, agentID).Scan(&a.ID, &a.Type, &a.CommandPath, &a.Instruction, &a.Status, &caps, &a.LastError, &a.CreatedAt, &a.UpdatedAt)
	_ = json.Unmarshal([]byte(caps), &a.Capabilities)
	a.Capabilities = normalizeCapabilities(a.Capabilities)
	return t, p, m, a, err
}

func (s *Store) CreateRun(ctx context.Context, r *model.Run) error {
	t := now()
	r.CreatedAt = t
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs(project_id,milestone_id,task_id,agent_id,status,instruction,working_directory,trace_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.ProjectID, r.MilestoneID, r.TaskID, r.AgentID, r.Status, r.Instruction, r.WorkingDirectory, r.TraceID, t)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}
func (s *Store) StartRun(ctx context.Context, id int64, pid int, started time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status='running',process_id=?,started_at=? WHERE id=?`, pid, started, id)
	return err
}
func (s *Store) FinishRun(ctx context.Context, id int64, status string, exitCode int, runErr string, ended time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,exit_code=?,error=?,ended_at=? WHERE id=?`, status, exitCode, runErr, ended, id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	if status == "success" {
		if _, err := tx.ExecContext(ctx, `
UPDATE tasks
SET status='agent_done',updated_at=?
WHERE id=(SELECT task_id FROM runs WHERE id=?) AND status='in_progress'
`, now(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) SetRunStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) ListRuns(ctx context.Context, limit int) ([]model.Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.project_id,p.name,r.milestone_id,r.task_id,t.title,r.agent_id,a.type,r.status,r.instruction,r.working_directory,r.process_id,r.exit_code,r.started_at,r.ended_at,r.trace_id,r.error,r.created_at FROM runs r JOIN projects p ON p.id=r.project_id JOIN tasks t ON t.id=r.task_id JOIN agents a ON a.id=r.agent_id ORDER BY r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		var r model.Run
		var pid, exit sql.NullInt64
		var start, end nullableTime
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.MilestoneID, &r.TaskID, &r.TaskTitle, &r.AgentID, &r.AgentType, &r.Status, &r.Instruction, &r.WorkingDirectory, &pid, &exit, &start, &end, &r.TraceID, &r.Error, &r.CreatedAt); err != nil {
			return nil, err
		}
		if pid.Valid {
			x := int(pid.Int64)
			r.ProcessID = &x
		}
		if exit.Valid {
			x := int(exit.Int64)
			r.ExitCode = &x
		}
		if start.Valid {
			r.StartedAt = &start.Time
		}
		if end.Valid {
			r.EndedAt = &end.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) GetRun(ctx context.Context, id int64) (model.Run, error) {
	runs, err := s.ListRuns(ctx, 500)
	if err != nil {
		return model.Run{}, err
	}
	for _, r := range runs {
		if r.ID == id {
			return r, nil
		}
	}
	return model.Run{}, sql.ErrNoRows
}
func (s *Store) AddLog(ctx context.Context, runID int64, stream, content string) error {
	var seq int64
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM run_logs WHERE run_id=? AND stream=?`, runID, stream).Scan(&seq)
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_logs(run_id,stream,sequence,content,created_at) VALUES(?,?,?,?,?)`, runID, stream, seq, content, now())
	return err
}
func (s *Store) Logs(ctx context.Context, runID int64) ([]model.RunLog, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,stream,sequence,content,created_at FROM run_logs WHERE run_id=? ORDER BY created_at,sequence`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RunLog
	for rows.Next() {
		var l model.RunLog
		if err := rows.Scan(&l.ID, &l.RunID, &l.Stream, &l.Sequence, &l.Content, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ListChats(ctx context.Context) ([]model.Chat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id,c.project_id,p.name,c.task_id,t.title,c.agent_id,a.type,c.title,c.created_at,c.updated_at
FROM chats c
JOIN projects p ON p.id=c.project_id
JOIN tasks t ON t.id=c.task_id
JOIN agents a ON a.id=c.agent_id
ORDER BY c.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Chat
	for rows.Next() {
		var c model.Chat
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ProjectName, &c.TaskID, &c.TaskTitle, &c.AgentID, &c.AgentType, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateChat(ctx context.Context, c *model.Chat) error {
	if c.AgentID == 0 {
		return errors.New("agent_id is required")
	}
	tm := now()
	if c.TaskID == 0 {
		if c.ProjectID == 0 {
			return errors.New("project_id is required")
		}
		title := strings.TrimSpace(c.Title)
		if title == "" {
			title = "Chat"
		}
		task := model.Task{
			ProjectID:       c.ProjectID,
			Title:           "Chat: " + title,
			Description:     "Conversation context created from Chats.",
			Status:          "todo",
			AssignedAgentID: &c.AgentID,
		}
		if err := s.SaveTask(ctx, &task); err != nil {
			return err
		}
		c.TaskID = task.ID
		c.TaskTitle = task.Title
	} else {
		if err := s.db.QueryRowContext(ctx, `SELECT project_id,title FROM tasks WHERE id=?`, c.TaskID).Scan(&c.ProjectID, &c.TaskTitle); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Title) == "" {
		c.Title = c.TaskTitle
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO chats(project_id,task_id,agent_id,title,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		c.ProjectID, c.TaskID, c.AgentID, strings.TrimSpace(c.Title), tm, tm)
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	c.CreatedAt, c.UpdatedAt = tm, tm
	return nil
}

func (s *Store) GetChat(ctx context.Context, id int64) (model.Chat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id,c.project_id,p.name,c.task_id,t.title,c.agent_id,a.type,c.title,c.created_at,c.updated_at
FROM chats c
JOIN projects p ON p.id=c.project_id
JOIN tasks t ON t.id=c.task_id
JOIN agents a ON a.id=c.agent_id
WHERE c.id=?`, id)
	if err != nil {
		return model.Chat{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return model.Chat{}, sql.ErrNoRows
	}
	var c model.Chat
	if err := rows.Scan(&c.ID, &c.ProjectID, &c.ProjectName, &c.TaskID, &c.TaskTitle, &c.AgentID, &c.AgentType, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return model.Chat{}, err
	}
	return c, rows.Err()
}

func (s *Store) UpdateChatAgent(ctx context.Context, chatID, agentID int64) error {
	if agentID == 0 {
		return errors.New("agent_id is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE chats SET agent_id=?,updated_at=? WHERE id=?`, agentID, now(), chatID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) AddChatMessage(ctx context.Context, chatID int64, runID *int64, content string) (model.ChatMessage, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return model.ChatMessage{}, errors.New("message is required")
	}
	tm := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO chat_messages(chat_id,run_id,role,content,created_at) VALUES(?,?,'user',?,?)`, chatID, runID, content, tm)
	if err != nil {
		return model.ChatMessage{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `UPDATE chats SET updated_at=? WHERE id=?`, tm, chatID); err != nil {
		return model.ChatMessage{}, err
	}
	return model.ChatMessage{ID: id, ChatID: chatID, RunID: runID, Role: "user", Content: content, CreatedAt: tm}, nil
}

func (s *Store) AttachChatMessageRun(ctx context.Context, messageID, runID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_messages SET run_id=? WHERE id=?`, runID, messageID)
	return err
}

func (s *Store) ListChatMessages(ctx context.Context, chatID int64) ([]model.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT cm.id,cm.chat_id,cm.run_id,cm.role,cm.content,cm.created_at,
       r.id,r.project_id,p.name,r.milestone_id,r.task_id,t.title,r.agent_id,a.type,r.status,r.instruction,r.working_directory,r.process_id,r.exit_code,r.started_at,r.ended_at,r.trace_id,r.error,r.created_at
FROM chat_messages cm
LEFT JOIN runs r ON r.id=cm.run_id
LEFT JOIN projects p ON p.id=r.project_id
LEFT JOIN tasks t ON t.id=r.task_id
LEFT JOIN agents a ON a.id=r.agent_id
WHERE cm.chat_id=?
ORDER BY cm.created_at,cm.id`, chatID)
	if err != nil {
		return nil, err
	}
	var out []model.ChatMessage
	for rows.Next() {
		var m model.ChatMessage
		var runID, rID, projectID, milestoneID, taskID, agentID, pid, exit sql.NullInt64
		var projectName, taskTitle, agentType, status, instruction, workingDirectory, traceID, runError sql.NullString
		var start, end, runCreated nullableTime
		if err := rows.Scan(&m.ID, &m.ChatID, &runID, &m.Role, &m.Content, &m.CreatedAt, &rID, &projectID, &projectName, &milestoneID, &taskID, &taskTitle, &agentID, &agentType, &status, &instruction, &workingDirectory, &pid, &exit, &start, &end, &traceID, &runError, &runCreated); err != nil {
			return nil, err
		}
		if runID.Valid {
			m.RunID = &runID.Int64
		}
		if rID.Valid {
			r := model.Run{
				ID: rID.Int64, ProjectID: projectID.Int64, ProjectName: projectName.String,
				MilestoneID: milestoneID.Int64, TaskID: taskID.Int64, TaskTitle: taskTitle.String,
				AgentID: agentID.Int64, AgentType: agentType.String, Status: status.String,
				Instruction: instruction.String, WorkingDirectory: workingDirectory.String,
				TraceID: traceID.String, Error: runError.String, CreatedAt: runCreated.Time,
			}
			if pid.Valid {
				x := int(pid.Int64)
				r.ProcessID = &x
			}
			if exit.Valid {
				x := int(exit.Int64)
				r.ExitCode = &x
			}
			if start.Valid {
				r.StartedAt = &start.Time
			}
			if end.Valid {
				r.EndedAt = &end.Time
			}
			m.Run = &r
		}
		out = append(out, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Run == nil {
			continue
		}
		logs, err := s.Logs(ctx, out[i].Run.ID)
		if err != nil {
			return nil, err
		}
		out[i].Logs = logs
	}
	return out, nil
}

func (s *Store) AddTrace(ctx context.Context, e *model.TraceEvent) error {
	attrs, _ := json.Marshal(e.Attributes)
	if e.StartedAt.IsZero() {
		e.StartedAt = now()
	}
	e.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO trace_events(trace_id,run_id,parent_event_id,event_type,status,message,attributes_json,started_at,ended_at,duration_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.TraceID, e.RunID, e.ParentEventID, e.EventType, e.Status, e.Message, string(attrs), e.StartedAt, e.EndedAt, e.DurationMS, e.CreatedAt)
	if err == nil {
		e.ID, _ = res.LastInsertId()
	}
	return err
}
func (s *Store) ListTraces(ctx context.Context, projectID, agentID int64, status, eventType, search string, limit int) ([]model.TraceEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	q := `SELECT e.id,e.trace_id,e.run_id,e.parent_event_id,e.event_type,e.status,e.message,e.attributes_json,e.started_at,e.ended_at,e.duration_ms,e.created_at,a.type,p.name,t.title FROM trace_events e JOIN runs r ON r.id=e.run_id JOIN agents a ON a.id=r.agent_id JOIN projects p ON p.id=r.project_id JOIN tasks t ON t.id=r.task_id WHERE 1=1`
	var args []any
	if projectID > 0 {
		q += ` AND r.project_id=?`
		args = append(args, projectID)
	}
	if agentID > 0 {
		q += ` AND r.agent_id=?`
		args = append(args, agentID)
	}
	if status != "" {
		q += ` AND e.status=?`
		args = append(args, status)
	}
	if eventType != "" {
		q += ` AND e.event_type=?`
		args = append(args, eventType)
	}
	if search != "" {
		q += ` AND (e.message LIKE ? OR e.attributes_json LIKE ?)`
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	q += ` ORDER BY e.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TraceEvent
	for rows.Next() {
		var e model.TraceEvent
		var parent, dur sql.NullInt64
		var end nullableTime
		var attrs string
		if err := rows.Scan(&e.ID, &e.TraceID, &e.RunID, &parent, &e.EventType, &e.Status, &e.Message, &attrs, &e.StartedAt, &end, &dur, &e.CreatedAt, &e.AgentType, &e.ProjectName, &e.TaskTitle); err != nil {
			return nil, err
		}
		if parent.Valid {
			e.ParentEventID = &parent.Int64
		}
		if end.Valid {
			e.EndedAt = &end.Time
		}
		if dur.Valid {
			e.DurationMS = &dur.Int64
		}
		_ = json.Unmarshal([]byte(attrs), &e.Attributes)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) SaveChatTaskProposal(ctx context.Context, messageID int64, tasks []model.TaskProposalItem) error {
	if len(tasks) == 0 {
		return errors.New("at least one task is required")
	}
	data, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO chat_task_proposals(message_id,tasks_json,status,created_task_ids_json,created_at)
VALUES(?,?,'pending','[]',?)
ON CONFLICT(message_id) DO UPDATE SET tasks_json=excluded.tasks_json
WHERE chat_task_proposals.status='pending'`, messageID, string(data), now())
	return err
}

func (s *Store) GetChatTaskProposal(ctx context.Context, messageID int64) (*model.ChatTaskProposal, error) {
	var tasksJSON, status, createdIDsJSON string
	err := s.db.QueryRowContext(ctx, `
SELECT tasks_json,status,created_task_ids_json
FROM chat_task_proposals WHERE message_id=?`, messageID).Scan(&tasksJSON, &status, &createdIDsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	proposal := &model.ChatTaskProposal{MessageID: messageID, Status: status}
	if err := json.Unmarshal([]byte(tasksJSON), &proposal.Tasks); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(createdIDsJSON), &proposal.CreatedTaskIDs); err != nil {
		return nil, err
	}
	return proposal, nil
}

func (s *Store) ConfirmChatTaskProposal(ctx context.Context, chatID, messageID int64) ([]model.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var projectID int64
	var tasksJSON, status string
	if err := tx.QueryRowContext(ctx, `
SELECT c.project_id,p.tasks_json,p.status
FROM chat_task_proposals p
JOIN chat_messages cm ON cm.id=p.message_id
JOIN chats c ON c.id=cm.chat_id
WHERE p.message_id=? AND cm.chat_id=?`, messageID, chatID).Scan(&projectID, &tasksJSON, &status); err != nil {
		return nil, err
	}
	if status != "pending" {
		return nil, errors.New("task proposal has already been confirmed")
	}
	var proposals []model.TaskProposalItem
	if err := json.Unmarshal([]byte(tasksJSON), &proposals); err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return nil, errors.New("task proposal is empty")
	}

	var defaultMilestoneID int64
	var prefix sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT default_milestone_id,prefix FROM projects WHERE id=?`, projectID).Scan(&defaultMilestoneID, &prefix); err != nil {
		return nil, err
	}
	tm := now()
	created := make([]model.Task, 0, len(proposals))
	createdIDs := make([]int64, 0, len(proposals))
	for _, proposal := range proposals {
		proposal.Title = strings.TrimSpace(proposal.Title)
		if proposal.Title == "" {
			return nil, errors.New("task title is required")
		}
		milestoneID := proposal.MilestoneID
		if milestoneID == 0 {
			milestoneID = defaultMilestoneID
		}
		var milestoneProjectID int64
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM milestones WHERE id=?`, milestoneID).Scan(&milestoneProjectID); err != nil {
			return nil, err
		}
		if milestoneProjectID != projectID {
			return nil, errors.New("milestone does not belong to chat project")
		}

		var projectNumber any
		task := model.Task{
			ProjectID: projectID, MilestoneID: milestoneID, Title: proposal.Title,
			Description: proposal.Description, Status: "todo", Instruction: proposal.Instruction,
			AcceptanceCriteria: proposal.AcceptanceCriteria, CreatedAt: tm, UpdatedAt: tm,
		}
		if prefix.Valid {
			var next int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(project_number),0)+1 FROM tasks WHERE project_id=?`, projectID).Scan(&next); err != nil {
				return nil, err
			}
			projectNumber = next
			task.ProjectNumber = &next
			task.DisplayID = fmt.Sprintf("%s-%d", prefix.String, next)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO tasks(project_id,project_number,milestone_id,title,description,status,instruction,acceptance_criteria,created_at,updated_at)
VALUES(?,?,?,?,?,'todo',?,?,?,?)`,
			projectID, projectNumber, milestoneID, task.Title, task.Description, task.Instruction, task.AcceptanceCriteria, tm, tm)
		if err != nil {
			return nil, err
		}
		task.ID, _ = res.LastInsertId()
		if task.DisplayID == "" {
			task.DisplayID = fmt.Sprint(task.ID)
		}
		created = append(created, task)
		createdIDs = append(createdIDs, task.ID)
	}
	createdJSON, err := json.Marshal(createdIDs)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE chat_task_proposals
SET status='created',created_task_ids_json=?,confirmed_at=?
WHERE message_id=? AND status='pending'`, string(createdJSON), tm, messageID)
	if err != nil {
		return nil, err
	}
	affected, _ := res.RowsAffected()
	if affected != 1 {
		return nil, errors.New("task proposal has already been confirmed")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) CreateApproval(ctx context.Context, a *model.Approval) error {
	a.Status = "pending"
	a.RequestedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO approvals(run_id,agent_id,project_id,task_id,request_type,operation,reason,risk,status,requested_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.RunID, a.AgentID, a.ProjectID, a.TaskID, a.RequestType, a.Operation, a.Reason, a.Risk, a.Status, a.RequestedAt, a.ExpiresAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}
func (s *Store) ListApprovals(ctx context.Context, status string) ([]model.Approval, error) {
	q := `SELECT ap.id,ap.run_id,ap.agent_id,a.type,ap.project_id,p.name,ap.task_id,t.title,ap.request_type,ap.operation,ap.reason,ap.risk,ap.status,ap.decision,ap.requested_at,ap.decided_at,ap.expires_at FROM approvals ap JOIN agents a ON a.id=ap.agent_id JOIN projects p ON p.id=ap.project_id JOIN tasks t ON t.id=ap.task_id`
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.QueryContext(ctx, q+` WHERE ap.status=? ORDER BY ap.requested_at DESC`, status)
	} else {
		rows, err = s.db.QueryContext(ctx, q+` ORDER BY ap.requested_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Approval
	for rows.Next() {
		var a model.Approval
		var decided nullableTime
		if err := rows.Scan(&a.ID, &a.RunID, &a.AgentID, &a.AgentType, &a.ProjectID, &a.ProjectName, &a.TaskID, &a.TaskTitle, &a.RequestType, &a.Operation, &a.Reason, &a.Risk, &a.Status, &a.Decision, &a.RequestedAt, &decided, &a.ExpiresAt); err != nil {
			return nil, err
		}
		if decided.Valid {
			a.DecidedAt = &decided.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) DecideApproval(ctx context.Context, id int64, decision string) (model.Approval, error) {
	if decision != "allowed" && decision != "denied" {
		return model.Approval{}, errors.New("decision must be allowed or denied")
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?,decision=?,decided_at=? WHERE id=? AND status='pending'`, decision, decision, t, id)
	if err != nil {
		return model.Approval{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return model.Approval{}, errors.New("approval is not pending")
	}
	items, err := s.ListApprovals(ctx, "")
	if err != nil {
		return model.Approval{}, err
	}
	for _, a := range items {
		if a.ID == id {
			return a, nil
		}
	}
	return model.Approval{}, sql.ErrNoRows
}
func (s *Store) ExpireApprovals(ctx context.Context) ([]model.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM approvals WHERE status='pending' AND expires_at<=?`, now())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE approvals SET status='expired',decision='denied',decided_at=? WHERE status='pending' AND expires_at<=?`, now(), now())
	if err != nil {
		return nil, err
	}
	all, err := s.ListApprovals(ctx, "expired")
	if err != nil {
		return nil, err
	}
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	var expired []model.Approval
	for _, approval := range all {
		if wanted[approval.ID] {
			expired = append(expired, approval)
		}
	}
	return expired, nil
}

func (s *Store) Settings(ctx context.Context, defaults model.Settings) (model.Settings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings`)
	if err != nil {
		return defaults, err
	}
	defer rows.Close()
	vals := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return defaults, err
		}
		vals[k] = v
	}
	b, _ := json.Marshal(defaults)
	_ = json.Unmarshal(b, &defaults)
	if raw := vals["settings"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &defaults)
	}
	defaults.DatabasePath = s.path
	return defaults, rows.Err()
}
func (s *Store) SaveSettings(ctx context.Context, v model.Settings) error {
	v.DatabasePath = s.path
	b, _ := json.Marshal(v)
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('settings',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	return err
}
func (s *Store) Dashboard(ctx context.Context) (model.Dashboard, error) {
	var d model.Dashboard
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM agents),(SELECT COUNT(*) FROM projects),(SELECT COUNT(*) FROM approvals WHERE status='pending'),(SELECT COUNT(*) FROM runs WHERE status IN ('queued','starting','running','waiting_approval'))`).Scan(&d.Agents, &d.Projects, &d.PendingApprovals, &d.Running)
	return d, err
}
