package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func TestRunStructuredUsesPlannerThenExecutor(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("existing task", "already on board"); err != nil {
		t.Fatalf("seed task failed: %v", err)
	}

	toolNames := []string{
		"todo",
		"task_create",
		"task_update",
		"task_list",
		"task_get",
		"read_file",
		"write_file",
		"edit_file",
		"bash",
	}
	toolMap := make(map[string]ToolDefinition, len(toolNames))
	for _, name := range toolNames {
		toolMap[name] = ToolDefinition{
			Name: name,
			Handler: func(context.Context, json.RawMessage, *Agent) (string, error) {
				return "", nil
			},
		}
	}

	type stageCall struct {
		systemPrompt string
		tools        []string
		lastUser     string
	}

	var calls []stageCall
	var reportedStage string
	var reportedContent string
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		tools:        toolMap,
		order:        toolNames,
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			lastUser := ""
			for i := len(messages) - 1; i >= 0; i-- {
				role, content, err := messageRoleAndContent(messages[i])
				if err == nil && role == "user" {
					lastUser = content
					break
				}
			}

			calls = append(calls, stageCall{
				systemPrompt: current.SystemPrompt,
				tools:        append([]string(nil), current.order...),
				lastUser:     lastUser,
			})

			if len(calls) == 1 {
				return "1. Inspect repo\n2. Edit code\nCurrent unfinished task: Inspect repo", nil
			}
			return "executor finished", nil
		},
	}
	agent.SetStageOutputReporter(func(stage, content string) {
		reportedStage = stage
		reportedContent = content
	})

	result, err := agent.RunStructured(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("old system prompt"),
		openai.UserMessage("implement the requested change"),
	})
	if err != nil {
		t.Fatalf("RunStructured failed: %v", err)
	}
	if result != "executor finished" {
		t.Fatalf("unexpected executor result: %q", result)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 stage calls, got %d", len(calls))
	}
	if reportedStage != "planner" {
		t.Fatalf("unexpected reported stage: %q", reportedStage)
	}
	if !strings.Contains(reportedContent, "Current unfinished task: Inspect repo") {
		t.Fatalf("unexpected reported planner content: %q", reportedContent)
	}

	planner := calls[0]
	if !strings.Contains(planner.systemPrompt, "Planner stage") {
		t.Fatalf("planner system prompt missing planner instructions: %q", planner.systemPrompt)
	}
	expectedPlannerTools := []string{"task_create", "task_update", "task_list", "task_get"}
	if strings.Join(planner.tools, ",") != strings.Join(expectedPlannerTools, ",") {
		t.Fatalf("unexpected planner tools: got %v want %v", planner.tools, expectedPlannerTools)
	}
	if !strings.Contains(planner.lastUser, "<planning_rule>") {
		t.Fatalf("planner context missing planning rule: %q", planner.lastUser)
	}
	if !strings.Contains(planner.lastUser, "existing task") {
		t.Fatalf("planner context missing current task board: %q", planner.lastUser)
	}

	executor := calls[1]
	if !strings.Contains(executor.systemPrompt, "Executor stage") {
		t.Fatalf("executor system prompt missing executor instructions: %q", executor.systemPrompt)
	}
	for _, required := range []string{"write_file", "edit_file", "bash"} {
		if !containsString(executor.tools, required) {
			t.Fatalf("executor tools missing %s: %v", required, executor.tools)
		}
	}
	if !strings.Contains(executor.lastUser, "<planner_output>") {
		t.Fatalf("executor context missing planner output: %q", executor.lastUser)
	}
	if !strings.Contains(executor.lastUser, "Current unfinished task: Inspect repo") {
		t.Fatalf("executor context missing planner summary: %q", executor.lastUser)
	}
	if !strings.Contains(executor.lastUser, "<unfinished_tasks>") {
		t.Fatalf("executor context missing unfinished tasks: %q", executor.lastUser)
	}
}

