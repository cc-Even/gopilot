package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Skill struct {
	Meta map[string]string
	Body string
	Path string
}

// SkillLoader scans skills/<name>/SKILL.md and exposes description/content views.
type SkillLoader struct {
	skillsDir string
	skills    map[string]Skill
	order     []string
}

func NewSkillLoader(skillsDir string) *SkillLoader {
	l := &SkillLoader{
		skillsDir: skillsDir,
		skills:    make(map[string]Skill),
	}
	l.loadAll()
	return l
}

func (l *SkillLoader) loadAll() {
	if l == nil || l.skillsDir == "" {
		return
	}
	info, err := os.Stat(l.skillsDir)
	if err != nil || !info.IsDir() {
		return
	}

	var files []string
	_ = filepath.WalkDir(l.skillsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "SKILL.md" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		meta, body := parseFrontmatter(string(raw))
		name := meta["name"]
		if name == "" {
			name = filepath.Base(filepath.Dir(f))
		}

		if _, exists := l.skills[name]; !exists {
			l.order = append(l.order, name)
		}
		l.skills[name] = Skill{
			Meta: meta,
			Body: body,
			Path: f,
		}
	}
}

func parseFrontmatter(text string) (map[string]string, string) {
	normalized := strings.TrimPrefix(text, "\uFEFF")
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	frontmatterRe := regexp.MustCompile(`(?s)^---\n(.*?)\n---(?:\n(.*))?$`)
	match := frontmatterRe.FindStringSubmatch(normalized)
	if len(match) == 0 {
		return map[string]string{}, strings.TrimSpace(normalized)
	}

	meta := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(match[1]), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key != "" {
			if val == "|" || val == ">" {
				block := make([]string, 0)
				for j := i + 1; j < len(lines); j++ {
					next := lines[j]
					trimmed := strings.TrimSpace(next)
					if trimmed == "" {
						block = append(block, "")
						i = j
						continue
					}
					if strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t") {
						block = append(block, strings.TrimLeft(next, " \t"))
						i = j
						continue
					}
					break
				}
				val = strings.TrimSpace(strings.Join(block, "\n"))
			}
			meta[key] = val
		}
	}

	body := ""
	if len(match) >= 3 {
		body = strings.TrimSpace(match[2])
	}
	return meta, body
}

