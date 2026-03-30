package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const (
	llmProviderEnv          = "LLM_PROVIDER"
	geminiBackendEnv        = "GEMINI_BACKEND"
	vertexAIBaseURLEnv      = "VERTEX_AI_BASE_URL"
	vertexAIAccessTokenEnv  = "VERTEX_AI_ACCESS_TOKEN"
	vertexAIProjectIDEnv    = "VERTEX_AI_PROJECT_ID"
	vertexAILocationEnv     = "VERTEX_AI_LOCATION"
	geminiBaseURLEnv        = "GEMINI_BASE_URL"
	geminiAccessTokenEnv    = "GEMINI_ACCESS_TOKEN"
	geminiProjectIDEnv      = "GEMINI_PROJECT_ID"
	geminiLocationEnv       = "GEMINI_LOCATION"
	geminiAPIKeyEnv         = "GEMINI_API_KEY"
	googleAPIKeyEnv         = "GOOGLE_API_KEY"
	googleCloudProjectEnv   = "GOOGLE_CLOUD_PROJECT"
	googleCloudLocationEnv  = "GOOGLE_CLOUD_LOCATION"
	googleCloudRegionEnv    = "GOOGLE_CLOUD_REGION"
	defaultVertexAILocation = "global"
)

type providerKind string

const (
	providerOpenAI providerKind = "openai"
	providerGemini providerKind = "gemini"
)

type geminiBackendKind string

const (
	geminiBackendDeveloper geminiBackendKind = "developer"
	geminiBackendVertex    geminiBackendKind = "vertex"
)

type modelProvider interface {
	Generate(context.Context, modelRequest, func(modelResponse)) (*modelResponse, error)
}

type modelProviderConfig struct {
	BaseURL       string
	APIKey        string
	AccessToken   string
	ProjectID     string
	Location      string
	GeminiBackend geminiBackendKind
	HTTPClient    *http.Client
}

type modelRequest struct {
	Model               string
	Messages            []openai.ChatCompletionMessageParamUnion
	Tools               []ToolDefinition
	MaxCompletionTokens int
	Stream              bool
}

type modelResponse struct {
	Content      string
	Refusal      string
	FinishReason string
	ToolCalls    []modelToolCall
	Usage        tokenUsage
}

type modelToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type tokenUsage struct {
	PromptTokens             int64
	CompletionTokens         int64
	TotalTokens              int64
	ReasoningTokens          int64
	CachedTokens             int64
	InputAudioTokens         int64
	OutputAudioTokens        int64
	AcceptedPredictionTokens int64
	RejectedPredictionTokens int64
}

type openAIProvider struct {
	client openai.Client
}

type geminiProvider struct {
	baseURL     string
	apiKey      string
	accessToken string
	projectID   string
	location    string
	backend     geminiBackendKind
	httpClient  *http.Client

	mu     sync.Mutex
	client *genai.Client
}

func resolveProviderKind(model, baseURL string) providerKind {
	raw := strings.ToLower(strings.TrimSpace(getenvFirst(llmProviderEnv)))
	switch raw {
	case string(providerGemini):
		return providerGemini
	case string(providerOpenAI):
		return providerOpenAI
	case "":
	default:
		return providerOpenAI
	}

	model = strings.ToLower(strings.TrimSpace(model))
	baseURL = strings.ToLower(strings.TrimSpace(firstNonEmpty(
		baseURL,
		getenvFirst(vertexAIBaseURLEnv, geminiBaseURLEnv, "OPENAI_BASE_URL"),
	)))
	if strings.HasPrefix(model, "gemini") ||
		strings.Contains(model, "/publishers/") ||
		strings.Contains(baseURL, "aiplatform.googleapis.com") ||
		strings.Contains(baseURL, "generativelanguage.googleapis.com") {
		return providerGemini
	}
	return providerOpenAI
}

