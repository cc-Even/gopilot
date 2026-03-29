package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestRouteToSubagentUsesParentModelWhenSubagentModelUnset(t *testing.T) {
	var subAgentModel string
	subAgent := &Agent{
		Name:         "reviewer",
		SystemPrompt: "review changes",
		Model:        "bootstrap-model",
		InheritModel: true,
		runLoopOverride: func(current *Agent, _ context.Context, _ []openai.ChatCompletionMessageParamUnion) (string, error) {
			subAgentModel = current.Model
			return "ok", nil
		},
	}

	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"reviewer": subAgent},
	}

	toolMap := map[string]ToolDefinition{}
	order := registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)
	if len(order) != 1 {
		t.Fatalf("expected route_to_subagent registration, got %v", order)
	}

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": "reviewer",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	output, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(output), &outcome); err != nil {
		t.Fatalf("parse subagent output failed: %v", err)
	}
	if outcome["status"] != "completed" || outcome["result"] != "ok" {
		t.Fatalf("unexpected subagent outcome: %+v", outcome)
	}
	if subAgentModel != "parent-model" {
		t.Fatalf("expected inherited parent model, got %q", subAgentModel)
	}
	if subAgent.Model != "bootstrap-model" {
		t.Fatalf("expected base sub-agent model to remain unchanged, got %q", subAgent.Model)
	}
}

func TestRouteToSubagentKeepsExplicitModel(t *testing.T) {
	var subAgentModel string
	subAgent := &Agent{
		Name:         "reviewer",
		SystemPrompt: "review changes",
		Model:        "explicit-model",
		runLoopOverride: func(current *Agent, _ context.Context, _ []openai.ChatCompletionMessageParamUnion) (string, error) {
			subAgentModel = current.Model
			return "ok", nil
		},
	}

	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"reviewer": subAgent},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": "reviewer",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	output, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(output), &outcome); err != nil {
		t.Fatalf("parse subagent output failed: %v", err)
	}
	if outcome["status"] != "completed" || outcome["result"] != "ok" {
		t.Fatalf("unexpected subagent outcome: %+v", outcome)
	}
	if subAgentModel != "explicit-model" {
		t.Fatalf("expected explicit model to win, got %q", subAgentModel)
	}
}

func TestRouteToSubagentReportsOutput(t *testing.T) {
	var stages []string
	var contents []string
	subAgent := &Agent{
		Name:         "reviewer",
		SystemPrompt: "review changes",
		Model:        "explicit-model",
		runLoopOverride: func(current *Agent, _ context.Context, _ []openai.ChatCompletionMessageParamUnion) (string, error) {
			return "found one bug", nil
		},
	}

	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"reviewer": subAgent},
		stageOutputReporter: func(stage, content string) {
			stages = append(stages, stage)
			contents = append(contents, content)
		},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": "reviewer",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	output, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(output), &outcome); err != nil {
		t.Fatalf("parse subagent output failed: %v", err)
	}
	if outcome["status"] != "completed" || outcome["result"] != "found one bug" {
		t.Fatalf("unexpected subagent outcome: %+v", outcome)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 reported events, got %d", len(stages))
	}
	if stages[0] != "SubAgent reviewer" || stages[1] != "SubAgent reviewer" {
		t.Fatalf("unexpected stages: %v", stages)
	}
	if contents[0] != "开始处理子任务:\ncheck this patch" {
		t.Fatalf("unexpected start content: %q", contents[0])
	}
	if contents[1] != "输出结果:\nfound one bug" {
		t.Fatalf("unexpected result content: %q", contents[1])
	}
}

func TestRouteToSubagentMatchesNormalizedName(t *testing.T) {
	var called bool
	subAgent := &Agent{
		Name:         "code-reviewer",
		SystemPrompt: "review changes",
		Model:        "explicit-model",
		runLoopOverride: func(current *Agent, _ context.Context, _ []openai.ChatCompletionMessageParamUnion) (string, error) {
			called = true
			return "ok", nil
		},
	}

	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"code-reviewer": subAgent},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": " Code Reviewer ",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	output, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(output), &outcome); err != nil {
		t.Fatalf("parse subagent output failed: %v", err)
	}
	if outcome["status"] != "completed" || outcome["sub_agent_name"] != "code-reviewer" {
		t.Fatalf("unexpected subagent outcome: %+v", outcome)
	}
	if !called {
		t.Fatal("expected normalized sub-agent name to match")
	}
}

