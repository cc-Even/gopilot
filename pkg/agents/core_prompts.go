package agents

import (
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
)

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