func resolveGeminiBackend(baseURL string) geminiBackendKind {
	raw := strings.ToLower(strings.TrimSpace(getenvFirst(geminiBackendEnv)))
	switch raw {
	case "vertex", "vertexai":
		return geminiBackendVertex
	case "developer", "geminiapi", "gemini_api", "mldev":
		return geminiBackendDeveloper
	}

	baseURL = strings.ToLower(strings.TrimSpace(firstNonEmpty(
		baseURL,
		getenvFirst(vertexAIBaseURLEnv, geminiBaseURLEnv),
	)))
	switch {
	case strings.Contains(baseURL, "aiplatform.googleapis.com"):
		return geminiBackendVertex
	case strings.Contains(baseURL, "generativelanguage.googleapis.com"):
		return geminiBackendDeveloper
	}

	if getenvFirst(
		vertexAIProjectIDEnv,
		geminiProjectIDEnv,
		googleCloudProjectEnv,
		vertexAILocationEnv,
		geminiLocationEnv,
		googleCloudLocationEnv,
		googleCloudRegionEnv,
		vertexAIAccessTokenEnv,
		geminiAccessTokenEnv,
	) != "" {
		return geminiBackendVertex
	}
	return geminiBackendDeveloper
}

func getenvFirst(keys ...string) string {
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func newModelProvider(kind providerKind, cfg modelProviderConfig) modelProvider {
	switch kind {
	case providerGemini:
		return &geminiProvider{
			baseURL:     strings.TrimSpace(cfg.BaseURL),
			apiKey:      strings.TrimSpace(cfg.APIKey),
			accessToken: strings.TrimSpace(cfg.AccessToken),
			projectID:   strings.TrimSpace(cfg.ProjectID),
			location:    strings.TrimSpace(cfg.Location),
			backend:     cfg.GeminiBackend,
			httpClient:  cfg.HTTPClient,
		}
	default:
		opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
		if strings.TrimSpace(cfg.BaseURL) != "" {
			opts = append(opts, option.WithBaseURL(cfg.BaseURL))
		}
		return &openAIProvider{client: openai.NewClient(opts...)}
	}
}

func (p *openAIProvider) Generate(ctx context.Context, req modelRequest, onUpdate func(modelResponse)) (*modelResponse, error) {
	params := openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: req.Messages,
		Tools:    buildOpenAITools(req.Tools),
	}
	if req.MaxCompletionTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxCompletionTokens))
	}

	if !req.Stream {
		resp, err := p.client.Chat.Completions.New(ctx, params)
		if err != nil {
			return nil, err
		}
		return mapOpenAICompletion(*resp), nil
	}

	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: openai.Bool(true),
	}
	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		chunk := stream.Current()
		if !acc.AddChunk(chunk) {
			return nil, fmt.Errorf("chat completion stream accumulate failed")
		}
		if onUpdate != nil {
			if current := mapOpenAICompletion(acc.ChatCompletion); current != nil {
				onUpdate(*current)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return mapOpenAICompletion(acc.ChatCompletion), nil
}

func (p *geminiProvider) Generate(ctx context.Context, req modelRequest, onUpdate func(modelResponse)) (*modelResponse, error) {
	client, err := p.clientFor(ctx)
	if err != nil {
		return nil, err
	}

	systemInstruction, contents, err := buildGeminiContents(req.Messages)
	if err != nil {
		return nil, err
	}

	config := &genai.GenerateContentConfig{}
	if systemInstruction != nil {
		config.SystemInstruction = systemInstruction
	}
	if len(req.Tools) > 0 {
		config.Tools = buildGeminiTools(req.Tools)
	}
	if req.MaxCompletionTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxCompletionTokens)
	}

	if !req.Stream {
		resp, err := client.Models.GenerateContent(ctx, req.Model, contents, config)
		if err != nil {
			return nil, err
		}
		return mapGeminiResponse(resp), nil
	}

	var merged *modelResponse
	for resp, err := range client.Models.GenerateContentStream(ctx, req.Model, contents, config) {
		if err != nil {
			return nil, err
		}
		merged = mergeModelResponse(merged, mapGeminiResponse(resp))
		if onUpdate != nil && merged != nil {
			onUpdate(*merged)
		}
	}
	return merged, nil
}

func (p *geminiProvider) clientFor(ctx context.Context) (*genai.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		return p.client, nil
	}

	cfg, err := p.buildClientConfig()
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p.client = client
	return client, nil
}

