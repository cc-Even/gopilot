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
	"strings"
)

type BashTool struct{}
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

// DefaultToolDefinitions returns the built-in tool list used by OpenAIAgent.
func DefaultToolDefinitions() []ToolDefinition {
	bash := BashTool{}
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
	}
}

var WORKDIR, _ = os.Getwd()

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

func (c BashTool) Call(_ context.Context, input string, agent *OpenAIAgent) (string, error) {
	log.Printf("[BashTool] Executing command: %s", input)
	result := RunBash(input)
	log.Printf("[BashTool] Command output (first 200 chars): %s", truncate(result, 200))
	return result, nil
}

// ReadFileTool implementation
func (r ReadFileTool) Name() string {
	return "read_file"
}

func (r ReadFileTool) Description() string {
	return "Read file contents. input should be a JSON string with fields: path, limit (optional, number of lines to read)."
}

func (r ReadFileTool) Call(_ context.Context, input string, agent *OpenAIAgent) (string, error) {
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

func (w WriteFileTool) Call(_ context.Context, input string, agent *OpenAIAgent) (string, error) {
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

func (e EditFileTool) Call(_ context.Context, input string, agent *OpenAIAgent) (string, error) {
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
