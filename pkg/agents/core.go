package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	InheritModel    bool
	LiveOutputID    string
	LiveOutputTitle string
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
	liveOutputReporter    func(id, title, content string, done bool)
	runStage              string
}

type StructuredRunStatus string

const (
	RunPaused StructuredRunStatus = "paused"
)

type StructuredPauseInfo struct {
	Kind     string
	Question string
}

type StructuredRunState struct {
	Status           StructuredRunStatus
	Stage            string
	Plan             string
	BaseMessages     []openai.ChatCompletionMessageParamUnion
	PlannerMessages  []openai.ChatCompletionMessageParamUnion
	ExecutorMessages []openai.ChatCompletionMessageParamUnion
	Pause            *StructuredPauseInfo
}

type StructuredRunError struct {
	Stage  string
	Cause  error
	Resume *StructuredRunState
}

func (e *StructuredRunError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Stage) == "" {
		if e.Cause == nil {
			return "structured run failed"
		}
		return e.Cause.Error()
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s stage failed", e.Stage)
	}
	return fmt.Sprintf("%s stage failed: %v", e.Stage, e.Cause)
}

func (e *StructuredRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
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

type PlanningPolicy string

const (
	openAIRateLimitRetryEnv     = "OPENAI_RATE_LIMIT_RETRY_SECONDS"
	planningReminderTurns       = 6
	agentMaxTurnsEnv            = "AGENT_MAX_TURNS"
	autoCompactTriggerCharsEnv  = "AUTO_COMPACT_TRIGGER_CHARS"
	autoCompactSummaryTokensEnv = "AUTO_COMPACT_SUMMARY_MAX_TOKENS"

	agentMaxTurnsDefault            = 999
	autoCompactTriggerCharsDefault  = 100000
	autoCompactSummaryTokensDefault = 20000

	PlanningPolicyAuto     PlanningPolicy = "auto"
	PlanningPolicyRequired PlanningPolicy = "required"
	PlanningPolicySkip     PlanningPolicy = "skip"
)

var plannerToolAllowlist = map[string]struct{}{
	"task_create":         {},
	"task_update":         {},
	"task_list":           {},
	"task_get":            {},
	"list_file":           {},
	"repo_map":            {},
	"read_file":           {},
	"bash":                {},
	"ask_user":            {},
	"handoff_to_executor": {},
}

var tokenLogMu sync.Mutex
var rateLimitSleep = sleepWithContext

type runPausedError struct {
	Stage    string
	Kind     string
	Question string
}

func (e *runPausedError) Error() string {
	if e == nil {
		return ""
	}
	question := strings.TrimSpace(e.Question)
	if question == "" {
		question = "run paused"
	}
	return question
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

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func openAIRateLimitRetryDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv(openAIRateLimitRetryEnv))
	if raw == "" {
		return 0
	}

	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("[OpenAI429Retry] invalid %s value %q: %v", openAIRateLimitRetryEnv, raw, err)
		return 0
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func intEnvOrDefault(name string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("[EnvConfig] invalid %s value %q: %v (using default %d)", name, raw, err, defaultValue)
		return defaultValue
	}
	if value <= 0 {
		log.Printf("[EnvConfig] invalid %s value %q: must be > 0 (using default %d)", name, raw, defaultValue)
		return defaultValue
	}
	return value
}

func maxTurnsLimit() int {
	return intEnvOrDefault(agentMaxTurnsEnv, agentMaxTurnsDefault)
}

func autoCompactTriggerThreshold() int {
	return intEnvOrDefault(autoCompactTriggerCharsEnv, autoCompactTriggerCharsDefault)
}

func autoCompactSummaryMaxTokens() int {
	return intEnvOrDefault(autoCompactSummaryTokensEnv, autoCompactSummaryTokensDefault)
}

func isOpenAIRateLimitError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests
	}
	return false
}

func withOpenAIRateLimitRetry[T any](ctx context.Context, label string, fn func() (T, error)) (T, error) {
	var zero T
	delay := openAIRateLimitRetryDelay()
	if delay <= 0 {
		return fn()
	}

	attempt := 1
	for {
		result, err := fn()
		if err == nil || !isOpenAIRateLimitError(err) {
			return result, err
		}

		log.Printf(
			"[OpenAI429Retry] call=%s attempt=%d wait_seconds=%.3f",
			strings.TrimSpace(label),
			attempt,
			delay.Seconds(),
		)
		if sleepErr := rateLimitSleep(ctx, delay); sleepErr != nil {
			return zero, sleepErr
		}
		attempt++
	}
}

func (a *Agent) SetStageOutputReporter(reporter func(stage, content string)) {
	if a == nil {
		return
	}
	a.stageOutputReporter = reporter
}

func (a *Agent) SetLiveOutputReporter(reporter func(id, title, content string, done bool)) {
	if a == nil {
		return
	}
	a.liveOutputReporter = reporter
}