func (p *geminiProvider) buildClientConfig() (*genai.ClientConfig, error) {
	backend := p.backend
	if backend == "" {
		backend = geminiBackendDeveloper
	}

	cfg := &genai.ClientConfig{
		HTTPOptions: genai.HTTPOptions{},
	}
	if p.httpClient != nil {
		cfg.HTTPClient = p.httpClient
	}
	if strings.TrimSpace(p.baseURL) != "" {
		cfg.HTTPOptions.BaseURL = strings.TrimSpace(p.baseURL)
	}

	switch backend {
	case geminiBackendVertex:
		cfg.Backend = genai.BackendVertexAI
		cfg.Project = firstNonEmpty(p.projectID, getenvFirst(vertexAIProjectIDEnv, geminiProjectIDEnv, googleCloudProjectEnv))
		cfg.Location = firstNonEmpty(p.location, getenvFirst(vertexAILocationEnv, geminiLocationEnv, googleCloudLocationEnv, googleCloudRegionEnv), defaultVertexAILocation)
		if strings.TrimSpace(cfg.Project) == "" {
			return nil, fmt.Errorf("gemini vertex backend requires project id; set %s or %s", vertexAIProjectIDEnv, googleCloudProjectEnv)
		}

		accessToken := firstNonEmpty(p.accessToken, getenvFirst(vertexAIAccessTokenEnv, geminiAccessTokenEnv))
		if accessToken != "" {
			if cfg.HTTPOptions.Headers == nil {
				cfg.HTTPOptions.Headers = make(http.Header)
			}
			cfg.HTTPOptions.Headers.Set("Authorization", "Bearer "+accessToken)
			return cfg, nil
		}

		if err := cfg.UseDefaultCredentials(); err != nil {
			return nil, err
		}
		return cfg, nil
	default:
		cfg.Backend = genai.BackendGeminiAPI
		cfg.APIKey = firstNonEmpty(p.apiKey, getenvFirst(geminiAPIKeyEnv, googleAPIKeyEnv))
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("gemini developer backend requires api key; set %s or %s", geminiAPIKeyEnv, googleAPIKeyEnv)
		}
		return cfg, nil
	}
}

func mapOpenAICompletion(resp openai.ChatCompletion) *modelResponse {
	if len(resp.Choices) == 0 {
		return nil
	}
	choice := resp.Choices[0]
	result := &modelResponse{
		Content:      choice.Message.Content,
		Refusal:      choice.Message.Refusal,
		FinishReason: strings.TrimSpace(choice.FinishReason),
		Usage:        openAIUsageToTokenUsage(resp.Usage),
	}
	for _, tc := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, modelToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	return result
}

func openAIUsageToTokenUsage(usage openai.CompletionUsage) tokenUsage {
	return tokenUsage{
		PromptTokens:             usage.PromptTokens,
		CompletionTokens:         usage.CompletionTokens,
		TotalTokens:              usage.TotalTokens,
		ReasoningTokens:          usage.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:             usage.PromptTokensDetails.CachedTokens,
		InputAudioTokens:         usage.PromptTokensDetails.AudioTokens,
		OutputAudioTokens:        usage.CompletionTokensDetails.AudioTokens,
		AcceptedPredictionTokens: usage.CompletionTokensDetails.AcceptedPredictionTokens,
		RejectedPredictionTokens: usage.CompletionTokensDetails.RejectedPredictionTokens,
	}
}

func buildOpenAITools(toolList []ToolDefinition) []openai.ChatCompletionToolUnionParam {
	if len(toolList) == 0 {
		return nil
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(toolList))
	for _, t := range toolList {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		tools = append(tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name,
					Description: openai.String(t.Description),
					Parameters:  t.Parameters,
				},
			},
		})
	}
	return tools
}

func buildGeminiTools(toolList []ToolDefinition) []*genai.Tool {
	if len(toolList) == 0 {
		return nil
	}

	declarations := make([]*genai.FunctionDeclaration, 0, len(toolList))
	for _, t := range toolList {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		declaration := &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: strings.TrimSpace(t.Description),
		}
		if normalized := convertSchemaForGemini(t.Parameters); normalized != nil {
			declaration.ParametersJsonSchema = normalized
		}
		declarations = append(declarations, declaration)
	}
	if len(declarations) == 0 {
		return nil
	}
	return []*genai.Tool{{
		FunctionDeclarations: declarations,
	}}
}

