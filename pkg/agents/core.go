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
	Handler     func(context.Context, json.RawMessage, *Agent) (string, error)
}

// ToolFromJSONString wraps handlers that already accept JSON string input.
func ToolFromJSONString(name, description string, parameters map[string]any, handler func(context.Context, string, *Agent) (string, error)) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
			return handler(ctx, string(args), agent)
		},
	}
}

// ToolFromStringArg builds a tool that reads one string field from JSON args.
func ToolFromStringArg(name, description, argName string, parameters map[string]any, handler func(context.Context, string, *Agent) (string, error)) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
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

type Agent struct {
	Name            string
	Description     string
	SystemPrompt    string
	BaseUrl         string
	ApiKey          string
	Model           string
	WorkDir         string
	SubAgents       map[string]*Agent
	SkillLoader     *SkillLoader
	TaskManager     *TaskManager
	WorktreeManager *WorktreeManager
	Background      *BackgroundManager
	TeamManager     *TeammateManager

	client openai.Client
	tools  map[string]ToolDefinition
	order  []string

	autoCompactSummarizer func(context.Context, string) (string, error)
	runLoopOverride       func(*Agent, context.Context, []openai.ChatCompletionMessageParamUnion) (string, error)
	stageOutputReporter   func(stage, content string)
}

type AgentOptions struct {
	Desc        string
	ToolList    []ToolDefinition
	BaseUrl     string
	ApiKey      string
	SubAgents   map[string]*Agent
	SkillLoader *SkillLoader
}

type turnEventAcks struct {
	commits   []func() error
	rollbacks []func() error
}

type AgentOption func(*AgentOptions)

const (
	autoCompactTriggerChars   = 80000
	autoCompactSummaryMaxChar = 80000
	autoCompactSummaryTokens  = 2000
)

var plannerToolAllowlist = map[string]struct{}{
	"todo":        {},
	"task_create": {},
	"task_update": {},
	"task_list":   {},
	"task_get":    {},
}

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

func WithSubAgents(SubAgents map[string]*Agent) AgentOption {
	return func(o *AgentOptions) {
		o.SubAgents = SubAgents
	}
}

func WithSkillLoader(skillLoader *SkillLoader) AgentOption {
	return func(o *AgentOptions) {
		o.SkillLoader = skillLoader
	}
}

func (a *Agent) SetStageOutputReporter(reporter func(stage, content string)) {
	if a == nil {
		return
	}
	a.stageOutputReporter = reporter
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
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
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

func registerRouteToSubagentTool(toolMap map[string]ToolDefinition, order []string, subAgents map[string]*Agent) []string {
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
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
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
			result, err := subAgent.Run(ctx, []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(subAgent.SystemPrompt),
				openai.UserMessage(params.Input),
			})
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
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
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

func NewOpenAIAgent(name, systemPrompt, model string, createOpts ...AgentOption) *Agent {
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
	if baseURL == "" {
		baseURL = os.Getenv("OPENAI_BASE_URL")
	}

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

	skillLoader := agentOpts.SkillLoader
	if skillLoader != nil {
		order = registerLoadSkillTool(toolMap, order, skillLoader)
	}

	subAgents := agentOpts.SubAgents
	taskManager, err := NewTaskManager(TASK_DIR)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize task manager: %v\n", err)
	}
	worktreeManager, err := NewWorktreeManager(REPO_ROOT, WORKTREE_DIR, taskManager)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize worktree manager: %v\n", err)
	}
	backgroundManager := NewBackgroundManager()
	order = registerRouteToSubagentTool(toolMap, order, subAgents)
	order = registerCompactTool(toolMap, order)

	agent := &Agent{
		Name:            name,
		SystemPrompt:    systemPrompt,
		Description:     agentOpts.Desc,
		BaseUrl:         baseURL,
		ApiKey:          apiKey,
		Model:           model,
		WorkDir:         WORKDIR,
		SubAgents:       subAgents,
		SkillLoader:     skillLoader,
		TaskManager:     taskManager,
		WorktreeManager: worktreeManager,
		Background:      backgroundManager,
		client:          openai.NewClient(opts...),
		tools:           toolMap,
		order:           order,
	}

	agent.TeamManager = NewTeammateManager(TEAM_DIR, agent)
	agent.order = registerTeamTools(agent.tools, agent.order)

	return agent
}