func registerLoadSkillTool(toolMap map[string]ToolDefinition, order []string, skillLoader *SkillLoader) []string {
	order = append(order, "load_skill")
	toolMap["load_skill"] = ToolDefinition{
		Name:        "load_skill",
		Description: "Load a skill by exact name before answering. Always pass {\"skill_name\":\"...\"} and choose only from the provided skill list.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"skill_name": map[string]any{
					"type":        "string",
					"description": "Exact skill name from the provided skill list",
					"minLength":   1,
				},
			},
			"required": []string{"skill_name"},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
			type paramsStruct struct {
				SkillName string `json:"skill_name"`
			}
			params := paramsStruct{}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid load_skill args: %w", err)
			}
			params.SkillName = strings.TrimSpace(params.SkillName)
			if params.SkillName == "" {
				return "", fmt.Errorf("load_skill args missing skill_name")
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

	names := make([]string, 0, len(subAgents))
	for subName := range subAgents {
		names = append(names, subName)
	}
	sort.Strings(names)

	agentListDesc := make([]map[string]string, 0, len(subAgents))
	for _, subName := range names {
		subAgent := subAgents[subName]
		agentDesc := make(map[string]string)
		agentDesc["name"] = subName
		agentDesc["description"] = subAgent.Description
		agentListDesc = append(agentListDesc, agentDesc)
	}

	order = append(order, "route_to_subagent")
	toolMap["route_to_subagent"] = ToolDefinition{
		Name:        "route_to_subagent",
		Description: fmt.Sprintf("Delegate a subtask to one sub-agent. Always pass {\"sub_agent_name\":\"...\",\"input\":\"...\"}. Available sub-agents: %v", agentListDesc),
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"sub_agent_name": map[string]any{
					"type":        "string",
					"description": "Exact sub-agent name from the available list",
					"minLength":   1,
					"enum":        names,
				},
				"input": map[string]any{
					"type":        "string",
					"description": "Detailed task description for the selected sub-agent",
					"minLength":   1,
				},
			},
			"required": []string{"sub_agent_name", "input"},
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
			params.SubAgentName = strings.TrimSpace(params.SubAgentName)
			params.Input = strings.TrimSpace(params.Input)
			if params.SubAgentName == "" {
				return "", fmt.Errorf("route_to_subagent args missing sub_agent_name")
			}
			if params.Input == "" {
				return "", fmt.Errorf("route_to_subagent args missing input")
			}
			resolvedName, subAgent, ok := resolveSubAgent(agent.SubAgents, params.SubAgentName)
			if !ok {
				return "", fmt.Errorf(
					"unknown sub-agent: %s. Available: %s",
					strings.TrimSpace(params.SubAgentName),
					strings.Join(availableSubAgentNames(agent.SubAgents), ", "),
				)
			}
			agent.reportStageOutput(
				fmt.Sprintf("SubAgent %s", resolvedName),
				fmt.Sprintf("开始处理子任务:\n%s", params.Input),
			)
			runner := subAgent.cloneWithTools(subAgent.SystemPrompt, nil)
			if runner == nil {
				return "", fmt.Errorf("failed to clone sub-agent: %s", resolvedName)
			}
			if runner.InheritModel {
				runner.Model = agent.Model
			}
			runner.LiveOutputID = fmt.Sprintf("subagent:%s", resolvedName)
			runner.LiveOutputTitle = fmt.Sprintf("SubAgent %s", resolvedName)
			result, err := runner.Run(ctx, []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(runner.SystemPrompt),
				openai.UserMessage(params.Input),
			})
			if err != nil {
				agent.reportStageOutput(
					fmt.Sprintf("SubAgent %s", resolvedName),
					fmt.Sprintf("执行失败:\n%s", err.Error()),
				)
				return "", err
			}
			agent.reportStageOutput(
				fmt.Sprintf("SubAgent %s", resolvedName),
				fmt.Sprintf("输出结果:\n%s", strings.TrimSpace(result)),
			)
			return result, err
		},
	}
	return order
}

func resolveSubAgent(subAgents map[string]*Agent, requested string) (string, *Agent, bool) {
	if len(subAgents) == 0 {
		return "", nil, false
	}

	name := strings.TrimSpace(requested)
	if name == "" {
		return "", nil, false
	}
	if subAgent, ok := subAgents[name]; ok {
		return name, subAgent, true
	}

	target := normalizeSubAgentLookupKey(name)
	if target == "" {
		return "", nil, false
	}

	names := availableSubAgentNames(subAgents)
	for _, candidate := range names {
		if normalizeSubAgentLookupKey(candidate) == target {
			return candidate, subAgents[candidate], true
		}
	}
	return "", nil, false
}

func availableSubAgentNames(subAgents map[string]*Agent) []string {
	if len(subAgents) == 0 {
		return nil
	}
	names := make([]string, 0, len(subAgents))
	for name := range subAgents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeSubAgentLookupKey(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
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

func registerAskUserTool(toolMap map[string]ToolDefinition, order []string) []string {
	order = append(order, "ask_user")
	toolMap["ask_user"] = ToolDefinition{
		Name:        "ask_user",
		Description: "Pause execution and ask the user one concise question when critical information is missing. Always pass {\"question\":\"...\"}.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The concise question to ask the user before continuing",
					"minLength":   1,
				},
			},
			"required": []string{"question"},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
			type paramsStruct struct {
				Question string `json:"question"`
			}
			params := paramsStruct{}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid ask_user args: %w", err)
			}
			params.Question = strings.TrimSpace(params.Question)
			if params.Question == "" {
				return "", fmt.Errorf("ask_user args missing question")
			}
			return params.Question, nil
		},
	}
	return order
}

