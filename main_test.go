package main

import (
	"claude-go/pkg/agents"
	"testing"
)

func TestParsePlanningPolicyArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    agents.PlanningPolicy
		wantErr bool
	}{
		{name: "default auto", args: nil, want: agents.PlanningPolicyAuto},
		{name: "force on flag", args: []string{"--plan"}, want: agents.PlanningPolicyRequired},
		{name: "force off flag", args: []string{"--no-plan"}, want: agents.PlanningPolicySkip},
		{name: "explicit on mode", args: []string{"--plan-mode=on"}, want: agents.PlanningPolicyRequired},
		{name: "explicit off mode", args: []string{"--plan-mode=off"}, want: agents.PlanningPolicySkip},
		{name: "explicit auto mode", args: []string{"--plan-mode=auto"}, want: agents.PlanningPolicyAuto},
		{name: "conflicting enable flags", args: []string{"--plan", "--no-plan"}, wantErr: true},
		{name: "conflicting plan mode and flag", args: []string{"--plan", "--plan-mode=off"}, wantErr: true},
		{name: "unknown mode", args: []string{"--plan-mode=maybe"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePlanningPolicyArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got policy %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlanningPolicyArgs returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected policy: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestHandlePlanCommand(t *testing.T) {
	session := &cliSession{
		planningPolicy: agents.PlanningPolicyAuto,
		updateHeader:   func() {},
		liveBlocks:     make(map[string]liveOutputBlock),
	}

	session.handlePlanCommand([]string{"on"})
	if session.planningPolicy != agents.PlanningPolicyRequired {
		t.Fatalf("expected session planning policy to be required, got %q", session.planningPolicy)
	}
	if currentPlanningPolicy != agents.PlanningPolicyRequired {
		t.Fatalf("expected global planning policy to be required, got %q", currentPlanningPolicy)
	}

	session.handlePlanCommand([]string{"off"})
	if session.planningPolicy != agents.PlanningPolicySkip {
		t.Fatalf("expected session planning policy to be skip, got %q", session.planningPolicy)
	}
}
