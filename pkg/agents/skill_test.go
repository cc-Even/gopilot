package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	text := "---\nname: alpha\ndescription: test skill\ntags: demo,local\n---\ncontent line\n"
	meta, body := parseFrontmatter(text)

	if meta["name"] != "alpha" {
		t.Fatalf("expected name=alpha, got %q", meta["name"])
	}
	if meta["description"] != "test skill" {
		t.Fatalf("expected description, got %q", meta["description"])
	}
	if body != "content line" {
		t.Fatalf("expected body content line, got %q", body)
	}
}

func TestParseFrontmatterCRLF(t *testing.T) {
	text := "---\r\nname: pdf\r\ndescription: Process PDF files\r\n---\r\n\r\nbody\r\n"
	meta, body := parseFrontmatter(text)

	if meta["name"] != "pdf" {
		t.Fatalf("expected name=pdf, got %q", meta["name"])
	}
	if meta["description"] != "Process PDF files" {
		t.Fatalf("expected description, got %q", meta["description"])
	}
	if body != "body" {
		t.Fatalf("expected body, got %q", body)
	}
}

func TestParseFrontmatterBlockDescription(t *testing.T) {
	text := "---\nname: agent-builder\ndescription: |\n  line one\n  line two\n---\nbody"
	meta, _ := parseFrontmatter(text)
	if meta["description"] != "line one\nline two" {
		t.Fatalf("unexpected block description: %q", meta["description"])
	}
}