func registerHandoffToExecutorTool(toolMap map[string]ToolDefinition, order []string) []string {
	order = append(order, "handoff_to_executor")
	toolMap["handoff_to_executor"] = ToolDefinition{
		Name:        "handoff_to_executor",
		Description: "Finish planner stage and transfer a concise execution brief to the executor after the task board has been created or refreshed. Always pass {\"plan_summary\":\"...\",\"current_task\":\"...\",\"task_board_updated\":true} and optionally notes.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"plan_summary": map[string]any{
					"type":        "string",
					"description": "Concise ordered execution brief for the executor",
					"minLength":   1,
				},
				"current_task": map[string]any{
					"type":        "string",
					"description": "The first unfinished task the executor should start with",
					"minLength":   1,
				},
				"task_board_updated": map[string]any{
					"type":        "boolean",
					"description": "Set to true only after the task board reflects the plan",
				},
				"notes": map[string]any{
					"type":        "string",
					"description": "Optional blockers, assumptions, or sequencing notes for the executor",
				},
			},
			"required": []string{"plan_summary", "current_task", "task_board_updated"},
		},
		Handler: func(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
			type paramsStruct struct {
				PlanSummary      string `json:"plan_summary"`
				CurrentTask      string `json:"current_task"`
				TaskBoardUpdated bool   `json:"task_board_updated"`
				Notes            string `json:"notes"`
			}
			params := paramsStruct{}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", fmt.Errorf("invalid handoff_to_executor args: %w", err)
			}
			params.PlanSummary = strings.TrimSpace(params.PlanSummary)
			params.CurrentTask = strings.TrimSpace(params.CurrentTask)
			params.Notes = strings.TrimSpace(params.Notes)
			if params.PlanSummary == "" {
				return "", fmt.Errorf("handoff_to_executor args missing plan_summary")
			}
			if params.CurrentTask == "" {
				return "", fmt.Errorf("handoff_to_executor args missing current_task")
			}
			if !params.TaskBoardUpdated {
				return "", fmt.Errorf("handoff_to_executor requires task_board_updated=true")
			}

			lines := []string{
				"<planner_handoff>",
				"<plan_summary>",
				params.PlanSummary,
				"</plan_summary>",
				"<current_task>",
				params.CurrentTask,
				"</current_task>",
				"<task_board_updated>true</task_board_updated>",
			}
			if params.Notes != "" {
				lines = append(lines,
					"<notes>",
					params.Notes,
					"</notes>",
				)
			}
			lines = append(lines, "</planner_handoff>")
			return strings.Join(lines, "\n"), nil
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
	order = registerAskUserTool(toolMap, order)
	order = registerHandoffToExecutorTool(toolMap, order)
	order = registerCompactTool(toolMap, order)

	agent := &Agent{
		Name:            name,
		SystemPrompt:    systemPrompt,
		Description:     agentOpts.Desc,
		BaseUrl:         baseURL,
		ApiKey:          apiKey,
		Model:           model,
		LiveOutputID:    "agent:" + name,
		LiveOutputTitle: name,
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
	result, _, err := a.RunStructuredWithState(ctx, messages)
	return result, err
}

func (a *Agent) RunStructuredWithState(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, *StructuredRunState, error) {
	return a.RunStructuredWithPolicyAndState(ctx, messages, PlanningPolicyAuto)
}

