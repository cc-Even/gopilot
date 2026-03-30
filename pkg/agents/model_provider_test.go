package agents

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/openai/openai-go/v3"
)

func TestBuildGeminiContentsMapsToolCallsAndResponses(t *testing.T) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("system prompt"),
		openai.UserMessage("find current task"),
		buildAssistantMessage(&modelResponse{
			ToolCalls: []modelToolCall{{
				ID:        "call-1",
				Name:      "task_get",
				Arguments: `{"id":1}`,
			}},
		}),
		openai.ToolMessage(`{"subject":"demo"}`, "call-1"),
	}

	systemInstruction, contents, err := buildGeminiContents(messages)
	if err != nil {
		t.Fatalf("buildGeminiContents failed: %v", err)
	}
	if systemInstruction == nil || len(systemInstruction.Parts) != 1 || systemInstruction.Parts[0].Text != "system prompt" {
		t.Fatalf("unexpected system instruction: %#v", systemInstruction)
	}
	if len(contents) != 3 {
		t.Fatalf("unexpected content count: %d", len(contents))
	}
	if contents[0].Role != "user" || contents[0].Parts[0].Text != "find current task" {
		t.Fatalf("unexpected user content: %#v", contents[0])
	}
	if contents[1].Role != "model" || contents[1].Parts[0].FunctionCall == nil {
		t.Fatalf("unexpected model content: %#v", contents[1])
	}
	if contents[1].Parts[0].FunctionCall.Name != "task_get" {
		t.Fatalf("unexpected function call name: %#v", contents[1].Parts[0].FunctionCall)
	}
	if contents[2].Role != "user" || contents[2].Parts[0].FunctionResponse == nil {
		t.Fatalf("unexpected tool response content: %#v", contents[2])
	}
	if contents[2].Parts[0].FunctionResponse.Name != "task_get" {
		t.Fatalf("unexpected function response name: %#v", contents[2].Parts[0].FunctionResponse)
	}
}

func TestBuildGeminiContentsPreservesToolNameWhenToolCallIDIsMissing(t *testing.T) {
	resp := mapGeminiResponse(&genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						Name: "task_get",
						Args: map[string]any{"id": 1},
					},
				}},
			},
		}},
	})
	if resp == nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("unexpected model response: %#v", resp)
	}
	if resp.ToolCalls[0].ID == "" {
		t.Fatalf("expected synthesized tool call id, got empty")
	}

	assistant := buildAssistantMessage(resp)
	toolCall := assistant.OfAssistant.ToolCalls[0].OfFunction
	systemInstruction, contents, err := buildGeminiContents([]openai.ChatCompletionMessageParamUnion{
		assistant,
		openai.ToolMessage(`{"subject":"demo"}`, toolCall.ID),
	})
	if err != nil {
		t.Fatalf("buildGeminiContents failed: %v", err)
	}
	if systemInstruction != nil {
		t.Fatalf("unexpected system instruction: %#v", systemInstruction)
	}
	if len(contents) != 2 {
		t.Fatalf("unexpected content count: %d", len(contents))
	}
	response := contents[1].Parts[0].FunctionResponse
	if response == nil || response.Name != "task_get" {
		t.Fatalf("unexpected function response: %#v", response)
	}
}

func TestConvertSchemaForGeminiPreservesJSONSchema(t *testing.T) {
	converted := convertSchemaForGemini(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type": "string",
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "integer",
				},
			},
		},
		"additionalProperties": false,
	})

	raw, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("marshal converted schema failed: %v", err)
	}
	payload := string(raw)
	for _, expected := range []string{`"type":"object"`, `"type":"string"`, `"type":"array"`, `"type":"integer"`} {
		if !strings.Contains(payload, expected) {
			t.Fatalf("converted schema missing %s: %s", expected, payload)
		}
	}
	if !strings.Contains(payload, `"additionalProperties":false`) {
		t.Fatalf("converted schema should preserve additionalProperties: %s", payload)
	}
}

func TestResolveProviderKindDetectsGeminiModel(t *testing.T) {
	t.Setenv(llmProviderEnv, "")

	if got := resolveProviderKind("gemini-2.5-flash", ""); got != providerGemini {
		t.Fatalf("expected gemini provider, got %s", got)
	}
	if got := resolveProviderKind("gpt-4o-mini", ""); got != providerOpenAI {
		t.Fatalf("expected openai provider, got %s", got)
	}
}

func TestResolveProviderKindHonorsExplicitOpenAIEnv(t *testing.T) {
	t.Setenv(llmProviderEnv, string(providerOpenAI))

	if got := resolveProviderKind("gemini-2.5-flash", "https://generativelanguage.googleapis.com"); got != providerOpenAI {
		t.Fatalf("expected explicit openai provider, got %s", got)
	}
}

