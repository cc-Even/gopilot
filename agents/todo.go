package agents

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

type TodoItems struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status,omitempty" validate:"oneof=pending in_progress completed" default:"pending"`
}

func UpdateTodoTool(_ context.Context, input string, agent *OpenAIAgent) (string, error) {
	items, err := parseTodoItems(input)
	if err != nil {
		return "input format error", err
	}
	return UpdateTodo(items), nil
}

func parseTodoItems(input string) ([]TodoItems, error) {
	var items []TodoItems
	err := json.Unmarshal([]byte(input), &items)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func UpdateTodo(items []TodoItems) string {
	var validated []TodoItems
	inProgressCount := 0

	for _, item := range items {
		status := item.Status
		if status == "in_progress" {
			inProgressCount += 1
		}
		validated = append(validated, item)
	}
	if inProgressCount > 1 {
		panic("Only one task can be in_progress")
	}

	if len(validated) == 0 {
		return "No todos."
	}

	lines := make([]string, 0, len(validated))
	done := 0
	for _, item := range validated {
		var marker string
		switch item.Status {
		case "pending":
			marker = "[ ]"
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
		default:
			marker = "[?]"
		}
		lines = append(lines, marker+" #"+itoa(item.ID)+": "+item.Text)
		if item.Status == "completed" {
			done++
		}
	}
	lines = append(lines, "\n("+itoa(done)+"/"+itoa(len(validated))+" completed)")
	return joinLines(lines)
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
