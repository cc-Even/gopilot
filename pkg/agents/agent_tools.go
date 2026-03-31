package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
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
				return marshalSubagentOutcome(resolvedName, "failed", "", err)
			}
			agent.reportStageOutput(
				fmt.Sprintf("SubAgent %s", resolvedName),
				fmt.Sprintf("输出结果:\n%s", strings.TrimSpace(result)),
			)
			return marshalSubagentOutcome(resolvedName, "completed", result, nil)
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
		Description: "Finish planner stage and transfer a concise execution brief to the executor after the task board has been created or refreshed. Always pass {\"plan_summary\":\"...\",\"current_task\":\"...\",\"task_board_updated\":true} and optionally notes. Use notes for blockers, sequencing, and any recommended spawn_teammate parallelization.",
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
					"description": "Optional blockers, assumptions, sequencing notes, or recommended teammate parallelization for the executor",
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

func (a *Agent) executeTool(ctx context.Context, name string, rawArgs json.RawMessage) (string, error) {
	t, ok := a.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Handler(ctx, rawArgs, a)
}

func toolResultCompact(output, toolName string) string {
	const limit = 1200
	trimmed := strings.TrimSpace(output)
	if toolName == "read_file" || toolName == "code_outline" {
		return trimmed
	}
	if utf8.RuneCountInString(trimmed) > limit {
		return truncate(trimmed, limit) + "\n... (truncated)"
	}
	return trimmed
}