func TestRunStructuredSkipPolicySkipsPlannerForSimpleRequest(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}

	toolNames := []string{"bash", "ask_user"}
	type stageCall struct {
		systemPrompt string
		lastUser     string
	}
	var calls []stageCall
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		order:        toolNames,
		tools: map[string]ToolDefinition{
			"bash": {
				Name: "bash",
				Handler: func(context.Context, json.RawMessage, *Agent) (string, error) {
					return "", nil
				},
			},
			"ask_user": {
				Name: "ask_user",
				Handler: func(context.Context, json.RawMessage, *Agent) (string, error) {
					return "", nil
				},
			},
		},
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			lastUser := ""
			for i := len(messages) - 1; i >= 0; i-- {
				role, content, err := messageRoleAndContent(messages[i])
				if err == nil && role == "user" {
					lastUser = content
					break
				}
			}
			calls = append(calls, stageCall{
				systemPrompt: current.SystemPrompt,
				lastUser:     lastUser,
			})
			return "executor finished", nil
		},
	}

	result, _, err := agent.RunStructuredWithPolicyAndState(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("read the README"),
	}, PlanningPolicySkip)
	if err != nil {
		t.Fatalf("RunStructuredWithPolicyAndState failed: %v", err)
	}
	if result != "executor finished" {
		t.Fatalf("unexpected executor result: %q", result)
	}
	if len(calls) != 1 {
		t.Fatalf("expected only executor call, got %d", len(calls))
	}
	if strings.Contains(calls[0].systemPrompt, "Planner stage") {
		t.Fatalf("skip policy unexpectedly ran planner: %q", calls[0].systemPrompt)
	}
	if !strings.Contains(calls[0].lastUser, "planner_skipped") {
		t.Fatalf("executor context missing skipped-planner marker: %q", calls[0].lastUser)
	}
}

func TestRunStructuredExecutorFailureReturnsResumeState(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}

	calls := 0
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			calls++
			if strings.Contains(current.SystemPrompt, "Planner stage") {
				return "1. Edit code\nCurrent unfinished task: Edit code", nil
			}
			return "", errors.New("model interrupted with finish reason: network_error")
		},
	}

	_, state, err := agent.RunStructuredWithPolicyAndState(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("old system prompt"),
		openai.UserMessage("fix flaky executor"),
	}, PlanningPolicyRequired)
	if err == nil {
		t.Fatal("expected structured run to fail")
	}
	if calls != 2 {
		t.Fatalf("expected planner and executor calls, got %d", calls)
	}

	var runErr *StructuredRunError
	if !errors.As(err, &runErr) {
		t.Fatalf("expected StructuredRunError, got %T", err)
	}
	if runErr.Stage != "executor" {
		t.Fatalf("unexpected failure stage: %q", runErr.Stage)
	}
	if runErr.Resume == nil || state == nil {
		t.Fatal("expected resume state on executor failure")
	}
	if runErr.Resume != state {
		t.Fatal("expected returned state to match error resume state")
	}
	if state.Plan != "1. Edit code\nCurrent unfinished task: Edit code" {
		t.Fatalf("unexpected plan: %q", state.Plan)
	}
	if len(state.ExecutorMessages) < 2 {
		t.Fatalf("expected executor context to be preserved, got %d messages", len(state.ExecutorMessages))
	}
	lastRole, lastContent, msgErr := messageRoleAndContent(state.ExecutorMessages[len(state.ExecutorMessages)-1])
	if msgErr != nil {
		t.Fatalf("read executor resume message failed: %v", msgErr)
	}
	if lastRole != "user" || !strings.Contains(lastContent, "<planner_output>") {
		t.Fatalf("unexpected executor resume context: role=%s content=%q", lastRole, lastContent)
	}
}

func TestRunStructuredAskUserReturnsPausedState(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}

	calls := 0
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			calls++
			if strings.Contains(current.SystemPrompt, "Planner stage") {
				return "1. Inspect file\nCurrent unfinished task: Inspect file", nil
			}
			return "", &runPausedError{
				Stage:    "executor",
				Kind:     "ask_user",
				Question: "Which file should I inspect?",
			}
		},
	}

	response, state, err := agent.RunStructuredWithPolicyAndState(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("inspect the config"),
	}, PlanningPolicySkip)
	if err != nil {
		t.Fatalf("RunStructuredWithPolicyAndState returned error: %v", err)
	}
	if response != "Which file should I inspect?" {
		t.Fatalf("unexpected pause question: %q", response)
	}
	if calls != 1 {
		t.Fatalf("expected skip policy to bypass planner, got %d calls", calls)
	}
	if state == nil {
		t.Fatal("expected paused state")
	}
	if state.Status != RunPaused {
		t.Fatalf("unexpected state status: %q", state.Status)
	}
	if state.Stage != "executor" {
		t.Fatalf("unexpected paused stage: %q", state.Stage)
	}
	if state.Pause == nil || state.Pause.Question != "Which file should I inspect?" {
		t.Fatalf("unexpected pause info: %+v", state.Pause)
	}
	if len(state.ExecutorMessages) == 0 {
		t.Fatal("expected executor messages to be preserved")
	}
}

