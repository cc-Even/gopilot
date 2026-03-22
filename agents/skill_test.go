package agents

import (
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
