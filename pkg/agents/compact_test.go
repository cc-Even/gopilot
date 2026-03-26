package agents

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestSplitMessagesForAutoCompact_PreservesSystemPrefixAndRecentTail(t *testing.T) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system-1"),
		openai.SystemMessage("system-2"),
		openai.UserMessage("older-user"),
		openai.AssistantMessage("older-assistant"),
		openai.UserMessage("recent-user"),
		openai.AssistantMessage("recent-assistant"),
	}

	systemMessages, summarizeMessages, recentMessages, err := splitMessagesForAutoCompact(messages)
	if err != nil {
		t.Fatalf("splitMessagesForAutoCompact failed: %v", err)
	}

	if len(systemMessages) != 2 {
		t.Fatalf("expected 2 system messages, got %d", len(systemMessages))
	}
	if len(summarizeMessages) != 2 {
		t.Fatalf("expected 2 summarized messages, got %d", len(summarizeMessages))
	}
	if len(recentMessages) != 2 {
		t.Fatalf("expected 2 recent messages, got %d", len(recentMessages))
	}

	if role, content := mustRoleAndContent(t, systemMessages[0]); role != "system" || content != "system-1" {
		t.Fatalf("unexpected first system message: role=%q content=%q", role, content)
	}
	if role, content := mustRoleAndContent(t, recentMessages[0]); role != "user" || content != "recent-user" {
		t.Fatalf("unexpected recent user message: role=%q content=%q", role, content)
	}
	if role, content := mustRoleAndContent(t, recentMessages[1]); role != "assistant" || content != "recent-assistant" {
		t.Fatalf("unexpected recent assistant message: role=%q content=%q", role, content)
	}
}

func TestMaybeAutoCompact_WithMockSummary(t *testing.T) {
	tmp := t.TempDir()
	originalTranscriptDir := TRANSCRIPT_DIR
	TRANSCRIPT_DIR = filepath.Join(tmp, "transcripts")
	t.Cleanup(func() {
		TRANSCRIPT_DIR = originalTranscriptDir
	})

	mockSummary := "mock summary: state preserved"
	var summarizePrompt string
	agent := &Agent{
		Model: "gpt-4o-mini",
		autoCompactSummarizer: func(ctx context.Context, prompt string) (string, error) {
			summarizePrompt = prompt
			if !strings.Contains(prompt, "Summarize the older portion of this conversation for continuity") {
				t.Fatalf("unexpected summarize prompt: %q", prompt)
			}
			return mockSummary, nil
		},
	}

	oldUser := strings.Repeat("u", 30000)
	oldAssistant := strings.Repeat("a", 30000)
	recentUser := strings.Repeat("r", 25000)
	recentAssistant := strings.Repeat("s", 25000)
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system"),
		openai.UserMessage(oldUser),
		openai.AssistantMessage(oldAssistant),
		openai.UserMessage(recentUser),
		openai.AssistantMessage(recentAssistant),
	}

	compacted, err := agent.maybeAutoCompact(context.Background(), messages)
	if err != nil {
		t.Fatalf("maybeAutoCompact failed: %v", err)
	}
	if len(compacted) != 5 {
		t.Fatalf("expected 5 messages after compact, got %d", len(compacted))
	}

	role0, content0 := mustRoleAndContent(t, compacted[0])
	if role0 != "system" || content0 != "system" {
		t.Fatalf("expected system prompt to be preserved, got role=%q content=%q", role0, content0)
	}

	role1, content1 := mustRoleAndContent(t, compacted[1])
	if role1 != "user" {
		t.Fatalf("expected summary marker as user message, got %q", role1)
	}
	if !strings.Contains(content1, "[Conversation compressed. Transcript: ") {
		t.Fatalf("summary marker missing transcript marker: %q", content1)
	}
	if !strings.Contains(content1, mockSummary) {
		t.Fatalf("summary marker missing summary content: %q", content1)
	}

	role2, content2 := mustRoleAndContent(t, compacted[2])
	if role2 != "assistant" {
		t.Fatalf("expected continuation assistant message, got %q", role2)
	}
	wantAssistant := "Understood. I have the context from the summary. Continuing."
	if content2 != wantAssistant {
		t.Fatalf("unexpected assistant continuation message, got %q", content2)
	}

	role3, content3 := mustRoleAndContent(t, compacted[3])
	if role3 != "user" || content3 != recentUser {
		t.Fatalf("expected recent user message to be preserved verbatim, got role=%q content=%q", role3, content3)
	}

	role4, content4 := mustRoleAndContent(t, compacted[4])
	if role4 != "assistant" || content4 != recentAssistant {
		t.Fatalf("expected recent assistant message to be preserved verbatim, got role=%q content=%q", role4, content4)
	}

	if !strings.Contains(summarizePrompt, oldUser) || !strings.Contains(summarizePrompt, oldAssistant) {
		t.Fatalf("summarize prompt missing older messages")
	}
	if strings.Contains(summarizePrompt, recentUser) || strings.Contains(summarizePrompt, recentAssistant) {
		t.Fatalf("summarize prompt should exclude preserved recent messages: %q", summarizePrompt)
	}

	matches, err := filepath.Glob(filepath.Join(TRANSCRIPT_DIR, "transcript_*.jsonl"))
	if err != nil {
		t.Fatalf("glob transcript failed: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 transcript file, got %d (%v)", len(matches), matches)
	}
}

func TestInjectIdentityBlockIfCompacted(t *testing.T) {
	agent := &Agent{
		Name:         "worker-1",
		Description:  "reviewer",
		SystemPrompt: "inspect core changes",
	}

	compacted := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("[Conversation compressed. Transcript: /tmp/transcript.jsonl]\n\nsummary"),
		openai.AssistantMessage("Understood. I have the context from the summary. Continuing."),
	}

	updated := agent.injectIdentityBlockIfCompacted(compacted)
	if len(updated) != 3 {
		t.Fatalf("expected identity block to be prepended, got %d messages", len(updated))
	}

	role, content := mustRoleAndContent(t, updated[0])
	if role != "system" {
		t.Fatalf("expected prepended system message, got %q", role)
	}
	if !strings.Contains(content, "<identity>") || !strings.Contains(content, "name=worker-1") || !strings.Contains(content, "instruction=inspect core changes") {
		t.Fatalf("unexpected identity block: %q", content)
	}
}

func mustRoleAndContent(t *testing.T, msg openai.ChatCompletionMessageParamUnion) (string, string) {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message failed: %v", err)
	}
	var payload struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal message failed: %v", err)
	}
	return payload.Role, payload.Content
}
