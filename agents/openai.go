package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
}

type AgentOptions struct {
	Desc      string
	ToolList  []ToolDefinition
	BaseUrl   string
	ApiKey    string
	SubAgents map[string]*OpenAIAgent
}

type AgentOption func(*AgentOptions)

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

	skillLoader := NewSkillLoader("../skills")
	order = registerLoadSkillTool(toolMap, order, skillLoader)

	systemPrompt += "\n Use load_skill to access specialized knowledge before tackling unfamiliar topics.\n\nSkills available:"
	systemPrompt += skillLoader.GetDescriptions()

	subAgents := agentOpts.SubAgents
	order = registerRouteToSubagentTool(toolMap, order, subAgents)

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
	rounds_since_todo := 0
	for turn := 0; turn < maxTurns; turn++ {
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
			}

		default:
			return "", fmt.Errorf("unsupported finish reason: %s", choice.FinishReason)
		}

		if usedTodo {
			rounds_since_todo = 0
		} else {
			rounds_since_todo++
		}
		if rounds_since_todo >= 3 {
			messages = append(messages, openai.UserMessage("<reminder>Update your todos.</reminder>"))
		}
	}

	return "", fmt.Errorf("max turns reached without final answer")
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
