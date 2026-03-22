package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
	Handler     func(context.Context, json.RawMessage, *OpenAIAgent) (string, error)
}

// ToolFromJSONString wraps handlers that already accept JSON string input.
func ToolFromJSONString(name, description string, parameters map[string]any, handler func(context.Context, string, *OpenAIAgent) (string, error)) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Handler: func(ctx context.Context, args json.RawMessage, agent *OpenAIAgent) (string, error) {
			return handler(ctx, string(args), agent)
		},
	}
}

// ToolFromStringArg builds a tool that reads one string field from JSON args.
func ToolFromStringArg(name, description, argName string, parameters map[string]any, handler func(context.Context, string, *OpenAIAgent) (string, error)) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Handler: func(ctx context.Context, args json.RawMessage, agent *OpenAIAgent) (string, error) {
			var params map[string]any
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid %s args: %w", name, err)
			}
			value, _ := params[argName].(string)
			if value == "" {
				return "", fmt.Errorf("%s args missing %s", name, argName)
			}
			return handler(ctx, value, agent)
		},
	}
}

type OpenAIAgent struct {
	Name         string
	Description  string
	SystemPrompt string
	BaseUrl      string
	ApiKey       string
	Model        string
	SubAgents    map[string]*OpenAIAgent
	SkillLoader  *SkillLoader

	client openai.Client
	tools  map[string]ToolDefinition
	order  []string

	autoCompactSummarizer func(context.Context, string) (string, error)
}

type AgentOptions struct {
	Desc      string
	ToolList  []ToolDefinition
	BaseUrl   string
	ApiKey    string
	SubAgents map[string]*OpenAIAgent
}

type AgentOption func(*AgentOptions)

const (
	autoCompactTriggerChars   = 80000
	autoCompactSummaryMaxChar = 80000
	autoCompactSummaryTokens  = 2000
)

func WithDesc(desc string) AgentOption {
	return func(o *AgentOptions) {
		o.Desc = desc
	}
}

func WithToolList(toolList []ToolDefinition) AgentOption {
	return func(o *AgentOptions) {
		o.ToolList = toolList
	}
}

func WithBaseUrl(baseUrl string) AgentOption {
	return func(o *AgentOptions) {
		o.BaseUrl = baseUrl
	}
}

func WithApiKey(apiKey string) AgentOption {
	return func(o *AgentOptions) {
		o.ApiKey = apiKey
	}
}

func WithSubAgents(SubAgents map[string]*OpenAIAgent) AgentOption {
	return func(o *AgentOptions) {
		o.SubAgents = SubAgents
	}
}

func registerLoadSkillTool(toolMap map[string]ToolDefinition, order []string, skillLoader *SkillLoader) []string {
	order = append(order, "load_skill")
	toolMap["load_skill"] = ToolDefinition{
		Name:        "load_skill",
		Description: "Use this method to load a skill before answering. If the question involves a specific topic, try loading a related skill first. If you don't have enough information to decide which skill to load, you can ask the user for more details.",
		Parameters: map[string]any{
			"skill_name": map[string]any{
				"type":        "string",
				"description": "name of the skill to load, choose from the provided skill list",
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *OpenAIAgent) (string, error) {
			type paramsStruct struct {
				SkillName string `json:"skill_name"`
			}
			params := paramsStruct{}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid load_skill args: %w", err)
			}
			skillContent := skillLoader.GetContent(params.SkillName)
			return skillContent, nil
		},
	}
	return order
}

