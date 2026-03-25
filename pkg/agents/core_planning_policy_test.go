package agents

import "testing"

func TestParsePlanningPolicy(t *testing.T) {
	tests := []struct {
		input   string
		want    PlanningPolicy
		wantErr bool
	}{
		{input: "", want: PlanningPolicyAuto},
		{input: "auto", want: PlanningPolicyAuto},
		{input: "on", want: PlanningPolicyRequired},
		{input: "required", want: PlanningPolicyRequired},
		{input: "off", want: PlanningPolicySkip},
		{input: "skip", want: PlanningPolicySkip},
		{input: "invalid", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParsePlanningPolicy(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("ParsePlanningPolicy(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParsePlanningPolicy(%q) returned error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("ParsePlanningPolicy(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPlanningPolicyLabel(t *testing.T) {
	tests := []struct {
		policy PlanningPolicy
		want   string
	}{
		{policy: PlanningPolicyAuto, want: "AUTO"},
		{policy: PlanningPolicyRequired, want: "FORCED ON"},
		{policy: PlanningPolicySkip, want: "FORCED OFF"},
		{policy: PlanningPolicy("unexpected"), want: "AUTO"},
	}

	for _, tt := range tests {
		if got := PlanningPolicyLabel(tt.policy); got != tt.want {
			t.Fatalf("PlanningPolicyLabel(%q) = %q, want %q", tt.policy, got, tt.want)
		}
	}
}
