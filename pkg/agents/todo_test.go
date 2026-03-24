package agents

import (
	"strings"
	"testing"
)

func TestUpdateTodoRejectsMultipleInProgress(t *testing.T) {
	_, err := UpdateTodo([]TodoItems{
		{ID: 1, Text: "first", Status: taskStatusInProgress},
		{ID: 2, Text: "second", Status: taskStatusInProgress},
	})
	if err == nil {
		t.Fatal("expected multiple in_progress todos to fail")
	}
}

func TestUpdateTodoDefaultsEmptyStatusToPending(t *testing.T) {
	result, err := UpdateTodo([]TodoItems{
		{ID: 1, Text: "first"},
	})
	if err != nil {
		t.Fatalf("UpdateTodo failed: %v", err)
	}
	if want := "[ ] #1: first"; result[:len(want)] != want {
		t.Fatalf("expected pending marker, got %q", result)
	}
}

func TestUpdateTodoToolAcceptsObjectPayload(t *testing.T) {
	result, err := UpdateTodoTool(nil, `{"items":[{"id":1,"text":"first","status":"in_progress"},{"id":2,"text":"second","status":"pending"}]}`, nil)
	if err != nil {
		t.Fatalf("UpdateTodoTool failed: %v", err)
	}
	if !strings.Contains(result, "[>] #1: first") {
		t.Fatalf("expected in_progress item in result, got %q", result)
	}
	if !strings.Contains(result, "[ ] #2: second") {
		t.Fatalf("expected pending item in result, got %q", result)
	}
}

func TestUpdateTodoToolKeepsLegacyArrayPayload(t *testing.T) {
	result, err := UpdateTodoTool(nil, `[{"id":1,"text":"first","status":"completed"}]`, nil)
	if err != nil {
		t.Fatalf("UpdateTodoTool failed: %v", err)
	}
	if !strings.Contains(result, "[x] #1: first") {
		t.Fatalf("expected completed item in result, got %q", result)
	}
}

func TestUpdateTodoToolRejectsMissingItems(t *testing.T) {
	_, err := UpdateTodoTool(nil, `{}`, nil)
	if err == nil {
		t.Fatal("expected missing items payload to fail")
	}
}