func buildGeminiContents(messages []openai.ChatCompletionMessageParamUnion) (*genai.Content, []*genai.Content, error) {
	if len(messages) == 0 {
		return nil, nil, nil
	}

	var systemParts []*genai.Part
	var contents []*genai.Content
	toolNamesByID := make(map[string]string)

	for _, message := range messages {
		role, content, err := messageRoleAndContent(message)
		if err != nil {
			return nil, nil, err
		}

		switch role {
		case "system":
			if strings.TrimSpace(content) != "" {
				systemParts = append(systemParts, genai.NewPartFromText(content))
			}
		case "user":
			if strings.TrimSpace(content) == "" {
				continue
			}
			contents = append(contents, genai.NewContentFromText(content, genai.RoleUser))
		case "assistant":
			parts := make([]*genai.Part, 0, 1)
			if strings.TrimSpace(content) != "" {
				parts = append(parts, genai.NewPartFromText(content))
			}
			for _, tc := range message.GetToolCalls() {
				function := tc.GetFunction()
				if function == nil {
					continue
				}
				id := stringOrEmpty(tc.GetID())
				name := strings.TrimSpace(function.Name)
				if id != "" && name != "" {
					toolNamesByID[id] = name
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   id,
						Name: name,
						Args: parseJSONArguments(function.Arguments),
					},
				})
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
		case "tool":
			toolCallID := stringOrEmpty(message.GetToolCallID())
			toolName := strings.TrimSpace(toolNamesByID[toolCallID])
			if toolName == "" {
				toolName = strings.TrimSpace(toolCallID)
			}
			if toolName == "" {
				toolName = "tool"
			}
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					ID:       toolCallID,
					Name:     toolName,
					Response: parseToolResponse(content),
				},
			}}, genai.RoleUser))
		}
	}

	var systemInstruction *genai.Content
	if len(systemParts) > 0 {
		systemInstruction = &genai.Content{
			Parts: systemParts,
		}
	}
	return systemInstruction, contents, nil
}

func parseJSONArguments(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed
	}
	return map[string]any{"raw": raw}
}

func parseToolResponse(output string) map[string]any {
	output = strings.TrimSpace(output)
	if output == "" {
		return map[string]any{"output": ""}
	}
	var parsed any
	if err := json.Unmarshal([]byte(output), &parsed); err == nil {
		if obj, ok := parsed.(map[string]any); ok {
			if _, hasOutput := obj["output"]; hasOutput {
				return obj
			}
			if _, hasError := obj["error"]; hasError {
				return obj
			}
		}
		return map[string]any{"output": parsed}
	}
	return map[string]any{"output": output}
}

func convertSchemaForGemini(schema map[string]any) any {
	if len(schema) == 0 {
		return nil
	}
	return normalizeJSONValue(schema)
}

func normalizeJSONValue(input any) any {
	raw, err := json.Marshal(input)
	if err != nil {
		return input
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return input
	}
	return normalized
}

func renderStreamingPreview(resp modelResponse) string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(resp.Content) != "" {
		parts = append(parts, resp.Content)
	} else if strings.TrimSpace(resp.Refusal) != "" {
		parts = append(parts, resp.Refusal)
	}
	if len(resp.ToolCalls) > 0 {
		parts = append(parts, "调用工具:\n"+formatToolCallPreview(resp.ToolCalls))
	}
	if len(parts) == 0 {
		return "思考中..."
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func formatToolCallPreview(toolCalls []modelToolCall) string {
	lines := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		name := strings.TrimSpace(tc.Name)
		if name == "" {
			name = "(pending)"
		}
		args := compactToolDisplay(strings.TrimSpace(tc.Arguments), name)
		if args == "" {
			lines = append(lines, fmt.Sprintf("- %s", name))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s", name, args))
	}
	return strings.Join(lines, "\n")
}

func buildAssistantMessage(resp *modelResponse) openai.ChatCompletionMessageParamUnion {
	if resp == nil {
		return openai.AssistantMessage("")
	}
	var message openai.ChatCompletionMessageParamUnion
	if strings.TrimSpace(resp.Content) != "" {
		message = openai.AssistantMessage(resp.Content)
	} else {
		message = openai.ChatCompletionMessageParamUnion{
			OfAssistant: &openai.ChatCompletionAssistantMessageParam{},
		}
	}
	if message.OfAssistant == nil {
		message.OfAssistant = &openai.ChatCompletionAssistantMessageParam{}
	}
	toolCalls := normalizeModelToolCalls(resp.ToolCalls)
	if len(toolCalls) > 0 {
		message.OfAssistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(toolCalls))
		for _, tc := range toolCalls {
			message.OfAssistant.ToolCalls = append(message.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				},
			})
		}
	}
	return message
}