func registerRouteToSubagentTool(toolMap map[string]ToolDefinition, order []string, subAgents map[string]*OpenAIAgent) []string {
	if len(subAgents) == 0 {
		return order
	}

	agentListDesc := make([]map[string]string, 0, len(subAgents))
	for subName, subAgent := range subAgents {
		agentDesc := make(map[string]string)
		agentDesc["name"] = subName
		agentDesc["description"] = subAgent.Description
		agentListDesc = append(agentListDesc, agentDesc)
	}

	order = append(order, "route_to_subagent")
	toolMap["route_to_subagent"] = ToolDefinition{
		Name:        "route_to_subagent",
		Description: fmt.Sprintf("Calling this method delegates the subtask to the sub-agent. sub-agent list: %v", agentListDesc),
		Parameters: map[string]any{
			"sub_agent_name": map[string]any{
				"type":        "string",
				"description": "selected sub-agent name"},
			"input": map[string]any{
				"type":        "string",
				"description": "detailed description of the task you gave to the sub-agent",
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *OpenAIAgent) (string, error) {
			type paramsStruct struct {
				SubAgentName string `json:"sub_agent_name"`
				Input        string `json:"input"`
			}
			params := paramsStruct{}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid route_to_subagent args: %w", err)
			}
			fmt.Println("Routing to sub-agent:", params.SubAgentName, "with input:", params.Input)
			subAgent, ok := agent.SubAgents[params.SubAgentName]
			if !ok {
				return "", fmt.Errorf("unknown sub-agent: %s", params.SubAgentName)
			}
			result, err := subAgent.Run(ctx, params.Input)
			return result, err
		},
	}
	return order
}

func registerCompactTool(toolMap map[string]ToolDefinition, order []string) []string {
	order = append(order, "compact")
	toolMap["compact"] = ToolDefinition{
		Name:        "compact",
		Description: "Trigger manual conversation compression.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"focus": map[string]any{
					"type":        "string",
					"description": "What to preserve in the summary",
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *OpenAIAgent) (string, error) {
			type paramsStruct struct {
				Focus string `json:"focus"`
			}
			params := paramsStruct{}
			if len(args) > 0 && string(args) != "null" {
				if err := json.Unmarshal(args, &params); err != nil {
					return "", fmt.Errorf("invalid compact args: %w", err)
				}
			}
			return "manual compact requested", nil
		},
	}
	return order
}