func (a *Agent) RunStructuredWithPolicyAndState(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, policy PlanningPolicy) (string, *StructuredRunState, error) {
	policy = normalizePlanningPolicy(policy)
	plan := ""
	plannerRan := false

	if a.shouldRunPlanner(ctx, messages, policy) {
		log.Printf("[StructuredRun] agent=%s starting planner stage", agentLogName(a))
		planner := a.cloneWithTools(a.plannerSystemPrompt(), plannerToolAllowlist)
		planner.runStage = "planner"
		planner.LiveOutputID = fmt.Sprintf("%s:planner", a.Name)
		planner.LiveOutputTitle = "Planner"
		plannerMessages := applySystemPrompt(messages, planner.SystemPrompt)
		plannerMessages = append(plannerMessages, openai.UserMessage(a.plannerContextMessage()))

		nextPlan, resumeMessages, err := planner.runLoopWithState(ctx, plannerMessages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s planner stage paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				state := a.buildPlannerState(messages, plannerMessages, resumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				state.Status = RunPaused
				return pauseErr.Question, state, nil
			}
			log.Printf("[StructuredRun] agent=%s planner stage failed: %v", agentLogName(a), err)
			state := a.buildPlannerState(messages, plannerMessages, resumeMessages, nil)
			return "", state, &StructuredRunError{
				Stage:  "planner",
				Cause:  err,
				Resume: state,
			}
		}
		plan = nextPlan
		plannerRan = true
		log.Printf("[StructuredRun] agent=%s planner stage completed: plan_size=%d", agentLogName(a), len(strings.TrimSpace(plan)))
		a.reportStageOutput("planner", plan)
	} else {
		log.Printf("[StructuredRun] agent=%s skipping planner stage (policy=%s)", agentLogName(a), policy)
	}

	log.Printf("[StructuredRun] agent=%s starting executor stage", agentLogName(a))
	executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
	executor.runStage = "executor"
	executor.LiveOutputID = fmt.Sprintf("%s:executor", a.Name)
	executor.LiveOutputTitle = "Executor"
	executorMessages := applySystemPrompt(messages, executor.SystemPrompt)
	executorMessages = append(executorMessages, openai.UserMessage(a.executorContextMessage(plan, plannerRan)))

	result, resumeMessages, err := executor.runLoopWithState(ctx, executorMessages)
	if err != nil {
		var pauseErr *runPausedError
		if errors.As(err, &pauseErr) {
			log.Printf("[StructuredRun] agent=%s executor stage paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
			state := a.buildExecutorState(messages, plan, executorMessages, resumeMessages, &StructuredPauseInfo{
				Kind:     pauseErr.Kind,
				Question: pauseErr.Question,
			})
			state.Status = RunPaused
			return pauseErr.Question, state, nil
		}
		log.Printf("[StructuredRun] agent=%s executor stage failed: %v", agentLogName(a), err)
		state := a.buildExecutorState(messages, plan, executorMessages, resumeMessages, nil)
		return "", state, &StructuredRunError{
			Stage:  "executor",
			Cause:  err,
			Resume: state,
		}
	}
	log.Printf("[StructuredRun] agent=%s executor stage completed: result_size=%d", agentLogName(a), len(strings.TrimSpace(result)))
	return result, nil, nil
}

func (a *Agent) ContinueStructured(ctx context.Context, state *StructuredRunState, input string) (string, *StructuredRunState, error) {
	if state == nil {
		return "", nil, fmt.Errorf("structured run state unavailable")
	}
	switch strings.TrimSpace(state.Stage) {
	case "planner":
		if len(state.PlannerMessages) == 0 {
			return "", nil, fmt.Errorf("structured planner state unavailable")
		}
		log.Printf("[StructuredRun] agent=%s resuming planner stage: input_size=%d", agentLogName(a), len(strings.TrimSpace(input)))

		planner := a.cloneWithTools(a.plannerSystemPrompt(), plannerToolAllowlist)
		planner.runStage = "planner"
		planner.LiveOutputID = fmt.Sprintf("%s:planner", a.Name)
		planner.LiveOutputTitle = "Planner"

		messages := cloneChatMessages(state.PlannerMessages)
		if strings.TrimSpace(input) != "" {
			messages = append(messages, openai.UserMessage(input))
		}

		plan, resumeMessages, err := planner.runLoopWithState(ctx, messages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s resumed planner paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildPlannerState(state.BaseMessages, messages, resumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s resumed planner failed: %v", agentLogName(a), err)
			next := a.buildPlannerState(state.BaseMessages, messages, resumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "planner",
				Cause:  err,
				Resume: next,
			}
		}

		log.Printf("[StructuredRun] agent=%s resumed planner completed: plan_size=%d", agentLogName(a), len(strings.TrimSpace(plan)))
		a.reportStageOutput("planner", plan)

		baseMessages := cloneChatMessages(state.BaseMessages)
		if len(baseMessages) == 0 {
			return "", nil, fmt.Errorf("planner resume missing base messages for executor handoff")
		}

		executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
		executor.runStage = "executor"
		executor.LiveOutputID = fmt.Sprintf("%s:executor", a.Name)
		executor.LiveOutputTitle = "Executor"

		executorMessages := applySystemPrompt(baseMessages, executor.SystemPrompt)
		executorMessages = append(executorMessages, openai.UserMessage(a.executorContextMessage(plan, true)))

		result, executorResumeMessages, err := executor.runLoopWithState(ctx, executorMessages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s executor stage paused after planner resume: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildExecutorState(baseMessages, plan, executorMessages, executorResumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s executor stage failed after planner resume: %v", agentLogName(a), err)
			next := a.buildExecutorState(baseMessages, plan, executorMessages, executorResumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "executor",
				Cause:  err,
				Resume: next,
			}
		}
		log.Printf("[StructuredRun] agent=%s executor stage completed after planner resume: result_size=%d", agentLogName(a), len(strings.TrimSpace(result)))
		return result, nil, nil

	case "", "executor":
		if len(state.ExecutorMessages) == 0 {
			return "", nil, fmt.Errorf("structured executor state unavailable")
		}
		log.Printf("[StructuredRun] agent=%s resuming executor stage: input_size=%d", agentLogName(a), len(strings.TrimSpace(input)))

		executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
		executor.runStage = "executor"
		executor.LiveOutputID = fmt.Sprintf("%s:executor", a.Name)
		executor.LiveOutputTitle = "Executor"

		messages := cloneChatMessages(state.ExecutorMessages)
		if strings.TrimSpace(input) != "" {
			messages = append(messages, openai.UserMessage(input))
		}

		result, resumeMessages, err := executor.runLoopWithState(ctx, messages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s resumed executor paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildExecutorState(state.BaseMessages, state.Plan, messages, resumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s resumed executor failed: %v", agentLogName(a), err)
			next := a.buildExecutorState(state.BaseMessages, state.Plan, messages, resumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "executor",
				Cause:  err,
				Resume: next,
			}
		}
		log.Printf("[StructuredRun] agent=%s resumed executor completed: result_size=%d", agentLogName(a), len(strings.TrimSpace(result)))
		return result, nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported structured stage %q", state.Stage)
	}
}

func (a *Agent) runLoop(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	result, _, err := a.runLoopWithState(ctx, messages)
	return result, err
}

func (a *Agent) runLoopWithState(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, []openai.ChatCompletionMessageParamUnion, error) {
	if a != nil && a.runLoopOverride != nil {
		result, err := a.runLoopOverride(a, ctx, messages)
		if err != nil {
			return "", cloneChatMessages(messages), err
		}
		return result, nil, nil
	}

	maxTurns := maxTurnsLimit()
	roundsSinceTodo := 0
	for turn := 0; turn < maxTurns; turn++ {
		var err error
		var compactErr error
		messages, compactErr = a.maybeAutoCompact(ctx, messages)
		if compactErr != nil {
			return "", cloneChatMessages(messages), fmt.Errorf("auto compact failed (turn=%d): %w", turn, compactErr)
		}
		stableMessages := cloneChatMessages(messages)
		turnAcks := &turnEventAcks{}
		messages = a.stageBackgroundNotifications(messages, turnAcks)
		messages, err = a.stageTeamInboxMessages(messages, turnAcks)
		if err != nil {
			_ = turnAcks.Rollback()
			return "", stableMessages, fmt.Errorf("stage turn events failed (turn=%d): %w", turn, err)
		}

		resp, err := a.streamChatCompletion(ctx, messages, turn)
		if err != nil {
			_ = turnAcks.Rollback()
			return "", stableMessages, err
		}
		if err := turnAcks.Commit(); err != nil {
			return "", stableMessages, fmt.Errorf("ack turn events failed (turn=%d): %w", turn, err)
		}
		if len(resp.Choices) == 0 {
			return "", stableMessages, fmt.Errorf("empty choices from model")
		}

		usedTodo := false

		choice := resp.Choices[0]

		switch choice.FinishReason {
		case "stop":
			messages = append(messages, choice.Message.ToParam())
			return choice.Message.Content, nil, nil

		case "tool_calls":
			messages = append(messages, choice.Message.ToParam())
			manualCompacted := false
			for _, tc := range choice.Message.ToolCalls {
				toolName := tc.Function.Name
				toolArgs := json.RawMessage(tc.Function.Arguments)
				a.reportStageOutput(
					fmt.Sprintf("%s Tool %s", a.displayTitle(), toolName),
					fmt.Sprintf("开始执行:\n%s", compactToolDisplay(tc.Function.Arguments, toolName)),
				)
				output, callErr := a.executeTool(ctx, toolName, toolArgs)
				if callErr != nil {
					output = "tool error: " + callErr.Error()
				}
				a.reportStageOutput(
					fmt.Sprintf("%s Tool %s", a.displayTitle(), toolName),
					fmt.Sprintf("执行结果:\n%s", strings.TrimSpace(toolResultCompact(output, toolName))),
				)

				// 回填 tool 消息，关联 tool_call_id
				messages = append(messages, openai.ToolMessage(output, tc.ID))
				if toolName == "ask_user" && callErr == nil {
					return output, cloneChatMessages(messages), &runPausedError{
						Stage:    strings.TrimSpace(a.runStage),
						Kind:     "ask_user",
						Question: strings.TrimSpace(output),
					}
				}
				if toolName == "handoff_to_executor" && callErr == nil && strings.TrimSpace(a.runStage) == "planner" {
					return output, cloneChatMessages(messages), nil
				}
				if countsAsPlanningTool(toolName) {
					usedTodo = true
				}

				if toolName == "compact" && callErr == nil {
					focus, parseErr := parseCompactFocus(toolArgs)
					if parseErr != nil {
						return "", cloneChatMessages(messages), fmt.Errorf("manual compact args parse failed (turn=%d): %w", turn, parseErr)
					}
					messages, err = a.forceAutoCompact(ctx, messages, focus)
					if err != nil {
						return "", cloneChatMessages(messages), fmt.Errorf("manual compact failed (turn=%d): %w", turn, err)
					}
					manualCompacted = true
					roundsSinceTodo = 0
					break
				}
			}
			if manualCompacted {
				continue
			}

		case "network_error":
			return "", stableMessages, fmt.Errorf("model interrupted with finish reason: %s", choice.FinishReason)

		default:
			return "", stableMessages, fmt.Errorf("unsupported finish reason: %s", choice.FinishReason)
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}
		if roundsSinceTodo >= planningReminderTurns {
			messages = append(messages, openai.UserMessage("<reminder>Update your task status or todos.</reminder>"))
		}
	}

	return "", cloneChatMessages(messages), fmt.Errorf("max turns reached without final answer")
}

func countsAsPlanningTool(toolName string) bool {
	toolName = strings.TrimSpace(toolName)
	return toolName == "todo" || strings.HasPrefix(toolName, "task_")
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
	log.Printf(
		"[StageOutput] agent=%s stage=%s content=%s",
		agentLogName(a),
		strings.TrimSpace(stage),
		truncate(strings.TrimSpace(content), 4000),
	)
	if a == nil || a.stageOutputReporter == nil {
		return
	}
	a.stageOutputReporter(stage, content)
}

func cloneChatMessages(messages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]openai.ChatCompletionMessageParamUnion, len(messages))
	copy(cloned, messages)
	return cloned
}

func normalizePlanningPolicy(policy PlanningPolicy) PlanningPolicy {
	switch policy {
	case PlanningPolicyRequired, PlanningPolicySkip, PlanningPolicyAuto:
		return policy
	default:
		return PlanningPolicyAuto
	}
}

func ParsePlanningPolicy(raw string) (PlanningPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return PlanningPolicyAuto, nil
	case "on", "enable", "enabled", "required", "force-on", "force_on":
		return PlanningPolicyRequired, nil
	case "off", "disable", "disabled", "skip", "force-off", "force_off":
		return PlanningPolicySkip, nil
	default:
		return "", fmt.Errorf("unknown planning policy %q", raw)
	}
}

func PlanningPolicyLabel(policy PlanningPolicy) string {
	switch normalizePlanningPolicy(policy) {
	case PlanningPolicyRequired:
		return "FORCED ON"
	case PlanningPolicySkip:
		return "FORCED OFF"
	default:
		return "AUTO"
	}
}

func (a *Agent) buildPlannerState(baseMessages []openai.ChatCompletionMessageParamUnion, plannerMessages []openai.ChatCompletionMessageParamUnion, resumeMessages []openai.ChatCompletionMessageParamUnion, pause *StructuredPauseInfo) *StructuredRunState {
	state := &StructuredRunState{
		Stage:           "planner",
		BaseMessages:    cloneChatMessages(baseMessages),
		PlannerMessages: cloneChatMessages(plannerMessages),
		Pause:           pause,
	}
	if len(resumeMessages) > 0 {
		state.PlannerMessages = cloneChatMessages(resumeMessages)
	}
	return state
}

func (a *Agent) buildExecutorState(baseMessages []openai.ChatCompletionMessageParamUnion, plan string, executorMessages []openai.ChatCompletionMessageParamUnion, resumeMessages []openai.ChatCompletionMessageParamUnion, pause *StructuredPauseInfo) *StructuredRunState {
	state := &StructuredRunState{
		Stage:            "executor",
		Plan:             plan,
		BaseMessages:     cloneChatMessages(baseMessages),
		ExecutorMessages: cloneChatMessages(executorMessages),
		Pause:            pause,
	}
	if len(resumeMessages) > 0 {
		state.ExecutorMessages = cloneChatMessages(resumeMessages)
	}
	return state
}

func (a *Agent) shouldRunPlanner(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, policy PlanningPolicy) bool {
	switch normalizePlanningPolicy(policy) {
	case PlanningPolicyRequired:
		return true
	case PlanningPolicySkip:
		return false
	}

	if a.hasUnfinishedTasks() {
		return true
	}

	latest := strings.TrimSpace(lastUserMessageContent(messages))
	if latest == "" {
		return true
	}
	return !isSimpleDirectExecutionRequest(ctx, a.client, a.Model, latest)
}

func (a *Agent) hasUnfinishedTasks() bool {
	if a == nil || a.TaskManager == nil {
		return false
	}
	tasks, err := a.TaskManager.Snapshot()
	if err != nil {
		return true
	}
	for _, task := range tasks {
		if task != nil && task.Status != taskStatusCompleted {
			return true
		}
	}
	return false
}

func lastUserMessageContent(messages []openai.ChatCompletionMessageParamUnion) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role, content, err := messageRoleAndContent(messages[i])
		if err == nil && role == "user" {
			return content
		}
	}
	return ""
}

