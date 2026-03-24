package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type TodoItems struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status,omitempty" validate:"oneof=pending in_progress completed" default:"pending"`
}

func UpdateTodoTool(_ context.Context, input string, agent *Agent) (string, error) {
	items, err := parseTodoItems(input)
	if err != nil {
		return "input format error", err
	}
	return UpdateTodo(items)
}

func parseTodoItems(input string) ([]TodoItems, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("todo args empty")
	}

	// Keep backward compatibility with the legacy top-level array format while
	// making the primary contract an object with an items field.
	if strings.HasPrefix(trimmed, "[") {
		var items []TodoItems
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, err
		}
		return items, nil
	}

	var payload struct {
		Items *[]TodoItems `json:"items"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, err
	}
	if payload.Items == nil {
		return nil, fmt.Errorf("todo args missing items")
	}
	return *payload.Items, nil
}

func UpdateTodo(items []TodoItems) (string, error) {
	var validated []TodoItems
	inProgressCount := 0

	for _, item := range items {
		status := item.Status
		if status == "" {
			status = taskStatusPending
			item.Status = status
		}
		if !isValidTaskStatus(status) {
			return "", fmt.Errorf("invalid todo status: %s", status)
		}
		if status == taskStatusInProgress {
			inProgressCount += 1
		}
		validated = append(validated, item)
	}
	if inProgressCount > 1 {
		return "", fmt.Errorf("only one task can be in_progress")
	}

	if len(validated) == 0 {
		return "No todos.", nil
	}

	lines := make([]string, 0, len(validated))
	done := 0
	for _, item := range validated {
		var marker string
		switch item.Status {
		case taskStatusPending:
			marker = "[ ]"
		case taskStatusInProgress:
			marker = "[>]"
		case taskStatusCompleted:
			marker = "[x]"
		default:
			marker = "[?]"
		}
		lines = append(lines, marker+" #"+itoa(item.ID)+": "+item.Text)
		if item.Status == taskStatusCompleted {
			done++
		}
	}
	lines = append(lines, "\n("+itoa(done)+"/"+itoa(len(validated))+" completed)")
	return joinLines(lines), nil
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
