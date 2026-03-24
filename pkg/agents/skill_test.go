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