func TestRouteToSubagentReturnsStructuredFailure(t *testing.T) {
	subAgent := &Agent{
		Name:         "reviewer",
		SystemPrompt: "review changes",
		Model:        "explicit-model",
		runLoopOverride: func(current *Agent, _ context.Context, _ []openai.ChatCompletionMessageParamUnion) (string, error) {
			return "", errors.New("chat completion failed (turn=0): i/o timeout")
		},
	}

	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"reviewer": subAgent},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": "reviewer",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	output, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err != nil {
		t.Fatalf("route_to_subagent should return structured failure, got error: %v", err)
	}

	var outcome map[string]any
	if err := json.Unmarshal([]byte(output), &outcome); err != nil {
		t.Fatalf("parse subagent output failed: %v", err)
	}
	if outcome["status"] != "failed" || outcome["sub_agent_name"] != "reviewer" {
		t.Fatalf("unexpected subagent outcome: %+v", outcome)
	}
	if outcome["retryable"] != true {
		t.Fatalf("expected retryable structured failure, got %+v", outcome)
	}
	if !strings.Contains(outcome["error"].(string), "i/o timeout") {
		t.Fatalf("unexpected structured error: %+v", outcome)
	}
}

func TestRouteToSubagentUnknownIncludesAvailableNames(t *testing.T) {
	parent := &Agent{
		Name:  "supervisor",
		Model: "parent-model",
		SubAgents: map[string]*Agent{
			"code-reviewer": {Name: "code-reviewer"},
			"helper":        {Name: "helper"},
		},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	args, err := json.Marshal(map[string]string{
		"sub_agent_name": "missing-agent",
		"input":          "check this patch",
	})
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	_, err = toolMap["route_to_subagent"].Handler(context.Background(), args, parent)
	if err == nil {
		t.Fatal("expected route_to_subagent to fail for unknown sub-agent")
	}
	if !strings.Contains(err.Error(), "Available: code-reviewer, helper") {
		t.Fatalf("expected available names in error, got %q", err.Error())
	}
}

func TestRouteToSubagentDefinesStrictSchema(t *testing.T) {
	subAgents := map[string]*Agent{
		"code-reviewer": {Name: "code-reviewer"},
		"helper":        {Name: "helper"},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, subAgents)

	schema := toolMap["route_to_subagent"].Parameters
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", schema["additionalProperties"])
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties has unexpected type %T", schema["properties"])
	}
	nameSchema, ok := properties["sub_agent_name"].(map[string]any)
	if !ok {
		t.Fatalf("sub_agent_name schema has unexpected type %T", properties["sub_agent_name"])
	}
	enumValues, ok := nameSchema["enum"].([]string)
	if !ok {
		t.Fatalf("sub_agent_name enum has unexpected type %T", nameSchema["enum"])
	}
	if !reflect.DeepEqual(enumValues, []string{"code-reviewer", "helper"}) {
		t.Fatalf("enum values = %v, want [code-reviewer helper]", enumValues)
	}
}

func TestRouteToSubagentRejectsMissingArgs(t *testing.T) {
	parent := &Agent{
		Name:      "supervisor",
		Model:     "parent-model",
		SubAgents: map[string]*Agent{"code-reviewer": {Name: "code-reviewer"}},
	}

	toolMap := map[string]ToolDefinition{}
	registerRouteToSubagentTool(toolMap, nil, parent.SubAgents)

	_, err := toolMap["route_to_subagent"].Handler(context.Background(), json.RawMessage(`{}`), parent)
	if err == nil || !strings.Contains(err.Error(), "missing sub_agent_name") {
		t.Fatalf("expected missing sub_agent_name error, got %v", err)
	}
}