// 判断是否需要运行计划器的判断逻辑现在改为使用大模型
func isSimpleDirectExecutionRequest(ctx context.Context, client openai.Client, model, userInput string) bool {
	trimmed := strings.TrimSpace(userInput)
	if trimmed == "" {
		return false
	}

	// 调用大模型进行判断
	return useModelToDetermineSimpleRequest(ctx, client, model, trimmed)
}

// useModelToDetermineSimpleRequest 使用大模型判断是否为简单请求
func useModelToDetermineSimpleRequest(ctx context.Context, client openai.Client, model, userInput string) bool {
	systemPrompt := `你是一个简单请求判断器。你的任务是根据用户输入判断该请求是否足够简单，可以直接执行而无需详细的计划。

判断标准：
- 简单请求：单一任务、明确目标、不需要多步骤处理、只是询问不要求行动
- 复杂请求：需要多个步骤、涉及多个文件、包含复杂逻辑、或者明确提到需要计划/规划

请只回答 "yes" 或 "no"：
- "yes"：这是一个简单请求，可以直接执行
- "no"：这不是一个简单请求，需要计划模式`

	resp, err := withOpenAIRateLimitRetry(ctx, "simple_request_classifier", func() (*openai.ChatCompletion, error) {
		return client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: model,
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(systemPrompt),
				openai.UserMessage(fmt.Sprintf("用户输入：%s\n\n请回答 yes 或 no：", userInput)),
			},
			MaxCompletionTokens: openai.Int(10), // 设置非常短的 maxTokens
		})
	})
	if err != nil {
		// 如果调用失败，默认返回 false（走计划模式）
		log.Printf("[isSimpleDirectExecutionRequest] model call failed: %v, defaulting to false", err)
		return false
	}
	if len(resp.Choices) == 0 {
		return false
	}
	recordTokenUsage(nil, model, "simple_request_classifier", -1, resp.Choices[0].FinishReason, resp.Usage)

	answer := strings.TrimSpace(strings.ToLower(resp.Choices[0].Message.Content))
	log.Printf("[isSimpleDirectExecutionRequest] model response: %s for input: %s", answer, truncate(userInput, 50))

	// 只在明确是 "yes" 时返回 true
	return answer == "yes"
}

