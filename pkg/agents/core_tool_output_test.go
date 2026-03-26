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

func TestToolResultCompactDoesNotTruncateReadFile(t *testing.T) {
	longOutput := "line1\n" + strings.Repeat("x", 1400)

	compacted := toolResultCompact(longOutput, "read_file")
	if compacted != longOutput {
		t.Fatalf("expected read_file output to remain unchanged")
	}
	if strings.Contains(compacted, "(truncated)") {
		t.Fatalf("read_file output should not be truncated: %q", compacted)
	}
}

func TestCompactToolDisplaySummarizesWriteFileArguments(t *testing.T) {
	compacted := compactToolDisplay(`{"path":"notes.txt","content":"`+strings.Repeat("x", 400)+`"}`, "write_file")
	if !strings.Contains(compacted, `"path": "notes.txt"`) {
		t.Fatalf("expected path in compacted output, got %q", compacted)
	}
	if !strings.Contains(compacted, `"content_bytes": 400`) {
		t.Fatalf("expected content_bytes in compacted output, got %q", compacted)
	}
	if !strings.Contains(compacted, `"content_preview": "`) {
		t.Fatalf("expected content_preview in compacted output, got %q", compacted)
	}
	if !strings.Contains(compacted, "(truncated)") {
		t.Fatalf("expected truncated marker in compacted output, got %q", compacted)
	}
}
