package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	expectedPlannerTools := []string{"todo", "task_create", "task_update", "task_list", "task_get"}
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