func TestContinueStructuredResumesExecutorWithoutPlanner(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}

	type stageCall struct {
		systemPrompt string
		lastUser     string
	}
	var calls []stageCall
	executorAttempts := 0
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			lastUser := ""
			for i := len(messages) - 1; i >= 0; i-- {
				role, content, err := messageRoleAndContent(messages[i])
				if err == nil && role == "user" {
					lastUser = content
					break
				}
			}
			calls = append(calls, stageCall{
				systemPrompt: current.SystemPrompt,
				lastUser:     lastUser,
			})

			if strings.Contains(current.SystemPrompt, "Planner stage") {
				return "1. Retry executor\nCurrent unfinished task: Retry executor", nil
			}

			executorAttempts++
			if executorAttempts == 1 {
				return "", errors.New("model interrupted with finish reason: network_error")
			}
			return "executor resumed", nil
		},
	}

	_, state, err := agent.RunStructuredWithPolicyAndState(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("recover from executor interruption"),
	}, PlanningPolicyRequired)
	if err == nil {
		t.Fatal("expected first executor run to fail")
	}
	if state == nil {
		t.Fatal("expected resume state")
	}

	result, nextState, err := agent.ContinueStructured(context.Background(), state, "continue")
	if err != nil {
		t.Fatalf("ContinueStructured failed: %v", err)
	}
	if result != "executor resumed" {
		t.Fatalf("unexpected resume result: %q", result)
	}
	if nextState != nil {
		t.Fatalf("expected cleared resume state after success, got %+v", nextState)
	}
	if len(calls) != 3 {
		t.Fatalf("expected planner + executor + resumed executor, got %d calls", len(calls))
	}
	if strings.Contains(calls[2].systemPrompt, "Planner stage") {
		t.Fatalf("resume unexpectedly reran planner: %q", calls[2].systemPrompt)
	}
	if calls[2].lastUser != "continue" {
		t.Fatalf("expected resume input to reach executor, got %q", calls[2].lastUser)
	}
	if strings.Contains(calls[1].lastUser, "continue") {
		t.Fatalf("initial executor attempt should not see resume input: %q", calls[1].lastUser)
	}
}

