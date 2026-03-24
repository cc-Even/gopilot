package agents

import (
	"context"
	"encoding/json"
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

	if _, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent); err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
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

	if _, err := toolMap["route_to_subagent"].Handler(context.Background(), args, parent); err != nil {
		t.Fatalf("route_to_subagent failed: %v", err)
	}
	if subAgentModel != "explicit-model" {
		t.Fatalf("expected explicit model to win, got %q", subAgentModel)
	}
}
