package agents

import "testing"

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