func TestContinueStructuredResumesPausedExecutorWithoutPlanner(t *testing.T) {
	taskManager, err := NewTaskManager(t.TempDir())
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}

	type stageCall struct {
		systemPrompt string
		lastUser     string
	}
	var calls []stageCall
	executorAttempts := 0
	agent := &Agent{
		SystemPrompt: "base prompt",
		TaskManager:  taskManager,
		runLoopOverride: func(current *Agent, _ context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
			lastUser := ""
			for i := len(messages) - 1; i >= 0; i-- {
				role, content, err := messageRoleAndContent(messages[i])
				if err == nil && role == "user" {
					lastUser = content
					break
				}
			}
			calls = append(calls, stageCall{
				systemPrompt: current.SystemPrompt,
				lastUser:     lastUser,
			})
			executorAttempts++
			if executorAttempts == 1 {
				return "", &runPausedError{
					Stage:    "executor",
					Kind:     "ask_user",
					Question: "Need the exact file path.",
				}
			}
			return "executor resumed", nil
		},
	}

	response, state, err := agent.RunStructuredWithPolicyAndState(context.Background(), []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("inspect the config"),
	}, PlanningPolicySkip)
	if err != nil {
		t.Fatalf("RunStructuredWithPolicyAndState failed: %v", err)
	}
	if response != "Need the exact file path." {
		t.Fatalf("unexpected pause response: %q", response)
	}
	if state == nil || state.Status != RunPaused {
		t.Fatalf("expected paused state, got %+v", state)
	}

	result, nextState, err := agent.ContinueStructured(context.Background(), state, "pkg/agents/core.go")
	if err != nil {
		t.Fatalf("ContinueStructured failed: %v", err)
	}
	if result != "executor resumed" {
		t.Fatalf("unexpected resume result: %q", result)
	}
	if nextState != nil {
		t.Fatalf("expected cleared resume state after success, got %+v", nextState)
	}
	if len(calls) != 2 {
		t.Fatalf("expected paused executor + resumed executor, got %d calls", len(calls))
	}
	if strings.Contains(calls[1].systemPrompt, "Planner stage") {
		t.Fatalf("resume unexpectedly reran planner: %q", calls[1].systemPrompt)
	}
	if calls[1].lastUser != "pkg/agents/core.go" {
		t.Fatalf("expected resume input to reach executor, got %q", calls[1].lastUser)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStageAndLiveOutputAreLogged(t *testing.T) {
	var buf bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer log.SetOutput(originalWriter)
	defer log.SetFlags(originalFlags)

	agent := &Agent{Name: "supervisor"}
	agent.reportStageOutput("planner", "plan ready")
	agent.reportLiveOutput("supervisor:executor", "Executor", "editing files", false)
	agent.reportLiveOutput("supervisor:executor", "Executor", "executor finished", true)

	logged := buf.String()
	if !strings.Contains(logged, "[StageOutput] agent=supervisor stage=planner content=plan ready") {
		t.Fatalf("missing stage log entry: %q", logged)
	}
	if strings.Contains(logged, "content=editing files") {
		t.Fatalf("unexpected incremental live log entry: %q", logged)
	}
	if !strings.Contains(logged, "[LiveOutput] agent=supervisor id=supervisor:executor title=Executor state=final content=executor finished") {
		t.Fatalf("missing final live log entry: %q", logged)
	}
}

func TestRecordTokenUsageLogsAndAppendsFile(t *testing.T) {
	var buf bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	originalTokenLogPath := TOKEN_LOG_PATH
	log.SetOutput(&buf)
	log.SetFlags(0)
	TOKEN_LOG_PATH = filepath.Join(t.TempDir(), "token.log")
	defer log.SetOutput(originalWriter)
	defer log.SetFlags(originalFlags)
	defer func() {
		TOKEN_LOG_PATH = originalTokenLogPath
	}()

	agent := &Agent{
		Name:     "supervisor",
		Model:    "gpt-4o-mini",
		runStage: "executor",
	}
	usage := openai.CompletionUsage{
		PromptTokens:     120,
		CompletionTokens: 45,
		TotalTokens:      165,
	}
	usage.CompletionTokensDetails.ReasoningTokens = 9
	usage.CompletionTokensDetails.AudioTokens = 2
	usage.CompletionTokensDetails.AcceptedPredictionTokens = 7
	usage.CompletionTokensDetails.RejectedPredictionTokens = 1
	usage.PromptTokensDetails.CachedTokens = 30
	usage.PromptTokensDetails.AudioTokens = 3

	recordTokenUsage(agent, "", "stream_chat_completion", 2, "stop", usage)

	logged := buf.String()
	if !strings.Contains(logged, "[TokenUsage] agent=supervisor stage=executor kind=stream_chat_completion turn=3 model=gpt-4o-mini finish_reason=stop prompt_tokens=120 completion_tokens=45 total_tokens=165") {
		t.Fatalf("missing token usage log entry: %q", logged)
	}

	raw, err := os.ReadFile(TOKEN_LOG_PATH)
	if err != nil {
		t.Fatalf("read token log failed: %v", err)
	}
	tokenLog := string(raw)
	if !strings.Contains(tokenLog, "[TokenUsage] agent=supervisor stage=executor kind=stream_chat_completion turn=3 model=gpt-4o-mini finish_reason=stop prompt_tokens=120 completion_tokens=45 total_tokens=165") {
		t.Fatalf("missing token usage file entry: %q", tokenLog)
	}
}

func TestWithOpenAIRateLimitRetryRetries429WhenConfigured(t *testing.T) {
	t.Setenv(openAIRateLimitRetryEnv, "2.5")

	originalSleep := rateLimitSleep
	defer func() {
		rateLimitSleep = originalSleep
	}()

	var slept []time.Duration
	rateLimitSleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests}

	attempts := 0
	result, err := withOpenAIRateLimitRetry(context.Background(), "unit_test", func() (string, error) {
		attempts++
		if attempts == 1 {
			return "", &openai.Error{
				StatusCode: http.StatusTooManyRequests,
				Request:    req,
				Response:   resp,
			}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("withOpenAIRateLimitRetry failed: %v", err)
	}
	if result != "ok" {
		t.Fatalf("unexpected result: %q", result)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if len(slept) != 1 || slept[0] != 2500*time.Millisecond {
		t.Fatalf("unexpected sleep durations: %v", slept)
	}
}