func (l *SkillLoader) GetDescriptions() string {
	if l == nil || len(l.skills) == 0 {
		return "(no skills available)"
	}

	lines := make([]string, 0, len(l.skills))
	for _, name := range l.order {
		skill := l.skills[name]
		desc := skill.Meta["description"]
		if desc == "" {
			desc = "No description"
		}
		tags := skill.Meta["tags"]

		line := fmt.Sprintf("  - %s: %s", name, desc)
		if tags != "" {
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (l *SkillLoader) GetContent(name string) string {
	if l == nil {
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: ", name)
	}
	skill, ok := l.skills[name]
	if !ok {
		available := append([]string(nil), l.order...)
		sort.Strings(available)
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, strings.Join(available, ", "))
	}
	return fmt.Sprintf("<skill name=\"%s\">\n%s\n</skill>", name, skill.Body)
}

type BashTool struct{}
type BackgroundRunTool struct{}
type BackgroundCheckTool struct{}
type ReadFileTool struct{}
type WriteFileTool struct{}
type EditFileTool struct{}

func StringParam() map[string]any {
	return map[string]any{"type": "string"}
}

func IntegerParam() map[string]any {
	return map[string]any{"type": "integer"}
}

func ObjectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// DefaultToolDefinitions returns the built-in tool list used by Agent.
func DefaultToolDefinitions() []ToolDefinition {
	bash := BashTool{}
	backgroundRun := BackgroundRunTool{}
	backgroundCheck := BackgroundCheckTool{}
	read := ReadFileTool{}
	write := WriteFileTool{}
	edit := EditFileTool{}

	return []ToolDefinition{
		ToolFromStringArg(
			bash.Name(),
			bash.Description(),
			"command",
			ObjectSchema(map[string]any{"command": StringParam()}, "command"),
			bash.Call,
		),
		ToolFromStringArg(
			backgroundRun.Name(),
			backgroundRun.Description(),
			"command",
			ObjectSchema(map[string]any{"command": StringParam()}, "command"),
			backgroundRun.Call,
		),
		ToolFromJSONString(
			backgroundCheck.Name(),
			backgroundCheck.Description(),
			ObjectSchema(map[string]any{"task_id": StringParam()}),
			backgroundCheck.Call,
		),
		ToolFromJSONString(
			read.Name(),
			read.Description(),
			ObjectSchema(map[string]any{"path": StringParam(), "limit": IntegerParam()}, "path"),
			read.Call,
		),
		ToolFromJSONString(
			write.Name(),
			write.Description(),
			ObjectSchema(map[string]any{"path": StringParam(), "content": StringParam()}, "path", "content"),
			write.Call,
		),
		ToolFromJSONString(
			edit.Name(),
			edit.Description(),
			ObjectSchema(map[string]any{"path": StringParam(), "old_text": StringParam(), "new_text": StringParam()}, "path", "old_text", "new_text"),
			edit.Call,
		),
		ToolFromJSONString(
			"todo",
			"Update todo items. input should be a JSON array of objects with fields: id, text, status (pending, in_progress, completed).",
			ObjectSchema(map[string]any{"id": IntegerParam(), "text": StringParam(), "status": StringParam()}),
			UpdateTodoTool,
		),
		// Task management tools
		ToolFromJSONString(
			"task_create",
			"Create a new task.",
			ObjectSchema(map[string]any{"subject": StringParam(), "description": StringParam()}, "subject"),
			TaskCreateTool,
		),
		ToolFromJSONString(
			"task_update",
			"Update a task's status or dependencies.",
			ObjectSchema(map[string]any{
				"task_id":      IntegerParam(),
				"status":       StringParam(),
				"addBlockedBy": map[string]any{"type": "array", "items": IntegerParam()},
				"addBlocks":    map[string]any{"type": "array", "items": IntegerParam()},
			}, "task_id"),
			TaskUpdateTool,
		),
		ToolFromJSONString(
			"task_list",
			"List all tasks with status summary.",
			ObjectSchema(map[string]any{}),
			TaskListTool,
		),
		ToolFromJSONString(
			"task_get",
			"Get full details of a task by ID.",
			ObjectSchema(map[string]any{"task_id": IntegerParam()}, "task_id"),
			TaskGetTool,
		),
	}
}

// safePath ensures the path is within WORKDIR
func safePath(p string) (string, error) {
	absPath := filepath.Join(WORKDIR, p)
	resolved, err := filepath.Abs(absPath)
	if err != nil {
		return "", err
	}
	workdirAbs, err := filepath.Abs(WORKDIR)
	if err != nil {
		return "", err
	}
	if len(resolved) < len(workdirAbs) || resolved[:len(workdirAbs)] != workdirAbs {
		return "", errors.New("Path escapes workspace: " + p)
	}
	return resolved, nil
}

// RunBash executes a shell command safely
func RunBash(command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = WORKDIR
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output)
	}
	out := string(output)
	if len(out) > 50000 {
		return out[:50000]
	}
	if len(out) == 0 {
		return "(no output)"
	}
	return out
}

// RunRead reads a file safely, with optional line limit
func RunRead(path string, limit int) string {
	resolved, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error: " + err.Error()
	}
	lines := strings.Split(string(data), "\n")
	if limit > 0 && limit < len(lines) {
		lines = append(lines[:limit], fmt.Sprintf("... (%d more lines)", len(lines)-limit))
	}
	result := strings.Join(lines, "\n")
	if len(result) > 50000 {
		return result[:50000]
	}
	return result
}

// RunWrite writes content to a file safely
func RunWrite(path, content string) string {
	resolved, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	dir := filepath.Dir(resolved)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return "Error: " + err.Error()
	}
	err = os.WriteFile(resolved, []byte(content), 0644)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// RunEdit replaces oldText with newText in a file safely
func RunEdit(path, oldText, newText string) string {
	resolved, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error: " + err.Error()
	}
	content := string(data)
	if !strings.Contains(content, oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}
	newContent := strings.Replace(content, oldText, newText, 1)
	err = os.WriteFile(resolved, []byte(newContent), 0644)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Edited %s", path)
}

func (c BashTool) Name() string {
	return "bash"
}

func (c BashTool) Description() string {
	return "Executes a bash command and returns the output. Use this tool to solve coding tasks by running bash commands. be careful Windows command is different to linux"
}

func (c BashTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	log.Printf("[BashTool] Executing command: %s", input)
	result := RunBash(input)
	log.Printf("[BashTool] Command output (first 200 chars): %s", truncate(result, 200))
	return result, nil
}

func (c BackgroundRunTool) Name() string {
	return "background_run"
}

func (c BackgroundRunTool) Description() string {
	return "Starts a bash command in the background and returns immediately with a task id. Completed results are queued and surfaced before the next LLM call."
}

func (c BackgroundRunTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.Background == nil {
		return "", fmt.Errorf("background manager not initialized")
	}

	log.Printf("[BackgroundRunTool] Starting command: %s", input)
	result := agent.Background.Run(input)
	log.Printf("[BackgroundRunTool] Started: %s", result)
	return result, nil
}

func (c BackgroundCheckTool) Name() string {
	return "background_check"
}