func (a *Agent) Run(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	return a.runLoop(ctx, messages)
}

func (a *Agent) RunStructured(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	planner := a.cloneWithTools(a.plannerSystemPrompt(), plannerToolAllowlist)
	plannerMessages := applySystemPrompt(messages, planner.SystemPrompt)
	plannerMessages = append(plannerMessages, openai.UserMessage(a.plannerContextMessage()))

	plan, err := planner.runLoop(ctx, plannerMessages)
	if err != nil {
		return "", fmt.Errorf("planner stage failed: %w", err)
	}
	a.reportStageOutput("planner", plan)

	executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
	executorMessages := applySystemPrompt(messages, executor.SystemPrompt)
	executorMessages = append(executorMessages, openai.UserMessage(a.executorContextMessage(plan)))

	result, err := executor.runLoop(ctx, executorMessages)
	if err != nil {
		return "", fmt.Errorf("executor stage failed: %w", err)
	}
	return result, nil
}

func (a *Agent) runLoop(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	if a != nil && a.runLoopOverride != nil {
		return a.runLoopOverride(a, ctx, messages)
	}

	const maxTurns = 40
	roundsSinceTodo := 0
	for turn := 0; turn < maxTurns; turn++ {
		var err error
		messages = compactToolMessages(messages)
		var compactErr error
		messages, compactErr = a.maybeAutoCompact(ctx, messages)
		if compactErr != nil {
			return "", fmt.Errorf("auto compact failed (turn=%d): %w", turn, compactErr)
		}
		turnAcks := &turnEventAcks{}
		messages = a.stageBackgroundNotifications(messages, turnAcks)
		messages, err = a.stageTeamInboxMessages(messages, turnAcks)
		if err != nil {
			_ = turnAcks.Rollback()
			return "", fmt.Errorf("stage turn events failed (turn=%d): %w", turn, err)
		}

		resp, err := a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    a.Model,
			Messages: messages,
			Tools:    a.openAITools(),
		})
		if err != nil {
			_ = turnAcks.Rollback()
			return "", fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
		}
		if err := turnAcks.Commit(); err != nil {
			return "", fmt.Errorf("ack turn events failed (turn=%d): %w", turn, err)
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

func (a *Agent) cloneWithTools(systemPrompt string, allowlist map[string]struct{}) *Agent {
	if a == nil {
		return nil
	}

	clone := *a
	clone.SystemPrompt = systemPrompt
	clone.tools = make(map[string]ToolDefinition)
	clone.order = make([]string, 0, len(a.order))
	for _, name := range a.order {
		if allowlist != nil {
			if _, ok := allowlist[name]; !ok {
				continue
			}
		}
		t, ok := a.tools[name]
		if !ok {
			continue
		}
		clone.tools[name] = t
		clone.order = append(clone.order, name)
	}
	return &clone
}

func (a *Agent) reportStageOutput(stage, content string) {
	if a == nil || a.stageOutputReporter == nil {
		return
	}
	a.stageOutputReporter(stage, content)
}

func (a *Agent) plannerSystemPrompt() string {
	rules := strings.Join([]string{
		"You are in Planner stage.",
		"Your only job in this stage is to produce or update the execution plan.",
		"Only use todo and task tools. Do not edit files, run shell commands, create worktrees, or delegate work.",
		"Prefer task_list/task_get before creating or updating tasks so you reuse the current board when possible.",
		"When the plan is ready, return a concise ordered plan and clearly identify the current unfinished task.",
	}, " ")
	return appendPromptSection(a.SystemPrompt, rules)
}

func (a *Agent) executorSystemPrompt() string {
	rules := strings.Join([]string{
		"You are in Executor stage.",
		"The planner stage has already produced the task list. Use it as the source of truth unless execution proves it is stale or blocked.",
		"Work through the current unfinished tasks in order, one task at a time.",
		"Keep todo/task status aligned with real progress, and only re-plan when blocked by new information.",
	}, " ")
	return appendPromptSection(a.SystemPrompt, rules)
}

func appendPromptSection(basePrompt, rules string) string {
	basePrompt = strings.TrimSpace(basePrompt)
	rules = strings.TrimSpace(rules)
	switch {
	case basePrompt == "":
		return rules
	case rules == "":
		return basePrompt
	default:
		return basePrompt + "\n\n" + rules
	}
}

func applySystemPrompt(messages []openai.ChatCompletionMessageParamUnion, systemPrompt string) []openai.ChatCompletionMessageParamUnion {
	updated := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	replaced := false
	for _, message := range messages {
		role, _, err := messageRoleAndContent(message)
		if !replaced && err == nil && role == "system" {
			updated = append(updated, openai.SystemMessage(systemPrompt))
			replaced = true
			continue
		}
		updated = append(updated, message)
	}
	if replaced {
		return updated
	}
	return append([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt)}, updated...)
}

