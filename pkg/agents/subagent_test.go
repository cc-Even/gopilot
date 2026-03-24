package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubAgentLoader(t *testing.T) {
	root := t.TempDir()

	reviewer := filepath.Join(root, "reviewer")
	if err := os.MkdirAll(reviewer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(reviewer, "SUBAGENT.md"),
		[]byte("---\nname: code-reviewer\ndescription: Reviews risky changes\nmodel: gpt-4.1-mini\n---\nReview code for regressions."),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	helper := filepath.Join(root, "helper")
	if err := os.MkdirAll(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(helper, "SUBAGENT.md"),
		[]byte("Handle general helper tasks."),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	loader := NewSubAgentLoader(root)
	descriptions := loader.GetDescriptions()
	if !strings.Contains(descriptions, "code-reviewer: Reviews risky changes [model=gpt-4.1-mini]") {
		t.Fatalf("unexpected descriptions: %s", descriptions)
	}
	if !strings.Contains(descriptions, "helper: No description") {
		t.Fatalf("unexpected descriptions: %s", descriptions)
	}

	agents := loader.BuildAgents("gpt-4o-mini", DefaultToolDefinitions(), nil)
	if len(agents) != 2 {
		t.Fatalf("expected 2 sub-agents, got %d", len(agents))
	}

	reviewerAgent, ok := agents["code-reviewer"]
	if !ok {
		t.Fatalf("missing code-reviewer sub-agent: %v", mapsKeys(agents))
	}
	if reviewerAgent.Description != "Reviews risky changes" {
		t.Fatalf("unexpected description: %q", reviewerAgent.Description)
	}
	if reviewerAgent.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected model: %q", reviewerAgent.Model)
	}
	if reviewerAgent.SystemPrompt != "Review code for regressions." {
		t.Fatalf("unexpected system prompt: %q", reviewerAgent.SystemPrompt)
	}

	helperAgent, ok := agents["helper"]
	if !ok {
		t.Fatalf("missing helper sub-agent: %v", mapsKeys(agents))
	}
	if helperAgent.Model != "gpt-4o-mini" {
		t.Fatalf("expected fallback model, got %q", helperAgent.Model)
	}
	if helperAgent.SystemPrompt != "Handle general helper tasks." {
		t.Fatalf("unexpected helper system prompt: %q", helperAgent.SystemPrompt)
	}
}

func mapsKeys(values map[string]*Agent) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