func TestResolveGeminiBackend(t *testing.T) {
	t.Setenv(geminiBackendEnv, "")
	t.Setenv(vertexAIBaseURLEnv, "")
	t.Setenv(geminiBaseURLEnv, "")
	t.Setenv(vertexAIProjectIDEnv, "")
	t.Setenv(geminiProjectIDEnv, "")
	t.Setenv(vertexAILocationEnv, "")
	t.Setenv(geminiLocationEnv, "")
	t.Setenv(vertexAIAccessTokenEnv, "")
	t.Setenv(geminiAccessTokenEnv, "")
	t.Setenv(geminiAPIKeyEnv, "")

	if got := resolveGeminiBackend("https://generativelanguage.googleapis.com"); got != geminiBackendDeveloper {
		t.Fatalf("expected developer backend, got %s", got)
	}

	t.Setenv(vertexAIProjectIDEnv, "demo-project")
	if got := resolveGeminiBackend(""); got != geminiBackendVertex {
		t.Fatalf("expected vertex backend, got %s", got)
	}
}

func TestResolveGeminiBackendUsesEnvBaseURL(t *testing.T) {
	t.Setenv(geminiBackendEnv, "")
	t.Setenv(vertexAIProjectIDEnv, "")
	t.Setenv(geminiProjectIDEnv, "")
	t.Setenv(vertexAILocationEnv, "")
	t.Setenv(geminiLocationEnv, "")
	t.Setenv(vertexAIAccessTokenEnv, "")
	t.Setenv(geminiAccessTokenEnv, "")
	t.Setenv(vertexAIBaseURLEnv, "https://aiplatform.googleapis.com")
	t.Setenv(geminiBaseURLEnv, "")

	if got := resolveGeminiBackend(""); got != geminiBackendVertex {
		t.Fatalf("expected vertex backend from env base url, got %s", got)
	}
}

func TestBuildGeminiClientConfigDeveloper(t *testing.T) {
	provider := &geminiProvider{
		backend: geminiBackendDeveloper,
		apiKey:  "test-key",
		baseURL: "https://generativelanguage.googleapis.com",
	}

	cfg, err := provider.buildClientConfig()
	if err != nil {
		t.Fatalf("buildClientConfig failed: %v", err)
	}
	if cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("unexpected backend: %v", cfg.Backend)
	}
	if cfg.APIKey != "test-key" {
		t.Fatalf("unexpected api key: %q", cfg.APIKey)
	}
	if cfg.HTTPOptions.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("unexpected base url: %q", cfg.HTTPOptions.BaseURL)
	}
}

func TestBuildGeminiClientConfigVertexAccessToken(t *testing.T) {
	provider := &geminiProvider{
		backend:     geminiBackendVertex,
		projectID:   "demo-project",
		location:    "global",
		accessToken: "token-123",
		httpClient:  &http.Client{},
	}

	cfg, err := provider.buildClientConfig()
	if err != nil {
		t.Fatalf("buildClientConfig failed: %v", err)
	}
	if cfg.Backend != genai.BackendVertexAI {
		t.Fatalf("unexpected backend: %v", cfg.Backend)
	}
	if cfg.Project != "demo-project" || cfg.Location != "global" {
		t.Fatalf("unexpected vertex config: project=%q location=%q", cfg.Project, cfg.Location)
	}
	if got := cfg.HTTPOptions.Headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("unexpected auth header: %q", got)
	}
}

func TestNewAgentUsesGeminiEnvBaseURLForBackendDetection(t *testing.T) {
	t.Setenv(llmProviderEnv, "")
	t.Setenv(geminiBackendEnv, "")
	t.Setenv(vertexAIBaseURLEnv, "https://aiplatform.googleapis.com")
	t.Setenv(geminiBaseURLEnv, "")
	t.Setenv(vertexAIProjectIDEnv, "")
	t.Setenv(geminiProjectIDEnv, "")
	t.Setenv(vertexAILocationEnv, "")
	t.Setenv(geminiLocationEnv, "")
	t.Setenv(vertexAIAccessTokenEnv, "")
	t.Setenv(geminiAccessTokenEnv, "")

	agent := NewAgent("tester", "prompt", "gemini-2.5-flash")
	provider, ok := agent.provider.(*geminiProvider)
	if !ok {
		t.Fatalf("expected gemini provider, got %#v", agent.provider)
	}
	if provider.backend != geminiBackendVertex {
		t.Fatalf("expected vertex backend, got %s", provider.backend)
	}
}