func (a *Agent) plannerContextMessage() string {
	lines := []string{
		"<planning_rule>",
		"Create or refresh the execution plan before any implementation work starts.",
		"Use the available todo/task tools to capture the plan.",
		"</planning_rule>",
		"<current_task_board>",
		a.currentTaskBoardSummary(),
		"</current_task_board>",
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) executorContextMessage(plan string) string {
	lines := []string{
		"<planner_output>",
		strings.TrimSpace(plan),
		"</planner_output>",
		"<current_task_board>",
		a.currentTaskBoardSummary(),
		"</current_task_board>",
		"<unfinished_tasks>",
		a.unfinishedTaskSummary(),
		"</unfinished_tasks>",
		"<execution_rule>",
		"Start from the first unfinished task above and complete tasks sequentially.",
		"If a task is blocked, explain the blocker and update the task state before moving on.",
		"</execution_rule>",
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) currentTaskBoardSummary() string {
	if a == nil || a.TaskManager == nil {
		return "Task board unavailable."
	}
	result, err := a.TaskManager.ListAll()
	if err != nil {
		return "Task board unavailable: " + err.Error()
	}
	return strings.TrimSpace(result)
}

func (a *Agent) unfinishedTaskSummary() string {
	if a == nil || a.TaskManager == nil {
		return "No task manager."
	}

	tasks, err := a.TaskManager.Snapshot()
	if err != nil {
		return "Unable to load unfinished tasks: " + err.Error()
	}

	lines := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task == nil || task.Status == taskStatusCompleted {
			continue
		}
		line := fmt.Sprintf("#%d [%s] %s", task.ID, task.Status, task.Subject)
		if len(task.BlockedBy) > 0 {
			line += fmt.Sprintf(" blocked_by=%v", task.BlockedBy)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No unfinished tasks."
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) stageBackgroundNotifications(messages []openai.ChatCompletionMessageParamUnion, acks *turnEventAcks) []openai.ChatCompletionMessageParamUnion {
	if a == nil || a.Background == nil {
		return messages
	}

	notifications := a.Background.PeekNotifications()
	if len(notifications) == 0 {
		return messages
	}

	lines := make([]string, 0, len(notifications)+2)
	lines = append(lines, "<background_notifications>")
	lines = append(lines, "Completed background tasks:")
	for _, notification := range notifications {
		lines = append(lines, fmt.Sprintf("- id=%s status=%s command=%q result=%q",
			notification.TaskID,
			notification.Status,
			notification.Command,
			notification.Result,
		))
	}
	lines = append(lines, "</background_notifications>")

	taskIDs := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		taskIDs = append(taskIDs, notification.TaskID)
	}
	acks.AddCommit(func() error {
		return a.Background.AckNotifications(taskIDs)
	})

	return append(messages, openai.UserMessage(strings.Join(lines, "\n")))
}

func (a *Agent) stageTeamInboxMessages(messages []openai.ChatCompletionMessageParamUnion, acks *turnEventAcks) ([]openai.ChatCompletionMessageParamUnion, error) {
	if a == nil || a.TeamManager == nil || a.TeamManager.bus == nil || strings.TrimSpace(a.Name) == "" {
		return messages, nil
	}

	inbox, keys, err := a.TeamManager.bus.PeekInbox(a.Name)
	if err != nil {
		return messages, err
	}
	if len(inbox) == 0 {
		return messages, nil
	}
	acks.AddCommit(func() error {
		return a.TeamManager.bus.AckInbox(a.Name, keys)
	})
	return append(messages, openai.UserMessage(formatInboxMessages(inbox))), nil
}

func (a *Agent) appendTeamInboxMessages(messages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if a == nil || a.TeamManager == nil || a.TeamManager.bus == nil || strings.TrimSpace(a.Name) == "" {
		return messages
	}

	inbox := a.TeamManager.bus.ReadInbox(a.Name)
	if len(inbox) == 0 {
		return messages
	}
	return append(messages, openai.UserMessage(formatInboxMessages(inbox)))
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

func (a *Agent) maybeAutoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) ([]openai.ChatCompletionMessageParamUnion, error) {
	conversationText, err := marshalConversation(messages)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(conversationText) <= autoCompactTriggerChars {
		return messages, nil
	}
	return a.autoCompact(ctx, messages, conversationText, "")
}

func (a *Agent) forceAutoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
	conversationText, err := marshalConversation(messages)
	if err != nil {
		return nil, err
	}
	return a.autoCompact(ctx, messages, conversationText, focus)
}

func (a *Agent) injectIdentityBlockIfCompacted(messages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if a == nil || len(messages) > 3 {
		return messages
	}

	role, _, err := messageRoleAndContent(messages[0])
	if err == nil && role == "system" {
		return messages
	}

	identity := strings.TrimSpace(a.identityBlock())
	if identity == "" {
		return messages
	}

	return append([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage(identity)}, messages...)
}

func (a *Agent) identityBlock() string {
	if a == nil {
		return ""
	}

	lines := []string{
		"<identity>",
		fmt.Sprintf("name=%s", strings.TrimSpace(a.Name)),
		fmt.Sprintf("role=%s", strings.TrimSpace(a.Description)),
	}
	if strings.TrimSpace(a.SystemPrompt) != "" {
		lines = append(lines, "instruction="+a.SystemPrompt)
	}
	lines = append(lines, "</identity>")
	return strings.Join(lines, "\n")
}

func (a *Agent) autoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, conversationText, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
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

func (a *Agent) summarizeForAutoCompact(ctx context.Context, prompt string) (string, error) {
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

func messageRoleAndContent(message openai.ChatCompletionMessageParamUnion) (string, string, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return "", "", err
	}

	type messagePayload struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := messagePayload{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", err
	}
	return payload.Role, payload.Content, nil
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

func (a *Agent) executeTool(ctx context.Context, name string, rawArgs json.RawMessage) (string, error) {
	t, ok := a.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Handler(ctx, rawArgs, a)
}

func (a *Agent) openAITools() []openai.ChatCompletionToolUnionParam {
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

func (a *turnEventAcks) AddCommit(fn func() error) {
	if a == nil || fn == nil {
		return
	}
	a.commits = append(a.commits, fn)
}

func (a *turnEventAcks) AddRollback(fn func() error) {
	if a == nil || fn == nil {
		return
	}
	a.rollbacks = append(a.rollbacks, fn)
}

func (a *turnEventAcks) Commit() error {
	if a == nil {
		return nil
	}
	for _, fn := range a.commits {
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			return err
		}
	}
	a.commits = nil
	a.rollbacks = nil
	return nil
}

func (a *turnEventAcks) Rollback() error {
	if a == nil {
		return nil
	}
	for i := len(a.rollbacks) - 1; i >= 0; i-- {
		fn := a.rollbacks[i]
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			return err
		}
	}
	a.commits = nil
	a.rollbacks = nil
	return nil
}