func isSimpleDirectExecutionRequestRule(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	if utf8.RuneCountInString(trimmed) > 120 {
		return false
	}
	if strings.Contains(trimmed, "\n") {
		return false
	}

	planningMarkers := []string{
		"plan",
		"planner",
		"roadmap",
		"step by step",
		"todo",
		"task list",
		"break down",
		"拆分",
		"规划",
		"计划",
		"步骤",
		"方案",
	}
	for _, marker := range planningMarkers {
		if strings.Contains(trimmed, marker) {
			return false
		}
	}

	complexityMarkers := []string{
		" and then ",
		" then ",
		" meanwhile ",
		"同时",
		"然后",
		"并且",
		"另外",
		"顺便",
		"分别",
	}
	for _, marker := range complexityMarkers {
		if strings.Contains(trimmed, marker) {
			return false
		}
	}

	return true
}

func (a *Agent) reportLiveOutput(id, title, content string, done bool) {
	if done {
		trimmed := strings.TrimSpace(content)
		if trimmed != "" {
			log.Printf(
				"[LiveOutput] agent=%s id=%s title=%s state=final content=%s",
				agentLogName(a),
				strings.TrimSpace(id),
				strings.TrimSpace(title),
				truncate(trimmed, 4000),
			)
		}
	}
	if a == nil || a.liveOutputReporter == nil {
		return
	}
	a.liveOutputReporter(id, title, content, done)
}

func recordTokenUsage(agent *Agent, model, kind string, turn int, finishReason string, usage openai.CompletionUsage) {
	stage := ""
	agentName := agentLogName(agent)
	if agent != nil {
		stage = strings.TrimSpace(agent.runStage)
		if strings.TrimSpace(model) == "" {
			model = agent.Model
		}
	}
	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}
	if strings.TrimSpace(kind) == "" {
		kind = "chat_completion"
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "unknown"
	}

	line := fmt.Sprintf(
		"[TokenUsage] agent=%s stage=%s kind=%s turn=%s model=%s finish_reason=%s prompt_tokens=%d completion_tokens=%d total_tokens=%d reasoning_tokens=%d cached_tokens=%d input_audio_tokens=%d output_audio_tokens=%d accepted_prediction_tokens=%d rejected_prediction_tokens=%d",
		agentName,
		stage,
		kind,
		formatTokenUsageTurn(turn),
		strings.TrimSpace(model),
		strings.TrimSpace(finishReason),
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.TotalTokens,
		usage.CompletionTokensDetails.ReasoningTokens,
		usage.PromptTokensDetails.CachedTokens,
		usage.PromptTokensDetails.AudioTokens,
		usage.CompletionTokensDetails.AudioTokens,
		usage.CompletionTokensDetails.AcceptedPredictionTokens,
		usage.CompletionTokensDetails.RejectedPredictionTokens,
	)
	log.Print(line)
	if err := appendTokenUsageLine(line); err != nil {
		log.Printf("[TokenUsage] failed to append token log %q: %v", TOKEN_LOG_PATH, err)
	}
}

func formatTokenUsageTurn(turn int) string {
	if turn < 0 {
		return "-"
	}
	return fmt.Sprintf("%d", turn+1)
}

func appendTokenUsageLine(line string) error {
	path := strings.TrimSpace(TOKEN_LOG_PATH)
	if path == "" {
		return fmt.Errorf("token log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tokenLogMu.Lock()
	defer tokenLogMu.Unlock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339Nano), line)
	return err
}

