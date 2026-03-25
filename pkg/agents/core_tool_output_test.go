package agents

import (
	"strings"
	"testing"
)

func TestToolResultCompactKeepsLeadingOutput(t *testing.T) {
	longOutput := "Invoke-RestMethod : request failed\n" + strings.Repeat("x", 1400)

	compacted := toolResultCompact(longOutput, "bash")
	if !strings.HasPrefix(compacted, "Invoke-RestMethod : request failed") {
		t.Fatalf("expected leading output to be preserved, got %q", compacted)
	}
	if !strings.Contains(compacted, "(truncated)") {
		t.Fatalf("expected truncated marker, got %q", compacted)
	}
	if strings.Contains(compacted, "Previous: used bash") {
		t.Fatalf("unexpected placeholder output: %q", compacted)
	}
}
