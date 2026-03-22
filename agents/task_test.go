package agents

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTaskManager(t *testing.T) {
	// Create a unique temporary directory for testing
	tempDir, err := os.MkdirTemp("", "task_manager_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Initialize TaskManager
	tm, err := NewTaskManager(tempDir)
	if err != nil {
		t.Fatalf("Failed to create TaskManager: %v", err)
	}

	// Test Create
	t.Run("Create", func(t *testing.T) {
		result, err := tm.Create("Test Task 1", "This is a test task")
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		var task Task
		if err := json.Unmarshal([]byte(result), &task); err != nil {
			t.Fatalf("Failed to parse task: %v", err)
		}

		if task.ID != 0 || task.Subject != "Test Task 1" || task.Status != "pending" {
			t.Errorf("Task not created correctly: %+v", task)
		}
	})

	// Test Get
	t.Run("Get", func(t *testing.T) {
		result, err := tm.Get(0)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}

		var task Task
		if err := json.Unmarshal([]byte(result), &task); err != nil {
			t.Fatalf("Failed to parse task: %v", err)
		}

		if task.ID != 0 {
			t.Errorf("Expected task ID 0, got %d", task.ID)
		}
	})

	// Test Update Status
	t.Run("Update Status", func(t *testing.T) {
		result, err := tm.Update(0, "in_progress", nil, nil)
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}

		var task Task
		if err := json.Unmarshal([]byte(result), &task); err != nil {
			t.Fatalf("Failed to parse task: %v", err)
		}

		if task.Status != "in_progress" {
			t.Errorf("Expected status 'in_progress', got '%s'", task.Status)
		}
	})

	// Test Create another task
	t.Run("Create Task 2", func(t *testing.T) {
		result, err := tm.Create("Test Task 2", "Second test task")
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		var task Task
		if err := json.Unmarshal([]byte(result), &task); err != nil {
			t.Fatalf("Failed to parse task: %v", err)
		}

		if task.ID != 1 {
			t.Errorf("Expected task ID 1, got %d", task.ID)
		}
	})

	// Test dependencies (Task 0 blocks Task 1)
	t.Run("Add Dependencies", func(t *testing.T) {
		result, err := tm.Update(0, "", nil, []int{1})
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}

		var task Task
		if err := json.Unmarshal([]byte(result), &task); err != nil {
			t.Fatalf("Failed to parse task: %v", err)
		}

		if len(task.Blocks) != 1 || task.Blocks[0] != 1 {
			t.Errorf("Expected task 0 to block task 1, got %v", task.Blocks)
		}

		// Check that task 1 has task 0 in blockedBy
		task1Result, err := tm.Get(1)
		if err != nil {
			t.Fatalf("Get task 1 failed: %v", err)
		}

		var task1 Task
		if err := json.Unmarshal([]byte(task1Result), &task1); err != nil {
			t.Fatalf("Failed to parse task 1: %v", err)
		}

		if len(task1.BlockedBy) != 1 || task1.BlockedBy[0] != 0 {
			t.Errorf("Expected task 1 to be blocked by task 0, got %v", task1.BlockedBy)
		}
	})

	// Test ListAll
	t.Run("ListAll", func(t *testing.T) {
		result, err := tm.ListAll()
		if err != nil {
			t.Fatalf("ListAll failed: %v", err)
		}

		if result == "No tasks." {
			t.Error("Expected tasks to be listed, but got 'No tasks.'")
		}

		if len(result) == 0 {
			t.Error("ListAll returned empty string")
		}
	})

	// Test Mark task as completed (should clear dependency)
	t.Run("Complete Task and Clear Dependency", func(t *testing.T) {
		_, err := tm.Update(0, "completed", nil, nil)
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}

		// Check that task 1 no longer has task 0 in blockedBy
		result, err := tm.Get(1)
		if err != nil {
			t.Fatalf("Get task 1 failed: %v", err)
		}

		var task1 Task
		if err := json.Unmarshal([]byte(result), &task1); err != nil {
			t.Fatalf("Failed to parse task 1: %v", err)
		}

		if len(task1.BlockedBy) != 0 {
			t.Errorf("Expected task 1 to have no blockedBy, got %v", task1.BlockedBy)
		}
	})

	// Test Delete
	t.Run("Delete", func(t *testing.T) {
		err := tm.Delete(1)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		// Try to get the deleted task
		_, err = tm.Get(1)
		if err == nil {
			t.Error("Expected error when getting deleted task, but got none")
		}
	})
}

