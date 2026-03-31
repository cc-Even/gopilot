package agents

import (
	"context"
	"strings"

	"github.com/openai/openai-go/v3"
)

const PlanningPolicyAuto PlanningPolicy = "auto"

const PlanningPolicyRequired PlanningPolicy = "required"

const PlanningPolicySkip PlanningPolicy = "skip"

var plannerToolAllowlist = map[string]struct{}{
	"task_create":         {},
	"task_update":         {},
	"task_list":           {},
	"task_get":            {},
	"list_file":           {},
	"code_outline":        {},
	"read_file":           {},
	"bash":                {},
	"ask_user":            {},
	"handoff_to_executor": {},
}

func countsAsPlanningTool(toolName string) bool {
	toolName = strings.TrimSpace(toolName)
	return toolName == "todo" || strings.HasPrefix(toolName, "task_")
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

func (a *Agent) plannerSystemPrompt() string {
	rules := strings.Join([]string{
		"You are in Planner stage.",
		"You are an experienced software architect. Your only job is to design the architecture and break down tasks.",
		"Do not edit the file or attempt any implementation work.",
		"Prefer task_list/task_get before creating or updating tasks so you reuse the current board when possible.",
		"Do not stop at prose only: create or update task board entries for the concrete execution steps you want the executor to follow.",
		"If the work has substantial independent subtasks, explicitly mark which tasks can run in parallel and recommend where the executor should use spawn_teammate, including a suggested teammate role and expected deliverable.",
		"Do not invent parallelism for tightly coupled or trivial steps; only recommend teammate delegation when it will materially reduce wall-clock time or unblock the main thread.",
		"If critical information is missing, call ask_user with one concise blocking question instead of guessing.",
		"When you have enough information and the task board is up to date, call handoff_to_executor to transfer a concise execution brief and the current unfinished task.",
		"Do not return a normal final answer to end planning; use handoff_to_executor once planning is complete.",
	}, " ")
	return rules
}

func (a *Agent) plannerContextMessage() string {
	lines := []string{
		"<planning_rule>",
		"Create or refresh the execution plan before any implementation work starts.",
		"Use the available todo/task tools to capture the plan, and ensure each meaningful execution step exists on the task board.",
		"When multiple substantial tasks are independent, split them into separate runnable tasks, record their dependencies, and state which ones the executor should consider delegating to teammates in parallel.",
		"If you are blocked on missing user information, pause with ask_user.",
		"When the plan and task board are ready, call handoff_to_executor with the concise handoff summary and current unfinished task.",
		"</planning_rule>",
		"<current_task_board>",
		a.currentTaskBoardSummary(),
		"</current_task_board>",
	}
	return strings.Join(lines, "\n")
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
	return !isSimpleDirectExecutionRequest(ctx, a.provider, a.Model, latest)
}