func TestSkillLoader(t *testing.T) {
	root := t.TempDir()

	s1 := filepath.Join(root, "skill-one")
	if err := os.MkdirAll(s1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(s1, "SKILL.md"),
		[]byte("---\nname: alpha\ndescription: Alpha desc\ntags: x,y\n---\nalpha body"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	s2 := filepath.Join(root, "skill-two")
	if err := os.MkdirAll(s2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(s2, "SKILL.md"),
		[]byte("no frontmatter"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillLoader(root)
	descriptions := loader.GetDescriptions()
	if !strings.Contains(descriptions, "alpha: Alpha desc [x,y]") {
		t.Fatalf("unexpected descriptions: %s", descriptions)
	}
	if !strings.Contains(descriptions, "skill-two: No description") {
		t.Fatalf("unexpected descriptions: %s", descriptions)
	}

	content := loader.GetContent("alpha")
	if !strings.Contains(content, "<skill name=\"alpha\">") || !strings.Contains(content, "alpha body") {
		t.Fatalf("unexpected content: %s", content)
	}

	unknown := loader.GetContent("missing")
	if !strings.Contains(unknown, "Error: Unknown skill 'missing'. Available: ") {
		t.Fatalf("unexpected unknown message: %s", unknown)
	}
}

func TestLoadSkillToolLoadsNamedSkill(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "skill-one")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: alpha\ndescription: Alpha desc\n---\nalpha body"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillLoader(root)
	toolMap := map[string]ToolDefinition{}
	registerLoadSkillTool(toolMap, nil, loader)

	output, err := toolMap["load_skill"].Handler(context.Background(), json.RawMessage(`{"skill_name":"alpha"}`), nil)
	if err != nil {
		t.Fatalf("load_skill failed: %v", err)
	}
	if !strings.Contains(output, "<skill name=\"alpha\">") {
		t.Fatalf("unexpected load_skill output: %s", output)
	}
}

func TestLoadSkillToolRejectsMissingSkillName(t *testing.T) {
	toolMap := map[string]ToolDefinition{}
	registerLoadSkillTool(toolMap, nil, NewSkillLoader(t.TempDir()))

	_, err := toolMap["load_skill"].Handler(context.Background(), json.RawMessage(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "missing skill_name") {
		t.Fatalf("expected missing skill_name error, got %v", err)
	}
}

func TestDefaultToolDefinitionsUseStrictObjectSchemas(t *testing.T) {
	tools := DefaultToolDefinitions()
	toolByName := make(map[string]ToolDefinition, len(tools))
	for _, tool := range tools {
		toolByName[tool.Name] = tool
	}

	readSchema := toolByName["read_file"].Parameters
	if readSchema["type"] != "object" {
		t.Fatalf("read_file schema type = %v, want object", readSchema["type"])
	}
	if readSchema["additionalProperties"] != false {
		t.Fatalf("read_file additionalProperties = %v, want false", readSchema["additionalProperties"])
	}

	listSchema := toolByName["list_file"].Parameters
	if listSchema["type"] != "object" {
		t.Fatalf("list_file schema type = %v, want object", listSchema["type"])
	}
	if listSchema["additionalProperties"] != false {
		t.Fatalf("list_file additionalProperties = %v, want false", listSchema["additionalProperties"])
	}

	codeOutlineSchema := toolByName["code_outline"].Parameters
	if codeOutlineSchema["type"] != "object" {
		t.Fatalf("code_outline schema type = %v, want object", codeOutlineSchema["type"])
	}
	if codeOutlineSchema["additionalProperties"] != false {
		t.Fatalf("code_outline additionalProperties = %v, want false", codeOutlineSchema["additionalProperties"])
	}

	checkTypesSchema := toolByName["check_types"].Parameters
	if checkTypesSchema["type"] != "object" {
		t.Fatalf("check_types schema type = %v, want object", checkTypesSchema["type"])
	}
	if checkTypesSchema["additionalProperties"] != false {
		t.Fatalf("check_types additionalProperties = %v, want false", checkTypesSchema["additionalProperties"])
	}

	readFilesSchema := toolByName["read_files"].Parameters
	if readFilesSchema["type"] != "object" {
		t.Fatalf("read_files schema type = %v, want object", readFilesSchema["type"])
	}
	if readFilesSchema["additionalProperties"] != false {
		t.Fatalf("read_files additionalProperties = %v, want false", readFilesSchema["additionalProperties"])
	}

	taskUpdateSchema := toolByName["task_update"].Parameters
	properties, ok := taskUpdateSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("task_update properties has unexpected type %T", taskUpdateSchema["properties"])
	}
	statusSchema, ok := properties["status"].(map[string]any)
	if !ok {
		t.Fatalf("task_update status schema has unexpected type %T", properties["status"])
	}
	enumValues, ok := statusSchema["enum"].([]string)
	if !ok {
		t.Fatalf("task_update status enum has unexpected type %T", statusSchema["enum"])
	}
	want := []string{taskStatusPending, taskStatusInProgress, taskStatusCompleted}
	if len(enumValues) != len(want) {
		t.Fatalf("task_update status enum len = %d, want %d", len(enumValues), len(want))
	}
	for i := range want {
		if enumValues[i] != want[i] {
			t.Fatalf("task_update status enum[%d] = %q, want %q", i, enumValues[i], want[i])
		}
	}
}

func TestBatchReadFileToolReturnsRemainingRequestsWhenBudgetExceeded(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	if err := os.WriteFile(firstPath, []byte("line1\nline2\nline3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("alpha\nbeta\ngamma"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := BatchReadFileTool{}
	output, err := tool.Call(context.Background(), `{
		"files": [
			{"path":"first.txt"},
			{"path":"second.txt"}
		],
		"max_chars": 11
	}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("read_files failed: %v", err)
	}

	var response batchReadFileResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if response.Completed {
		t.Fatalf("expected incomplete response when budget exceeded")
	}
	if len(response.Files) != 1 {
		t.Fatalf("expected 1 file result, got %d", len(response.Files))
	}
	if !response.Files[0].BudgetTruncated {
		t.Fatalf("expected first file to be budget truncated")
	}
	if response.Files[0].EndLine != 2 {
		t.Fatalf("expected first file end_line=2, got %d", response.Files[0].EndLine)
	}
	if len(response.Remaining) != 2 {
		t.Fatalf("expected 2 remaining requests, got %d", len(response.Remaining))
	}
	if response.Remaining[0].Path != "first.txt" || response.Remaining[0].StartLine != 3 {
		t.Fatalf("unexpected first remaining request: %+v", response.Remaining[0])
	}
	if response.Remaining[1].Path != "second.txt" || response.Remaining[1].StartLine != 1 {
		t.Fatalf("unexpected second remaining request: %+v", response.Remaining[1])
	}
}

func TestBatchReadFileToolPreservesRemainingLineLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := BatchReadFileTool{}
	output, err := tool.Call(context.Background(), `{
		"files": [
			{"path":"file.txt","limit":3}
		],
		"max_chars": 3
	}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("read_files failed: %v", err)
	}

	var response batchReadFileResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if len(response.Remaining) != 1 {
		t.Fatalf("expected 1 remaining request, got %d", len(response.Remaining))
	}
	if response.Remaining[0].StartLine != 3 || response.Remaining[0].Limit != 1 {
		t.Fatalf("unexpected remaining request after partial limited read: %+v", response.Remaining[0])
	}
}

func TestListFileToolListsWorkspaceRootWhenPathOmitted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "root.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := ListFileTool{}
	output, err := tool.Call(context.Background(), `{}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("list_file failed: %v", err)
	}

	var response listFileResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if response.Path != "." {
		t.Fatalf("response path = %q, want .", response.Path)
	}
	if len(response.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(response.Entries))
	}
	if response.Entries[0].Path != "nested" || !response.Entries[0].IsDir {
		t.Fatalf("unexpected first entry: %+v", response.Entries[0])
	}
	if response.Entries[1].Path != "root.txt" || response.Entries[1].IsDir {
		t.Fatalf("unexpected second entry: %+v", response.Entries[1])
	}
}

func TestListFileToolListsSpecifiedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "agents", "core.go"), []byte("package agents"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := ListFileTool{}
	output, err := tool.Call(context.Background(), `{"path":"pkg"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("list_file failed: %v", err)
	}

	var response listFileResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if response.Path != "pkg" {
		t.Fatalf("response path = %q, want pkg", response.Path)
	}
	if len(response.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(response.Entries))
	}
	if response.Entries[0].Path != "pkg/agents" || !response.Entries[0].IsDir {
		t.Fatalf("unexpected entry: %+v", response.Entries[0])
	}
}
