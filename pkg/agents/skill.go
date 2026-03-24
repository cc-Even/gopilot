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
	"unicode/utf8"
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
type BatchReadFileTool struct{}
type WriteFileTool struct{}
type EditFileTool struct{}

const (
	maxReadOutputChars       = 50000
	defaultBatchReadMaxChars = 40000
)

func StringParam() map[string]any {
	return map[string]any{"type": "string"}
}

func NonEmptyStringParam() map[string]any {
	return map[string]any{"type": "string", "minLength": 1}
}

func IntegerParam() map[string]any {
	return map[string]any{"type": "integer"}
}

func BoolParam() map[string]any {
	return map[string]any{"type": "boolean"}
}

func EnumStringParam(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func ObjectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
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
	batchRead := BatchReadFileTool{}
	write := WriteFileTool{}
	edit := EditFileTool{}

	return []ToolDefinition{
		ToolFromStringArg(
			bash.Name(),
			bash.Description(),
			"command",
			ObjectSchema(map[string]any{"command": NonEmptyStringParam()}, "command"),
			bash.Call,
		),
		ToolFromStringArg(
			backgroundRun.Name(),
			backgroundRun.Description(),
			"command",
			ObjectSchema(map[string]any{"command": NonEmptyStringParam()}, "command"),
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
			ObjectSchema(map[string]any{"path": NonEmptyStringParam(), "limit": IntegerParam()}, "path"),
			read.Call,
		),
		ToolFromJSONString(
			batchRead.Name(),
			batchRead.Description(),
			ObjectSchema(map[string]any{
				"files": map[string]any{
					"type": "array",
					"items": ObjectSchema(map[string]any{
						"path":       NonEmptyStringParam(),
						"start_line": IntegerParam(),
						"limit":      IntegerParam(),
					}, "path"),
					"minItems": 1,
				},
				"max_chars": IntegerParam(),
			}, "files"),
			batchRead.Call,
		),
		ToolFromJSONString(
			write.Name(),
			write.Description(),
			ObjectSchema(map[string]any{"path": NonEmptyStringParam(), "content": StringParam()}, "path", "content"),
			write.Call,
		),
		ToolFromJSONString(
			edit.Name(),
			edit.Description(),
			ObjectSchema(map[string]any{"path": NonEmptyStringParam(), "old_text": StringParam(), "new_text": StringParam()}, "path", "old_text", "new_text"),
			edit.Call,
		),
		ToolFromJSONString(
			"todo",
			"Update todo items. Always send the complete list in {\"items\":[...]}. Each item must include id, text, and optional status from pending, in_progress, completed.",
			ObjectSchema(map[string]any{
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"id":     IntegerParam(),
							"text":   StringParam(),
							"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						},
						"required": []string{"id", "text"},
					},
				},
			}, "items"),
			UpdateTodoTool,
		),
		// Task management tools
		ToolFromJSONString(
			"task_create",
			"Create a new task.",
			ObjectSchema(map[string]any{"subject": NonEmptyStringParam(), "description": StringParam()}, "subject"),
			TaskCreateTool,
		),
		ToolFromJSONString(
			"task_update",
			"Update a task's status or dependencies. Use status only with pending, in_progress, or completed.",
			ObjectSchema(map[string]any{
				"task_id":      IntegerParam(),
				"status":       EnumStringParam(taskStatusPending, taskStatusInProgress, taskStatusCompleted),
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
		ToolFromJSONString(
			"worktree_create",
			"Create a git worktree and optionally bind it to a task.",
			ObjectSchema(map[string]any{
				"name":    NonEmptyStringParam(),
				"task_id": IntegerParam(),
			}, "name"),
			WorktreeCreateTool,
		),
		ToolFromJSONString(
			"worktree_list",
			"List known worktrees from the registry.",
			ObjectSchema(map[string]any{}),
			WorktreeListTool,
		),
		ToolFromJSONString(
			"worktree_keep",
			"Mark a worktree as kept so the directory remains available.",
			ObjectSchema(map[string]any{"name": NonEmptyStringParam()}, "name"),
			WorktreeKeepTool,
		),
		ToolFromJSONString(
			"worktree_remove",
			"Remove a worktree directory. Optionally complete and unbind its task in the same call.",
			ObjectSchema(map[string]any{
				"name":          NonEmptyStringParam(),
				"force":         BoolParam(),
				"complete_task": BoolParam(),
			}, "name"),
			WorktreeRemoveTool,
		),
	}
}

func agentWorkspaceDir(agent *Agent) string {
	if agent != nil && strings.TrimSpace(agent.WorkDir) != "" {
		return agent.WorkDir
	}
	return WORKDIR
}

// safePath ensures the path is within the current workspace directory.
func safePath(baseDir, p string) (string, error) {
	absPath := p
	if !filepath.IsAbs(p) {
		absPath = filepath.Join(baseDir, p)
	}
	resolved, err := filepath.Abs(absPath)
	if err != nil {
		return "", err
	}
	workdirAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(workdirAbs, resolved)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("Path escapes workspace: " + p)
	}
	return resolved, nil
}