func (a *Agent) displayTitle() string {
	if a == nil {
		return "Agent"
	}
	if strings.TrimSpace(a.LiveOutputTitle) != "" {
		return a.LiveOutputTitle
	}
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return "Agent"
}

func (a *Agent) liveOutputIdentity() (string, string) {
	if a == nil {
		return "", ""
	}
	id := strings.TrimSpace(a.LiveOutputID)
	if id == "" {
		id = "agent:" + strings.TrimSpace(a.Name)
	}
	title := strings.TrimSpace(a.LiveOutputTitle)
	if title == "" {
		title = a.displayTitle()
	}
	return id, title
}

func (a *Agent) streamChatCompletion(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, turn int) (*openai.ChatCompletion, error) {
	return withOpenAIRateLimitRetry(ctx, "stream_chat_completion", func() (*openai.ChatCompletion, error) {
		stream := a.client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
			Model:    a.Model,
			Messages: messages,
			Tools:    a.openAITools(),
			StreamOptions: openai.ChatCompletionStreamOptionsParam{
				IncludeUsage: openai.Bool(true),
			},
		})
		acc := openai.ChatCompletionAccumulator{}
		liveID, liveTitle := a.liveOutputIdentity()
		lastPreview := ""
		if liveID != "" {
			lastPreview = "思考中..."
			a.reportLiveOutput(liveID, liveTitle, lastPreview, false)
		}

		for stream.Next() {
			chunk := stream.Current()
			if !acc.AddChunk(chunk) {
				a.reportLiveOutput(liveID, liveTitle, "", true)
				return nil, fmt.Errorf("chat completion stream accumulate failed (turn=%d)", turn)
			}
			preview := renderStreamingPreview(acc.ChatCompletion)
			if preview != "" && preview != lastPreview {
				lastPreview = preview
				a.reportLiveOutput(liveID, liveTitle, preview, false)
			}
		}

		if err := stream.Err(); err != nil {
			a.reportLiveOutput(liveID, liveTitle, "", true)
			return nil, fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
		}
		a.reportLiveOutput(liveID, liveTitle, renderStreamingPreview(acc.ChatCompletion), true)

		if len(acc.Choices) == 0 {
			return nil, fmt.Errorf("empty choices from model")
		}
		resp := acc.ChatCompletion
		recordTokenUsage(a, a.Model, "stream_chat_completion", turn, resp.Choices[0].FinishReason, resp.Usage)
		return &resp, nil
	})
}

func renderStreamingPreview(resp openai.ChatCompletion) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	choice := resp.Choices[0]
	parts := make([]string, 0, 2)
	if strings.TrimSpace(choice.Message.Content) != "" {
		parts = append(parts, choice.Message.Content)
	} else if strings.TrimSpace(choice.Message.Refusal) != "" {
		parts = append(parts, choice.Message.Refusal)
	}
	if len(choice.Message.ToolCalls) > 0 {
		parts = append(parts, "调用工具:\n"+formatToolCallPreview(choice.Message.ToolCalls))
	}
	if len(parts) == 0 {
		return "思考中..."
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func formatToolCallPreview(toolCalls []openai.ChatCompletionMessageToolCallUnion) string {
	lines := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			name = "(pending)"
		}
		args := compactToolDisplay(strings.TrimSpace(tc.Function.Arguments), name)
		if args == "" {
			lines = append(lines, fmt.Sprintf("- %s", name))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s %s", name, args))
	}
	return strings.Join(lines, "\n")
}

func compactToolDisplay(payload, toolName string) string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return ""
	}

	switch toolName {
	case "write_file":
		return compactWriteFileDisplay(trimmed)
	default:
		return trimmed
	}
}

func compactWriteFileDisplay(payload string) string {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(payload), &params); err != nil {
		return payload
	}

	type writeFileDisplay struct {
		Path           string `json:"path,omitempty"`
		ContentBytes   int    `json:"content_bytes"`
		ContentPreview string `json:"content_preview,omitempty"`
	}

	display := writeFileDisplay{
		Path:         strings.TrimSpace(params.Path),
		ContentBytes: len(params.Content),
	}

	const previewLimit = 200
	preview := params.Content
	if utf8.RuneCountInString(preview) > previewLimit {
		preview = truncateByRunes(preview, previewLimit) + "... (truncated)"
	}
	if strings.TrimSpace(preview) != "" {
		display.ContentPreview = preview
	}

	data, err := json.MarshalIndent(display, "", "  ")
	if err != nil {
		return payload
	}
	return string(data)
}

func (a *Agent) plannerSystemPrompt() string {
	rules := strings.Join([]string{
		"You are in Planner stage.",
		"You are an experienced software architect. Your only job is to design the architecture and break down tasks.",
		"Do not edit the file or attempt any implementation work.",
		"Prefer task_list/task_get before creating or updating tasks so you reuse the current board when possible.",
		"Do not stop at prose only: create or update task board entries for the concrete execution steps you want the executor to follow.",
		"If critical information is missing, call ask_user with one concise blocking question instead of guessing.",
		"When you have enough information and the task board is up to date, call handoff_to_executor to transfer a concise execution brief and the current unfinished task.",
		"Do not return a normal final answer to end planning; use handoff_to_executor once planning is complete.",
	}, " ")
	return rules
}