func NewOpenAIAgent(name, systemPrompt, model string, createOpts ...AgentOption) *OpenAIAgent {
	// 初始化选项
	agentOpts := &AgentOptions{}
	for _, opt := range createOpts {
		opt(agentOpts)
	}

	// 处理 ApiKey 默认值
	apiKey := agentOpts.ApiKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	// 处理 Model 默认值
	if model == "" {
		model = os.Getenv("MODEL")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 处理 BaseUrl 默认值
	baseURL := agentOpts.BaseUrl

	// 构建 OpenAI 客户端选项
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	// 处理工具列表
	toolList := agentOpts.ToolList
	if toolList == nil {
		toolList = make([]ToolDefinition, 0)
	}

	toolMap := make(map[string]ToolDefinition, len(toolList))
	order := make([]string, 0, len(toolList))
	for _, t := range toolList {
		if t.Name == "" || t.Handler == nil {
			continue
		}
		toolMap[t.Name] = t
		order = append(order, t.Name)
	}
	WORKDIR, _ := os.Getwd()
	skillLoader := NewSkillLoader(filepath.Join(WORKDIR, "skills"))
	order = registerLoadSkillTool(toolMap, order, skillLoader)

	systemPrompt += "\n Use load_skill to access specialized knowledge before tackling unfamiliar topics.\n\nSkills available:"
	systemPrompt += skillLoader.GetDescriptions()

	subAgents := agentOpts.SubAgents
	order = registerRouteToSubagentTool(toolMap, order, subAgents)
	order = registerCompactTool(toolMap, order)

	return &OpenAIAgent{
		Name:         name,
		SystemPrompt: systemPrompt,
		Description:  agentOpts.Desc,
		BaseUrl:      baseURL,
		ApiKey:       apiKey,
		Model:        model,
		SubAgents:    subAgents,
		SkillLoader:  skillLoader,
		client:       openai.NewClient(opts...),
		tools:        toolMap,
		order:        order,
	}
}

func (a *OpenAIAgent) Run(ctx context.Context, userInput string) (string, error) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(a.SystemPrompt),
		openai.UserMessage(userInput),
	}

	const maxTurns = 20
	roundsSinceTodo := 0
	for turn := 0; turn < maxTurns; turn++ {
		messages = compactToolMessages(messages)
		var compactErr error
		messages, compactErr = a.maybeAutoCompact(ctx, messages)
		if compactErr != nil {
			return "", fmt.Errorf("auto compact failed (turn=%d): %w", turn, compactErr)
		}

		resp, err := a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    a.Model,
			Messages: messages,
			Tools:    a.openAITools(),
		})
		if err != nil {
			return "", fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("empty choices from model")
		}

		usedTodo := false

		choice := resp.Choices[0]

		// 把 assistant 这条消息（含 tool_calls）放回上下文，保证下一轮可继续
		messages = append(messages, choice.Message.ToParam())

		switch choice.FinishReason {
		case "stop":
			return choice.Message.Content, nil

		case "tool_calls":
			manualCompacted := false
			for _, tc := range choice.Message.ToolCalls {
				toolName := tc.Function.Name
				toolArgs := json.RawMessage(tc.Function.Arguments)
				output, callErr := a.executeTool(ctx, toolName, toolArgs)
				if callErr != nil {
					output = "tool error: " + callErr.Error()
				}

				// 回填 tool 消息，关联 tool_call_id
				messages = append(messages, openai.ToolMessage(output, tc.ID))
				if toolName == "todo" {
					usedTodo = true
				}

				if toolName == "compact" && callErr == nil {
					focus, parseErr := parseCompactFocus(toolArgs)
					if parseErr != nil {
						return "", fmt.Errorf("manual compact args parse failed (turn=%d): %w", turn, parseErr)
					}
					messages, err = a.forceAutoCompact(ctx, messages, focus)
					if err != nil {
						return "", fmt.Errorf("manual compact failed (turn=%d): %w", turn, err)
					}
					manualCompacted = true
					roundsSinceTodo = 0
					break
				}
			}
			if manualCompacted {
				continue
			}

		default:
			return "", fmt.Errorf("unsupported finish reason: %s", choice.FinishReason)
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}
		if roundsSinceTodo >= 3 {
			messages = append(messages, openai.UserMessage("<reminder>Update your todos.</reminder>"))
		}
	}

	return "", fmt.Errorf("max turns reached without final answer")
}

func parseCompactFocus(args json.RawMessage) (string, error) {
	if len(args) == 0 || string(args) == "null" {
		return "", nil
	}
	type paramsStruct struct {
		Focus string `json:"focus"`
	}
	params := paramsStruct{}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", err
	}
	return params.Focus, nil
}

func (a *OpenAIAgent) maybeAutoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) ([]openai.ChatCompletionMessageParamUnion, error) {
	conversationText, err := marshalConversation(messages)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(conversationText) <= autoCompactTriggerChars {
		return messages, nil
	}
	return a.autoCompact(ctx, messages, conversationText, "")
}

func (a *OpenAIAgent) forceAutoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
	conversationText, err := marshalConversation(messages)
	if err != nil {
		return nil, err
	}
	return a.autoCompact(ctx, messages, conversationText, focus)
}

func (a *OpenAIAgent) autoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, conversationText, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
	workdir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory failed: %w", err)
	}
	transcriptDir := filepath.Join(workdir, "transcripts")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		return nil, fmt.Errorf("create transcript dir failed: %w", err)
	}

	transcriptPath := filepath.Join(transcriptDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	file, err := os.Create(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("create transcript file failed: %w", err)
	}
	defer file.Close()

	for _, msg := range messages {
		raw, marshalErr := json.Marshal(msg)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal transcript line failed: %w", marshalErr)
		}
		if _, writeErr := file.Write(append(raw, '\n')); writeErr != nil {
			return nil, fmt.Errorf("write transcript failed: %w", writeErr)
		}
	}
	fmt.Printf("[transcript saved: %s]\n", transcriptPath)

	conversationText = truncateByRunes(conversationText, autoCompactSummaryMaxChar)
	prompt := "Summarize this conversation for continuity. Include: " +
		"1) What was accomplished, 2) Current state, 3) Key decisions made. " +
		"Be concise but preserve critical details.\n\n" + conversationText
	if strings.TrimSpace(focus) != "" {
		prompt += "\n\nFocus to preserve: " + focus
	}

	summary, err := a.summarizeForAutoCompact(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary)),
		openai.AssistantMessage("Understood. I have the context from the summary. Continuing."),
	}, nil
}