// RunBash executes a shell command safely
func RunBash(command, dir string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
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
func RunRead(baseDir, path string, limit int) string {
	resolved, err := safePath(baseDir, path)
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
	if len(result) > maxReadOutputChars {
		return result[:maxReadOutputChars]
	}
	return result
}

type batchReadFileRequest struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type batchReadFileResult struct {
	Path             string `json:"path"`
	StartLine        int    `json:"start_line"`
	EndLine          int    `json:"end_line"`
	TotalLines       int    `json:"total_lines,omitempty"`
	LineLimitApplied bool   `json:"line_limit_applied,omitempty"`
	BudgetTruncated  bool   `json:"budget_truncated,omitempty"`
	Error            string `json:"error,omitempty"`
	Content          string `json:"content,omitempty"`
}

type batchReadFileResponse struct {
	BudgetChars   int                    `json:"budget_chars"`
	ReturnedChars int                    `json:"returned_chars"`
	Completed     bool                   `json:"completed"`
	Files         []batchReadFileResult  `json:"files"`
	Remaining     []batchReadFileRequest `json:"remaining,omitempty"`
}

func clampBatchReadMaxChars(maxChars int) int {
	switch {
	case maxChars > 0 && maxChars <= maxReadOutputChars:
		return maxChars
	case maxChars > maxReadOutputChars:
		return maxReadOutputChars
	default:
		return defaultBatchReadMaxChars
	}
}

func normalizeBatchReadRequest(req batchReadFileRequest) batchReadFileRequest {
	req.Path = strings.TrimSpace(req.Path)
	if req.StartLine <= 0 {
		req.StartLine = 1
	}
	return req
}

func normalizeBatchReadRequests(requests []batchReadFileRequest) []batchReadFileRequest {
	if len(requests) == 0 {
		return nil
	}
	normalized := make([]batchReadFileRequest, 0, len(requests))
	for _, req := range requests {
		normalized = append(normalized, normalizeBatchReadRequest(req))
	}
	return normalized
}

func countRunes(text string) int {
	return utf8.RuneCountInString(text)
}

func joinLinesWithinRuneBudget(lines []string, maxRunes int) (string, int) {
	if maxRunes <= 0 || len(lines) == 0 {
		return "", 0
	}

	var b strings.Builder
	used := 0
	consumed := 0
	for i, line := range lines {
		lineRunes := countRunes(line)
		sepRunes := 0
		if i > 0 {
			sepRunes = 1
		}
		if used+sepRunes+lineRunes > maxRunes {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		used += sepRunes + lineRunes
		consumed++
	}
	return b.String(), consumed
}

// RunWrite writes content to a file safely
func RunWrite(baseDir, path, content string) string {
	resolved, err := safePath(baseDir, path)
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
func RunEdit(baseDir, path, oldText, newText string) string {
	resolved, err := safePath(baseDir, path)
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
	log.Printf("[BashTool] agent=%s Executing command: %s", agentLogName(agent), input)
	result := RunBash(input, agentWorkspaceDir(agent))
	log.Printf("[BashTool] agent=%s Command output (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
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

	log.Printf("[BackgroundRunTool] agent=%s Starting command: %s", agentLogName(agent), input)
	result := agent.Background.Run(input)
	log.Printf("[BackgroundRunTool] agent=%s Started: %s", agentLogName(agent), result)
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
			log.Printf("[BackgroundCheckTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
			return "", fmt.Errorf("invalid input: %v", err)
		}
	}

	log.Printf("[BackgroundCheckTool] agent=%s Checking task: task_id=%s", agentLogName(agent), params.TaskID)
	result := agent.Background.Check(params.TaskID)
	log.Printf("[BackgroundCheckTool] agent=%s Check result (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
	return result, nil
}

// ReadFileTool implementation
func (r ReadFileTool) Name() string {
	return "read_file"
}

func (r ReadFileTool) Description() string {
	return "Read file contents. Input must be a JSON object with path and optional limit (number of lines to read)."
}

func (r ReadFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[ReadFileTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[ReadFileTool] agent=%s Reading file: path=%s, limit=%d", agentLogName(agent), params.Path, params.Limit)
	result := RunRead(agentWorkspaceDir(agent), params.Path, params.Limit)
	log.Printf("[ReadFileTool] agent=%s File read completed (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
	return result, nil
}

func (r BatchReadFileTool) Name() string {
	return "read_files"
}

func (r BatchReadFileTool) Description() string {
	return "Read multiple files in one call. Input must be a JSON object with files=[{path,start_line?,limit?}] and optional max_chars total budget. When the total output would be too large, this tool returns partial results plus remaining file requests for the next call."
}

func (r BatchReadFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Files    []batchReadFileRequest `json:"files"`
		MaxChars int                    `json:"max_chars"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[BatchReadFileTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	if len(params.Files) == 0 {
		return "", fmt.Errorf("files is required")
	}

	response := batchReadFileResponse{
		BudgetChars: clampBatchReadMaxChars(params.MaxChars),
		Completed:   true,
		Files:       make([]batchReadFileResult, 0, len(params.Files)),
	}
	baseDir := agentWorkspaceDir(agent)

	for i, rawReq := range params.Files {
		req := normalizeBatchReadRequest(rawReq)
		if req.Path == "" {
			response.Files = append(response.Files, batchReadFileResult{
				Path:      rawReq.Path,
				StartLine: req.StartLine,
				Error:     "path is required",
			})
			continue
		}

		resolved, err := safePath(baseDir, req.Path)
		if err != nil {
			response.Files = append(response.Files, batchReadFileResult{
				Path:      req.Path,
				StartLine: req.StartLine,
				Error:     err.Error(),
			})
			continue
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			response.Files = append(response.Files, batchReadFileResult{
				Path:      req.Path,
				StartLine: req.StartLine,
				Error:     err.Error(),
			})
			continue
		}

		lines := strings.Split(string(data), "\n")
		if req.StartLine > len(lines)+1 {
			response.Files = append(response.Files, batchReadFileResult{
				Path:       req.Path,
				StartLine:  req.StartLine,
				EndLine:    req.StartLine - 1,
				TotalLines: len(lines),
				Error:      fmt.Sprintf("start_line %d out of range", req.StartLine),
			})
			continue
		}

		startIdx := req.StartLine - 1
		if startIdx > len(lines) {
			startIdx = len(lines)
		}
		endIdx := len(lines)
		lineLimitApplied := false
		if req.Limit > 0 && startIdx+req.Limit < endIdx {
			endIdx = startIdx + req.Limit
			lineLimitApplied = true
		}

		selected := lines[startIdx:endIdx]
		fullContent := strings.Join(selected, "\n")
		fullRunes := countRunes(fullContent)
		remainingBudget := response.BudgetChars - response.ReturnedChars
		if remainingBudget <= 0 {
			response.Completed = false
			response.Remaining = append(response.Remaining, req)
			response.Remaining = append(response.Remaining, normalizeBatchReadRequests(params.Files[i+1:])...)
			break
		}

		if fullRunes <= remainingBudget {
			response.Files = append(response.Files, batchReadFileResult{
				Path:             req.Path,
				StartLine:        req.StartLine,
				EndLine:          req.StartLine + len(selected) - 1,
				TotalLines:       len(lines),
				LineLimitApplied: lineLimitApplied,
				Content:          fullContent,
			})
			response.ReturnedChars += fullRunes
			continue
		}

		partialContent, consumedLines := joinLinesWithinRuneBudget(selected, remainingBudget)
		if consumedLines == 0 {
			response.Completed = false
			response.Remaining = append(response.Remaining, req)
			response.Remaining = append(response.Remaining, normalizeBatchReadRequests(params.Files[i+1:])...)
			break
		}

		response.Files = append(response.Files, batchReadFileResult{
			Path:             req.Path,
			StartLine:        req.StartLine,
			EndLine:          req.StartLine + consumedLines - 1,
			TotalLines:       len(lines),
			LineLimitApplied: lineLimitApplied,
			BudgetTruncated:  true,
			Content:          partialContent,
		})
		response.ReturnedChars += countRunes(partialContent)
		response.Completed = false

		nextReq := req
		nextReq.StartLine = req.StartLine + consumedLines
		if nextReq.Limit > 0 {
			nextReq.Limit -= consumedLines
			if nextReq.Limit < 0 {
				nextReq.Limit = 0
			}
		}
		response.Remaining = append(response.Remaining, nextReq)
		response.Remaining = append(response.Remaining, normalizeBatchReadRequests(params.Files[i+1:])...)
		break
	}

	data, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}
	log.Printf("[BatchReadFileTool] agent=%s Read files completed: completed=%t returned_chars=%d remaining=%d", agentLogName(agent), response.Completed, response.ReturnedChars, len(response.Remaining))
	return string(data), nil
}

// WriteFileTool implementation
func (w WriteFileTool) Name() string {
	return "write_file"
}

func (w WriteFileTool) Description() string {
	return "Write content to file. Input must be a JSON object with path and content."
}

func (w WriteFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[WriteFileTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[WriteFileTool] agent=%s Writing file: path=%s, content_size=%d bytes", agentLogName(agent), params.Path, len(params.Content))
	result := RunWrite(agentWorkspaceDir(agent), params.Path, params.Content)
	log.Printf("[WriteFileTool] agent=%s File write completed: %s", agentLogName(agent), result)
	return result, nil
}

// EditFileTool implementation
func (e EditFileTool) Name() string {
	return "edit_file"
}

func (e EditFileTool) Description() string {
	return "Replace exact text in file. Input must be a JSON object with path, old_text, and new_text."
}

func (e EditFileTool) Call(_ context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[EditFileTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	log.Printf("[EditFileTool] agent=%s Editing file: path=%s, old_text_size=%d, new_text_size=%d", agentLogName(agent), params.Path, len(params.OldText), len(params.NewText))
	result := RunEdit(agentWorkspaceDir(agent), params.Path, params.OldText, params.NewText)
	log.Printf("[EditFileTool] agent=%s File edit completed: %s", agentLogName(agent), result)
	return result, nil
}

// truncate returns first n characters of a string, with ellipsis if truncated
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func agentLogName(agent *Agent) string {
	if agent == nil || strings.TrimSpace(agent.Name) == "" {
		return "unknown"
	}
	return agent.Name
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
		log.Printf("[TaskCreateTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	if params.Subject == "" {
		return "", fmt.Errorf("subject is required")
	}

	log.Printf("[TaskCreateTool] agent=%s Creating task: subject=%s", agentLogName(agent), params.Subject)
	result, err := agent.TaskManager.Create(params.Subject, params.Description)
	if err != nil {
		log.Printf("[TaskCreateTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	log.Printf("[TaskCreateTool] agent=%s Task created successfully", agentLogName(agent))
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
		log.Printf("[TaskUpdateTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	log.Printf("[TaskUpdateTool] agent=%s Updating task: task_id=%d, params=%s", agentLogName(agent), params.TaskID, input)
	result, err := agent.TaskManager.Update(params.TaskID, params.Status, params.AddBlockedBy, params.AddBlocks)
	if err != nil {
		log.Printf("[TaskUpdateTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	log.Printf("[TaskUpdateTool] agent=%s Task updated successfully", agentLogName(agent))
	return result, nil
}

// TaskListTool lists all tasks
func TaskListTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	log.Printf("[TaskListTool] agent=%s Listing all tasks", agentLogName(agent))
	result, err := agent.TaskManager.ListAll()
	if err != nil {
		log.Printf("[TaskListTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	log.Printf("[TaskListTool] agent=%s Task list retrieved successfully", agentLogName(agent))
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
		log.Printf("[TaskGetTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}

	log.Printf("[TaskGetTool] agent=%s Getting task: task_id=%d", agentLogName(agent), params.TaskID)
	result, err := agent.TaskManager.Get(params.TaskID)
	if err != nil {
		log.Printf("[TaskGetTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	log.Printf("[TaskGetTool] agent=%s Task retrieved successfully", agentLogName(agent))
	return result, nil
}

func WorktreeCreateTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.WorktreeManager == nil {
		return "", fmt.Errorf("worktree manager not initialized")
	}

	var params struct {
		Name   string `json:"name"`
		TaskID *int   `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}

	record, err := agent.WorktreeManager.Create(params.Name, params.TaskID)
	if err != nil {
		return "", err
	}
	if record == nil {
		return "", fmt.Errorf("worktree create returned no result")
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func WorktreeListTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.WorktreeManager == nil {
		return "", fmt.Errorf("worktree manager not initialized")
	}

	records, err := agent.WorktreeManager.List()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func WorktreeKeepTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.WorktreeManager == nil {
		return "", fmt.Errorf("worktree manager not initialized")
	}

	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}

	record, err := agent.WorktreeManager.Keep(params.Name)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func WorktreeRemoveTool(ctx context.Context, input string, agent *Agent) (string, error) {
	if agent == nil || agent.WorktreeManager == nil {
		return "", fmt.Errorf("worktree manager not initialized")
	}

	var params struct {
		Name         string `json:"name"`
		Force        bool   `json:"force"`
		CompleteTask bool   `json:"complete_task"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}

	record, err := agent.WorktreeManager.Remove(params.Name, params.Force, params.CompleteTask)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func idleToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        "idle",
		Description: "Signal no more work.",
		Parameters:  ObjectSchema(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
			return "entering idle", nil
		},
	}
}