func (a *Agent) executorSystemPrompt() string {
	rules := strings.Join([]string{
		"You are in Executor stage.",
		"If the planner stage already produced a task list, use it as the source of truth unless execution proves it is stale or blocked.",
		"If execution is blocked on missing user input, call ask_user instead of guessing.",
		"Work through the current unfinished tasks in order when a plan exists.",
		"Keep todo/task status aligned with real progress, and only re-plan when blocked by new information or the work expands beyond a simple request.",
		"After you write or edit code, run check_types on a relevant changed file for each edited language or project before you finish.",
		"If check_types reports errors, keep fixing the code and rerun it until the relevant checks pass or you have a concrete toolchain blocker to report.",
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
		"Use the available todo/task tools to capture the plan, and ensure each meaningful execution step exists on the task board.",
		"If you are blocked on missing user information, pause with ask_user.",
		"When the plan and task board are ready, call handoff_to_executor with the concise handoff summary and current unfinished task.",
		"</planning_rule>",
		"<current_task_board>",
		a.currentTaskBoardSummary(),
		"</current_task_board>",
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) executorContextMessage(plan string, plannerRan bool) string {
	lines := []string{
		"<planning_status>",
		map[bool]string{true: "planner_completed", false: "planner_skipped"}[plannerRan],
		"</planning_status>",
	}
	if plannerRan {
		lines = append(lines,
			"<planner_output>",
			strings.TrimSpace(plan),
			"</planner_output>",
		)
	}
	lines = append(lines,
		"<current_task_board>",
		a.currentTaskBoardSummary(),
		"</current_task_board>",
	)
	if plannerRan {
		lines = append(lines,
			"<unfinished_tasks>",
			a.unfinishedTaskSummary(),
			"</unfinished_tasks>",
			"<execution_rule>",
			"Start from the first unfinished task above and complete tasks sequentially.",
			"If a task is blocked, explain the blocker and update the task state before moving on.",
			"</execution_rule>",
		)
	} else {
		lines = append(lines,
			"<execution_rule>",
			"This request skipped formal planning because it appears simple.",
			"Proceed directly. If the work becomes multi-step, blocked, or needs missing input, pause with ask_user or trigger a fresh structured run later.",
			"</execution_rule>",
		)
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
	if utf8.RuneCountInString(conversationText) <= autoCompactTriggerThreshold() {
		return messages, nil
	}
	return a.autoCompact(ctx, messages, "")
}

func (a *Agent) forceAutoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
	return a.autoCompact(ctx, messages, focus)
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

func (a *Agent) autoCompact(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, focus string) ([]openai.ChatCompletionMessageParamUnion, error) {
	if err := os.MkdirAll(TRANSCRIPT_DIR, 0o755); err != nil {
		return nil, fmt.Errorf("create transcript dir failed: %w", err)
	}

	transcriptPath := filepath.Join(TRANSCRIPT_DIR, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
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

	systemMessages, summarizeMessages, recentMessages, err := splitMessagesForAutoCompact(messages)
	if err != nil {
		return nil, err
	}
	if len(summarizeMessages) == 0 {
		return messages, nil
	}

	conversationText, err := marshalConversation(summarizeMessages)
	if err != nil {
		return nil, err
	}
	prompt := "Summarize the older portion of this conversation for continuity while newer messages remain verbatim. Include: " +
		"1) What was accomplished, 2) Current state and pending work, 3) Key decisions, constraints, and important metadata. " +
		"Be concise but preserve critical details.\n\n" + conversationText
	if strings.TrimSpace(focus) != "" {
		prompt += "\n\nFocus to preserve: " + focus
	}

	summary, err := a.summarizeForAutoCompact(ctx, prompt)
	if err != nil {
		return nil, err
	}

	compacted := cloneChatMessages(systemMessages)
	compacted = append(compacted,
		openai.UserMessage(fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcriptPath, summary)),
		openai.AssistantMessage("Understood. I have the context from the summary. Continuing."),
	)
	compacted = append(compacted, cloneChatMessages(recentMessages)...)
	return compacted, nil
}

func (a *Agent) summarizeForAutoCompact(ctx context.Context, prompt string) (string, error) {
	if a.autoCompactSummarizer != nil {
		return a.autoCompactSummarizer(ctx, prompt)
	}

	resp, err := withOpenAIRateLimitRetry(ctx, "auto_compact_summary", func() (*openai.ChatCompletion, error) {
		return a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:               a.Model,
			Messages:            []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
			MaxCompletionTokens: openai.Int(int64(autoCompactSummaryMaxTokens())),
		})
	})
	if err != nil {
		return "", fmt.Errorf("summary generation failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty summary choices from model")
	}
	recordTokenUsage(a, a.Model, "auto_compact_summary", -1, resp.Choices[0].FinishReason, resp.Usage)

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

func splitMessagesForAutoCompact(messages []openai.ChatCompletionMessageParamUnion) ([]openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionMessageParamUnion, []openai.ChatCompletionMessageParamUnion, error) {
	if len(messages) == 0 {
		return nil, nil, nil, nil
	}

	systemPrefixCount := 0
	for systemPrefixCount < len(messages) {
		role, _, err := messageRoleAndContent(messages[systemPrefixCount])
		if err != nil {
			return nil, nil, nil, err
		}
		if role != "system" {
			break
		}
		systemPrefixCount++
	}

	systemMessages := cloneChatMessages(messages[:systemPrefixCount])
	nonSystemMessages := cloneChatMessages(messages[systemPrefixCount:])
	if len(nonSystemMessages) == 0 {
		return systemMessages, nil, nil, nil
	}

	recentCount := (len(nonSystemMessages)*30 + 99) / 100
	if recentCount <= 0 {
		recentCount = 1
	}
	if recentCount >= len(nonSystemMessages) {
		if len(nonSystemMessages) == 1 {
			return systemMessages, cloneChatMessages(nonSystemMessages), nil, nil
		}
		recentCount = len(nonSystemMessages) - 1
	}

	splitIndex := len(nonSystemMessages) - recentCount
	return systemMessages, cloneChatMessages(nonSystemMessages[:splitIndex]), cloneChatMessages(nonSystemMessages[splitIndex:]), nil
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
	const limit = 1200
	trimmed := strings.TrimSpace(output)
	if toolName == "read_file" {
		return trimmed
	}
	if utf8.RuneCountInString(trimmed) > limit {
		return truncate(trimmed, limit) + "\n... (truncated)"
	}
	return trimmed
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
