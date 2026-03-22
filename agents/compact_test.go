package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestCompactToolMessages_CompactOldKeepLatest(t *testing.T) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system"),
		openai.UserMessage("user"),
	}

	tc1 := struct {
		ID   string
		Name string
	}{ID: "tc_1", Name: "load_skill"}
	tc2 := struct {
		ID   string
		Name string
	}{ID: "tc_2", Name: "route_to_subagent"}

	messages = append(messages, assistantToolCallMessage(tc1.ID, tc1.Name))
	oldOutput := strings.Repeat("A", 120)
	messages = append(messages, openai.ToolMessage(oldOutput, tc1.ID))

	messages = append(messages, assistantToolCallMessage(tc2.ID, tc2.Name))
	latestOutput := strings.Repeat("B", 120)
	messages = append(messages, openai.ToolMessage(latestOutput, tc2.ID))

	compacted := compactToolMessages(messages)

	gotOld := mustToolContentByID(t, compacted, tc1.ID)
	wantOld := "Previous: used load_skill"
	if gotOld != wantOld {
		t.Fatalf("old tool message not compacted, got %q, want %q", gotOld, wantOld)
	}

	gotLatest := mustToolContentByID(t, compacted, tc2.ID)
	if gotLatest != latestOutput {
		t.Fatalf("latest tool message should be kept, got %q, want %q", gotLatest, latestOutput)
	}
}

func TestCompactToolMessages_DoNotCompactShortToolMessage(t *testing.T) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system"),
		openai.UserMessage("user"),
	}

	tc := struct {
		ID   string
		Name string
	}{ID: "tc_short", Name: "load_skill"}
	shortOutput := strings.Repeat("x", 100)

	messages = append(messages, assistantToolCallMessage(tc.ID, tc.Name))
	messages = append(messages, openai.ToolMessage(shortOutput, tc.ID))
	messages = append(messages, openai.AssistantMessage("done"))

	compacted := compactToolMessages(messages)
	got := mustToolContentByID(t, compacted, tc.ID)
	if got != shortOutput {
		t.Fatalf("short tool message should not be compacted, got %q, want %q", got, shortOutput)
	}
}

func assistantToolCallMessage(toolCallID, toolName string) openai.ChatCompletionMessageParamUnion {
	assistant := openai.ChatCompletionAssistantMessageParam{
		ToolCalls: []openai.ChatCompletionMessageToolCallUnionParam{
			{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: toolCallID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      toolName,
						Arguments: "{}",
					},
				},
			},
		},
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
}

func mustToolContentByID(t *testing.T, messages []openai.ChatCompletionMessageParamUnion, toolCallID string) string {
	t.Helper()
	for _, msg := range messages {
		ok, content, id, err := parseToolMessage(msg)
		if err != nil {
			t.Fatalf("parseToolMessage failed: %v", err)
		}
		if ok && id == toolCallID {
			return content
		}
	}
	t.Fatalf("tool message not found for tool_call_id=%s", toolCallID)
	return ""
}

func TestMaybeAutoCompact_WithMockSummary(t *testing.T) {
	tmp := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})

	mockSummary := "mock summary: state preserved"
	agent := &Agent{
		Model: "gpt-4o-mini",
		autoCompactSummarizer: func(ctx context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "Summarize this conversation for continuity.") {
				t.Fatalf("unexpected summarize prompt: %q", prompt)
			}
			return mockSummary, nil
		},
	}

	longText := strings.Repeat("x", autoCompactTriggerChars+10)
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system"),
		openai.UserMessage(longText),
	}

	compacted, err := agent.maybeAutoCompact(context.Background(), messages)
	if err != nil {
		t.Fatalf("maybeAutoCompact failed: %v", err)
	}
	if len(compacted) != 2 {
		t.Fatalf("expected 2 messages after compact, got %d", len(compacted))
	}

	role0, content0 := mustRoleAndContent(t, compacted[0])
	if role0 != "user" {
		t.Fatalf("expected first role user, got %q", role0)
	}
	if !strings.Contains(content0, "[Conversation compressed. Transcript: ") {
		t.Fatalf("first message missing transcript marker: %q", content0)
	}
	if !strings.Contains(content0, mockSummary) {
		t.Fatalf("first message missing summary content: %q", content0)
	}

	role1, content1 := mustRoleAndContent(t, compacted[1])
	if role1 != "assistant" {
		t.Fatalf("expected second role assistant, got %q", role1)
	}
	wantAssistant := "Understood. I have the context from the summary. Continuing."
	if content1 != wantAssistant {
		t.Fatalf("unexpected assistant continuation message, got %q", content1)
	}

	matches, err := filepath.Glob(filepath.Join(tmp, "transcripts", "transcript_*.jsonl"))
	if err != nil {
		t.Fatalf("glob transcript failed: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 transcript file, got %d (%v)", len(matches), matches)
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