func (a *OpenAIAgent) summarizeForAutoCompact(ctx context.Context, prompt string) (string, error) {
	if a.autoCompactSummarizer != nil {
		return a.autoCompactSummarizer(ctx, prompt)
	}

	resp, err := a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               a.Model,
		Messages:            []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
		MaxCompletionTokens: openai.Int(autoCompactSummaryTokens),
	})
	if err != nil {
		return "", fmt.Errorf("summary generation failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty summary choices from model")
	}

	return resp.Choices[0].Message.Content, nil
}

func marshalConversation(messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return "", fmt.Errorf("marshal conversation failed: %w", err)
	}
	return string(raw), nil
}

func truncateByRunes(input string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(input) <= maxRunes {
		return input
	}
	out := make([]rune, 0, maxRunes)
	for _, r := range input {
		out = append(out, r)
		if len(out) >= maxRunes {
			break
		}
	}
	return string(out)
}

func compactToolMessages(messages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	toolCallNameByID := buildToolCallNameByID(messages)

	lastToolIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if ok, _, _, _ := parseToolMessage(messages[i]); ok {
			lastToolIndex = i
			break
		}
	}
	if lastToolIndex == -1 {
		return messages
	}

	compacted := make([]openai.ChatCompletionMessageParamUnion, len(messages))
	copy(compacted, messages)
	for i := 0; i < len(compacted); i++ {
		if i == lastToolIndex {
			continue
		}
		ok, content, toolCallID, err := parseToolMessage(compacted[i])
		if err != nil || !ok {
			continue
		}
		compactedContent := toolResultCompact(content, toolCallNameByID[toolCallID])
		if compactedContent == content || toolCallID == "" {
			continue
		}
		compacted[i] = openai.ToolMessage(compactedContent, toolCallID)
	}
	return compacted
}

func buildToolCallNameByID(messages []openai.ChatCompletionMessageParamUnion) map[string]string {
	toolCallNameByID := make(map[string]string)
	for _, message := range messages {
		raw, err := json.Marshal(message)
		if err != nil {
			continue
		}
		type toolCall struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		type payload struct {
			ToolCalls []toolCall `json:"tool_calls"`
		}
		p := payload{}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		for _, tc := range p.ToolCalls {
			if tc.ID == "" || tc.Function.Name == "" {
				continue
			}
			toolCallNameByID[tc.ID] = tc.Function.Name
		}
	}
	return toolCallNameByID
}

func parseToolMessage(message openai.ChatCompletionMessageParamUnion) (bool, string, string, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return false, "", "", err
	}

	type messagePayload struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	}
	payload := messagePayload{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, "", "", err
	}
	if payload.Role != "tool" {
		return false, "", "", nil
	}
	return true, payload.Content, payload.ToolCallID, nil
}

func toolResultCompact(output, toolName string) string {
	if utf8.RuneCountInString(output) > 100 {
		if toolName == "" {
			toolName = "tool"
		}
		return fmt.Sprintf("Previous: used %s", toolName)
	}
	return output
}

func (a *OpenAIAgent) executeTool(ctx context.Context, name string, rawArgs json.RawMessage) (string, error) {
	t, ok := a.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Handler(ctx, rawArgs, a)
}

func (a *OpenAIAgent) openAITools() []openai.ChatCompletionToolUnionParam {
	if len(a.order) == 0 {
		return nil
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(a.order))
	for _, name := range a.order {
		t := a.tools[name]
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