func TestTaskManagerHelpers(t *testing.T) {
	t.Run("extractTaskID", func(t *testing.T) {
		id := extractTaskID("task_123.json")
		if id != 123 {
			t.Errorf("Expected 123, got %d", id)
		}
	})

	t.Run("statusMarker", func(t *testing.T) {
		tests := []struct {
			status   string
			expected string
		}{
			{"pending", "[ ]"},
			{"in_progress", "[>]"},
			{"completed", "[x]"},
			{"unknown", "[?]"},
		}

		for _, test := range tests {
			result := statusMarker(test.status)
			if result != test.expected {
				t.Errorf("statusMarker(%s) = %s, expected %s", test.status, result, test.expected)
			}
		}
	})

	t.Run("containsInt", func(t *testing.T) {
		slice := []int{1, 2, 3, 4, 5}
		if !containsInt(slice, 3) {
			t.Error("Expected containsInt to return true for 3")
		}
		if containsInt(slice, 10) {
			t.Error("Expected containsInt to return false for 10")
		}
	})

	t.Run("removeInt", func(t *testing.T) {
		slice := []int{1, 2, 3, 4, 5}
		result := removeInt(slice, 3)
		if len(result) != 4 {
			t.Errorf("Expected length 4, got %d", len(result))
		}
		if containsInt(result, 3) {
			t.Error("Expected 3 to be removed from slice")
		}
	})

	t.Run("uniqueIntSlice", func(t *testing.T) {
		slice := []int{3, 1, 2, 1, 3, 5, 4}
		result := uniqueIntSlice(slice)
		expected := []int{1, 2, 3, 4, 5}

		if len(result) != len(expected) {
			t.Errorf("Expected length %d, got %d", len(expected), len(result))
		}

		for i, v := range expected {
			if i >= len(result) || result[i] != v {
				t.Errorf("Expected %v, got %v", expected, result)
				break
			}
		}
	})
}

func TestBackgroundManager(t *testing.T) {
	t.Run("RunCompletesAndQueuesNotification", func(t *testing.T) {
		bm := NewBackgroundManager()

		result := bm.Run("printf 'background-ok'")
		if !strings.Contains(result, "Background task ") || !strings.Contains(result, "started:") {
			t.Fatalf("unexpected run result: %s", result)
		}

		var notifications []BackgroundNotification
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			notifications = bm.DrainNotifications()
			if len(notifications) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if len(notifications) != 1 {
			t.Fatalf("expected 1 notification, got %d", len(notifications))
		}

		notification := notifications[0]
		if notification.Status != "completed" {
			t.Fatalf("expected completed status, got %s", notification.Status)
		}
		if notification.Result != "background-ok" {
			t.Fatalf("unexpected notification result: %q", notification.Result)
		}

		if secondDrain := bm.DrainNotifications(); len(secondDrain) != 0 {
			t.Fatalf("expected queue to be empty after drain, got %d notifications", len(secondDrain))
		}

		checkAll := bm.Check("")
		if !strings.Contains(checkAll, "[completed]") {
			t.Fatalf("expected completed task in list, got %q", checkAll)
		}
		if !strings.Contains(checkAll, notification.TaskID) {
			t.Fatalf("expected task id in list, got %q", checkAll)
		}

		checkOne := bm.Check(notification.TaskID)
		if !strings.Contains(checkOne, "background-ok") {
			t.Fatalf("expected task result in check output, got %q", checkOne)
		}
	})
}