func collectToolList(a *Agent) []ToolDefinition {
	if a == nil || len(a.order) == 0 {
		return nil
	}
	toolList := make([]ToolDefinition, 0, len(a.order))
	for _, name := range a.order {
		t, ok := a.tools[name]
		if !ok {
			continue
		}
		toolList = append(toolList, t)
	}
	return toolList
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func mapGeminiResponse(resp *genai.GenerateContentResponse) *modelResponse {
	if resp == nil {
		return nil
	}

	result := &modelResponse{}
	if resp.UsageMetadata != nil {
		result.Usage = tokenUsage{
			PromptTokens:     int64(resp.UsageMetadata.PromptTokenCount),
			CompletionTokens: int64(resp.UsageMetadata.CandidatesTokenCount),
			TotalTokens:      int64(resp.UsageMetadata.TotalTokenCount),
			CachedTokens:     int64(resp.UsageMetadata.CachedContentTokenCount),
			ReasoningTokens:  int64(resp.UsageMetadata.ThoughtsTokenCount),
		}
	}
	if len(resp.Candidates) == 0 {
		result.FinishReason = "stop"
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			result.Refusal = string(resp.PromptFeedback.BlockReason)
		}
		return result
	}

	candidate := resp.Candidates[0]
	result.FinishReason = normalizeGeminiFinishReason(string(candidate.FinishReason))
	if strings.TrimSpace(candidate.FinishMessage) != "" {
		result.Refusal = strings.TrimSpace(candidate.FinishMessage)
	}
	if candidate.TokenCount > 0 && result.Usage.CompletionTokens == 0 {
		result.Usage.CompletionTokens = int64(candidate.TokenCount)
	}
	if candidate.Content == nil {
		return result
	}

	for _, part := range candidate.Content.Parts {
		if part == nil {
			continue
		}
		if strings.TrimSpace(part.Text) != "" {
			result.Content += part.Text
		}
		if part.FunctionCall != nil {
			args, _ := json.Marshal(part.FunctionCall.Args)
			result.ToolCalls = append(result.ToolCalls, modelToolCall{
				ID:        strings.TrimSpace(part.FunctionCall.ID),
				Name:      strings.TrimSpace(part.FunctionCall.Name),
				Arguments: string(args),
			})
		}
	}
	if len(result.ToolCalls) > 0 {
		result.ToolCalls = normalizeModelToolCalls(result.ToolCalls)
		result.FinishReason = "tool_calls"
	}
	return result
}

func normalizeModelToolCalls(toolCalls []modelToolCall) []modelToolCall {
	if len(toolCalls) == 0 {
		return nil
	}

	normalized := append([]modelToolCall(nil), toolCalls...)
	for i := range normalized {
		normalized[i].ID = normalizeToolCallID(normalized[i].ID, i)
	}
	return normalized
}

func normalizeToolCallID(id string, index int) string {
	id = strings.TrimSpace(id)
	if id != "" {
		return id
	}
	return fmt.Sprintf("tool-%d", index)
}

func mergeModelResponse(current, next *modelResponse) *modelResponse {
	if next == nil {
		return current
	}
	if current == nil {
		cloned := *next
		if len(next.ToolCalls) > 0 {
			cloned.ToolCalls = append([]modelToolCall(nil), next.ToolCalls...)
		}
		return &cloned
	}

	if strings.TrimSpace(next.Content) != "" {
		current.Content += next.Content
	}
	if strings.TrimSpace(next.Refusal) != "" {
		current.Refusal = next.Refusal
	}
	if strings.TrimSpace(next.FinishReason) != "" {
		current.FinishReason = next.FinishReason
	}
	if hasTokenUsage(next.Usage) {
		current.Usage = next.Usage
	}

	for _, tc := range next.ToolCalls {
		if containsToolCall(current.ToolCalls, tc) {
			continue
		}
		current.ToolCalls = append(current.ToolCalls, tc)
	}
	return current
}

func containsToolCall(existing []modelToolCall, target modelToolCall) bool {
	for _, item := range existing {
		if strings.TrimSpace(target.ID) != "" && item.ID == target.ID {
			return true
		}
		if item.Name == target.Name && item.Arguments == target.Arguments {
			return true
		}
	}
	return false
}

func hasTokenUsage(usage tokenUsage) bool {
	return usage.PromptTokens > 0 ||
		usage.CompletionTokens > 0 ||
		usage.TotalTokens > 0 ||
		usage.ReasoningTokens > 0 ||
		usage.CachedTokens > 0 ||
		usage.InputAudioTokens > 0 ||
		usage.OutputAudioTokens > 0 ||
		usage.AcceptedPredictionTokens > 0 ||
		usage.RejectedPredictionTokens > 0
}

func normalizeGeminiFinishReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "", "STOP", "FINISH_REASON_UNSPECIFIED":
		return "stop"
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}