func (c BackgroundCheckTool) Description() string {
	return "Checks one background task by task_id, or lists all background tasks when task_id is omitted."
}

func (c BackgroundCheckTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.Background == nil {
		return "", fmt.Errorf("background manager not initialized")
	}

	var params struct {
		TaskID string `json:"task_id"`
	}
	if input != "" && input != "null" {
		if err := json.Unmarshal([]byte(input), &params); err != nil {
			log.Printf("[BackgroundCheckTool] Error parsing input: %v", err)
			return "", fmt.Errorf("invalid input: %v", err)
		}
	}

	log.Printf("[BackgroundCheckTool] Checking task: task_id=%s", params.TaskID)
	result := agent.Background.Check(params.TaskID)
	log.Printf("[BackgroundCheckTool] Check result (first 200 chars): %s", truncate(result, 200))
	return result, nil
}

// ReadFileTool implementation
func (r ReadFileTool) Name() string {
	return "read_file"
}

func (r ReadFileTool) Description() string {
	return "Read file contents. input should be a JSON string with fields: path, limit (optional, number of lines to read)."
}

func (r ReadFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[ReadFileTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[ReadFileTool] Reading file: path=%s, limit=%d", params.Path, params.Limit)
	result := RunRead(params.Path, params.Limit)
	log.Printf("[ReadFileTool] File read completed (first 200 chars): %s", truncate(result, 200))
	return result, nil
}

// WriteFileTool implementation
func (w WriteFileTool) Name() string {
	return "write_file"
}

func (w WriteFileTool) Description() string {
	return "Write content to file. input should be a JSON string with fields: path, content."
}

func (w WriteFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[WriteFileTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[WriteFileTool] Writing file: path=%s, content_size=%d bytes", params.Path, len(params.Content))
	result := RunWrite(params.Path, params.Content)
	log.Printf("[WriteFileTool] File write completed: %s", result)
	return result, nil
}

// EditFileTool implementation
func (e EditFileTool) Name() string {
	return "edit_file"
}

func (e EditFileTool) Description() string {
	return "Replace exact text in file. input should be a JSON string with fields: path, old_text, new_text."
}

func (e EditFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[EditFileTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[EditFileTool] Editing file: path=%s, old_text_size=%d, new_text_size=%d", params.Path, len(params.OldText), len(params.NewText))
	result := RunEdit(params.Path, params.OldText, params.NewText)
	log.Printf("[EditFileTool] File edit completed: %s", result)
	return result, nil
}

// truncate returns first n characters of a string, with ellipsis if truncated
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Task Management Tool Handlers

// TaskCreateTool creates a new task
func TaskCreateTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	var params struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[TaskCreateTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	if params.Subject == "" {
		return "", fmt.Errorf("subject is required")
	}

	log.Printf("[TaskCreateTool] Creating task: subject=%s", params.Subject)
	result, err := agent.TaskManager.Create(params.Subject, params.Description)
	if err != nil {
		log.Printf("[TaskCreateTool] Error: %v", err)
		return "", err
	}
	log.Printf("[TaskCreateTool] Task created successfully")
	return result, nil
}

// TaskUpdateTool updates a task's status or dependencies
func TaskUpdateTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	var params struct {
		TaskID       int    `json:"task_id"`
		Status       string `json:"status"`
		AddBlockedBy []int  `json:"addBlockedBy"`
		AddBlocks    []int  `json:"addBlocks"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[TaskUpdateTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	log.Printf("[TaskUpdateTool] Updating task: task_id=%d, params=%s", params.TaskID, input)
	result, err := agent.TaskManager.Update(params.TaskID, params.Status, params.AddBlockedBy, params.AddBlocks)
	if err != nil {
		log.Printf("[TaskUpdateTool] Error: %v", err)
		return "", err
	}
	log.Printf("[TaskUpdateTool] Task updated successfully")
	return result, nil
}

// TaskListTool lists all tasks
func TaskListTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	log.Printf("[TaskListTool] Listing all tasks")
	result, err := agent.TaskManager.ListAll()
	if err != nil {
		log.Printf("[TaskListTool] Error: %v", err)
		return "", err
	}
	log.Printf("[TaskListTool] Task list retrieved successfully")
	return result, nil
}

// TaskGetTool gets details of a specific task
func TaskGetTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	var params struct {
		TaskID int `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[TaskGetTool] Error parsing input: %v", err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	log.Printf("[TaskGetTool] Getting task: task_id=%d", params.TaskID)
	result, err := agent.TaskManager.Get(params.TaskID)
	if err != nil {
		log.Printf("[TaskGetTool] Error: %v", err)
		return "", err
	}
	log.Printf("[TaskGetTool] Task retrieved successfully")
	return result, nil
}
