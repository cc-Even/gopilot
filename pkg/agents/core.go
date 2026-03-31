package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
)

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

	Provider providerKind

	provider modelProvider
	tools    map[string]ToolDefinition
	order    []string

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

type PlanningPolicy string

const (
	openAIRateLimitRetryEnv = "OPENAI_RATE_LIMIT_RETRY_SECONDS"
	planningReminderTurns   = 6
	agentMaxTurnsEnv        = "AGENT_MAX_TURNS"

	agentMaxTurnsDefault = 999

	openAITransientRetryMaxAttempts = 3
)

var rateLimitSleep = sleepWithContext

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

func openAITransientRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(1<<(attempt-1)) * time.Second
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

func NewAgent(name, systemPrompt, model string, createOpts ...AgentOption) *Agent {
	// 初始化选项
	agentOpts := &AgentOptions{}
	for _, opt := range createOpts {
		opt(agentOpts)
	}

	// 处理 Model 默认值
	if model == "" {
		model = os.Getenv("MODEL")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 处理 Provider / BaseUrl / 凭据默认值
	baseURL := strings.TrimSpace(agentOpts.BaseUrl)
	detectedBaseURL := baseURL
	if detectedBaseURL == "" {
		detectedBaseURL = firstNonEmpty(
			os.Getenv("OPENAI_BASE_URL"),
			getenvFirst(geminiBaseURLEnv, vertexAIBaseURLEnv),
		)
	}
	providerKind := resolveProviderKind(model, detectedBaseURL)
	geminiBackend := geminiBackendKind("")
	apiKey := strings.TrimSpace(agentOpts.ApiKey)
	accessToken := ""

	switch providerKind {
	case providerGemini:
		geminiBackend = resolveGeminiBackend(firstNonEmpty(baseURL, getenvFirst(vertexAIBaseURLEnv, geminiBaseURLEnv)))
		if baseURL == "" {
			switch geminiBackend {
			case geminiBackendVertex:
				baseURL = getenvFirst(vertexAIBaseURLEnv, geminiBaseURLEnv)
			default:
				baseURL = getenvFirst(geminiBaseURLEnv)
			}
		}
		switch geminiBackend {
		case geminiBackendVertex:
			accessToken = firstNonEmpty(apiKey, getenvFirst(vertexAIAccessTokenEnv, geminiAccessTokenEnv))
			apiKey = ""
		default:
			if apiKey == "" {
				apiKey = getenvFirst(geminiAPIKeyEnv, googleAPIKeyEnv)
			}
		}
	default:
		if baseURL == "" {
			baseURL = os.Getenv("OPENAI_BASE_URL")
		}
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
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
		Provider:        providerKind,
		provider: newModelProvider(providerKind, modelProviderConfig{
			BaseURL:       baseURL,
			APIKey:        apiKey,
			AccessToken:   accessToken,
			ProjectID:     getenvFirst(vertexAIProjectIDEnv, geminiProjectIDEnv, googleCloudProjectEnv),
			Location:      firstNonEmpty(getenvFirst(vertexAILocationEnv, geminiLocationEnv, googleCloudLocationEnv, googleCloudRegionEnv), defaultVertexAILocation),
			GeminiBackend: geminiBackend,
		}),
		tools: toolMap,
		order: order,
	}

	agent.TeamManager = NewTeammateManager(TEAM_DIR, agent)
	agent.order = registerTeamTools(agent.tools, agent.order)

	return agent
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
func isSimpleDirectExecutionRequest(ctx context.Context, provider modelProvider, model, userInput string) bool {
	trimmed := strings.TrimSpace(userInput)
	if trimmed == "" {
		return false
	}

	// 调用大模型进行判断
	return useModelToDetermineSimpleRequest(ctx, provider, model, trimmed)
}

// useModelToDetermineSimpleRequest 使用大模型判断是否为简单请求
func useModelToDetermineSimpleRequest(ctx context.Context, provider modelProvider, model, userInput string) bool {
	if provider == nil {
		return false
	}
	systemPrompt := `你是一个简单请求判断器。你的任务是根据用户输入判断该请求是否足够简单，可以直接执行而无需详细的计划。

判断标准：
- 简单请求：单一任务、明确目标、不需要多步骤处理、只是询问不要求行动
- 复杂请求：需要多个步骤、涉及多个文件、包含复杂逻辑、或者明确提到需要计划/规划

请只回答 "yes" 或 "no"：
- "yes"：这是一个简单请求，可以直接执行
- "no"：这不是一个简单请求，需要计划模式`

	resp, err := withOpenAIRateLimitRetry(ctx, "simple_request_classifier", func() (*modelResponse, error) {
		return provider.Generate(ctx, modelRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(systemPrompt),
				openai.UserMessage(fmt.Sprintf("用户输入：%s\n\n请回答 yes 或 no：", userInput)),
			},
			MaxCompletionTokens: 10,
		}, nil)
	})
	if err != nil {
		// 如果调用失败，默认返回 false（走计划模式）
		log.Printf("[isSimpleDirectExecutionRequest] model call failed: %v, defaulting to false", err)
		return false
	}
	recordTokenUsage(nil, model, "simple_request_classifier", -1, resp.FinishReason, resp.Usage)

	answer := strings.TrimSpace(strings.ToLower(resp.Content))
	log.Printf("[isSimpleDirectExecutionRequest] model response: %s for input: %s", answer, truncate(userInput, 50))

	// 只在明确是 "yes" 时返回 true
	return answer == "yes"
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
	if a.provider == nil {
		return "", fmt.Errorf("model provider unavailable")
	}

	resp, err := withOpenAIRateLimitRetry(ctx, "auto_compact_summary", func() (*modelResponse, error) {
		return a.provider.Generate(ctx, modelRequest{
			Model:               a.Model,
			Messages:            []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
			MaxCompletionTokens: autoCompactSummaryMaxTokens(),
		}, nil)
	})
	if err != nil {
		return "", fmt.Errorf("summary generation failed: %w", err)
	}
	recordTokenUsage(a, a.Model, "auto_compact_summary", -1, resp.FinishReason, resp.Usage)

	return resp.Content, nil
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
