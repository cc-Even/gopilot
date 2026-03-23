package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessageBusSendReadAndBroadcast(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)

	result := bus.Send("alice", "bob", "hello", "message", map[string]any{"topic": "test"})
	if !strings.Contains(result, "Sent message to bob") {
		t.Fatalf("unexpected send result: %s", result)
	}

	messages := bus.ReadInbox("bob")
	if len(messages) != 1 {
		t.Fatalf("expected 1 inbox message, got %d", len(messages))
	}
	if messages[0].From != "alice" || messages[0].Content != "hello" || messages[0].Type != "message" {
		t.Fatalf("unexpected message: %+v", messages[0])
	}

	drained := bus.ReadInbox("bob")
	if len(drained) != 0 {
		t.Fatalf("expected inbox to be drained, got %d messages", len(drained))
	}

	broadcastResult := bus.Broadcast("alice", "team update", []string{"alice", "bob", "carol"})
	if !strings.Contains(broadcastResult, "Broadcast to 2 teammates") {
		t.Fatalf("unexpected broadcast result: %s", broadcastResult)
	}
	if len(bus.ReadInbox("bob")) != 1 {
		t.Fatalf("expected broadcast for bob")
	}
	if len(bus.ReadInbox("carol")) != 1 {
		t.Fatalf("expected broadcast for carol")
	}

	raw, err := os.ReadFile(talkPath)
	if err != nil {
		t.Fatalf("read talk log failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 talk log lines, got %d: %q", len(lines), string(raw))
	}
	if !strings.Contains(lines[0], "from=alice") || !strings.Contains(lines[0], "to=bob") || !strings.Contains(lines[0], `content="hello"`) {
		t.Fatalf("unexpected first talk log line: %s", lines[0])
	}
	if !strings.Contains(lines[1], `content="team update"`) || !strings.Contains(lines[2], `content="team update"`) {
		t.Fatalf("unexpected broadcast talk log: %q", string(raw))
	}
}

func TestTeammateManagerSpawnPersistsConfigAndResetsStatus(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	runnerCalled := make(chan struct{}, 1)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		runnerCalled <- struct{}{}
		return nil
	}

	result := manager.Spawn("worker-1", "reviewer", "inspect core changes", "")
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	member := manager.findMemberLocked("worker-1")
	if member == nil || member.Status == teammateStatusIdle {
		manager.mu.Unlock()
		t.Fatalf("expected spawned teammate to stay working, got %+v", member)
	}
	if len(manager.threads) != 0 {
		manager.mu.Unlock()
		t.Fatalf("spawn should not register running thread, got %d", len(manager.threads))
	}
	manager.mu.Unlock()

	raw, err := os.ReadFile(filepath.Join(teamDir, "config.json"))
	if err != nil {
		t.Fatalf("read config failed: %v", err)
	}
	if !strings.Contains(string(raw), `"name": "worker-1"`) {
		t.Fatalf("config missing teammate entry: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"status": "working"`) {
		t.Fatalf("config missing working status: %s", string(raw))
	}
}

func TestTeammateManagerWakeReusesOriginalPrompt(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)

	prompts := make(chan string, 2)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		prompts <- prompt
		return nil
	}

	result := manager.Spawn("worker-1", "reviewer", "inspect core changes", "")
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	wakeResult := manager.Wake("worker-1")
	if !strings.Contains(wakeResult, `Woke "worker-1"`) {
		t.Fatalf("unexpected wake result: %s", wakeResult)
	}

	select {
	case got := <-prompts:
		if got != "inspect core changes" {
			t.Fatalf("wake should reuse original prompt, got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("wake prompt not received")
	}
}

func TestTeammateManagerNextIdleEventPrefersInbox(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	manager.bus.Send("lead", "worker-1", "check inbox first", "message", nil)
	message, err := manager.nextIdleEvent(agent)
	if err != nil {
		t.Fatalf("nextIdleEvent failed: %v", err)
	}
	if !strings.Contains(message, "<inbox>") || !strings.Contains(message, "check inbox first") {
		t.Fatalf("expected inbox message, got %q", message)
	}

	taskJSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	var task Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		t.Fatalf("parse task failed: %v", err)
	}
	if task.Owner != "" || task.Status != "pending" {
		t.Fatalf("inbox should not claim task yet, got %+v", task)
	}
}

func TestTeammateManagerNextIdleEventClaimsTask(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	message, err := manager.nextIdleEvent(agent)
	if err != nil {
		t.Fatalf("nextIdleEvent failed: %v", err)
	}
	if !strings.Contains(message, "<task_claim>") || !strings.Contains(message, `"owner": "worker-1"`) {
		t.Fatalf("expected claimed task payload, got %q", message)
	}

	taskJSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	var task Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		t.Fatalf("parse task failed: %v", err)
	}
	if task.Owner != "worker-1" || task.Status != "in_progress" {
		t.Fatalf("expected claimed task persisted, got %+v", task)
	}
}

func TestClaimTaskToolClaimsTaskEvenWithInboxMessages(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	manager.bus.Send("lead", "worker-1", "please read later", "message", nil)

	result, err := claimTaskTool(context.Background(), json.RawMessage(`{}`), agent)
	if err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}
	if !strings.Contains(result, "<task_claim>") || !strings.Contains(result, `"owner": "worker-1"`) {
		t.Fatalf("expected claimed task payload, got %q", result)
	}

	messages := manager.bus.ReadInbox("worker-1")
	if len(messages) != 1 || messages[0].Content != "please read later" {
		t.Fatalf("claim_task should not drain inbox, got %+v", messages)
	}
}

func TestClaimTaskToolClaimsSpecificTaskID(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("first", "blocked task"); err != nil {
		t.Fatalf("create task 0 failed: %v", err)
	}
	if _, err := taskManager.Create("second", "claim this one"); err != nil {
		t.Fatalf("create task 1 failed: %v", err)
	}
	if _, err := taskManager.Update(1, "", []int{0}, nil); err != nil {
		t.Fatalf("block task 1 failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	result, err := claimTaskTool(context.Background(), json.RawMessage(`{"task_id":0}`), agent)
	if err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}
	if !strings.Contains(result, "<task_claim>") || !strings.Contains(result, `"id": 0`) {
		t.Fatalf("expected claimed task 0 payload, got %q", result)
	}

	task0JSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task 0 failed: %v", err)
	}
	var task0 Task
	if err := json.Unmarshal([]byte(task0JSON), &task0); err != nil {
		t.Fatalf("parse task 0 failed: %v", err)
	}
	if task0.Owner != "worker-1" || task0.Status != "in_progress" {
		t.Fatalf("expected task 0 claimed, got %+v", task0)
	}

	task1JSON, err := taskManager.Get(1)
	if err != nil {
		t.Fatalf("get task 1 failed: %v", err)
	}
	var task1 Task
	if err := json.Unmarshal([]byte(task1JSON), &task1); err != nil {
		t.Fatalf("parse task 1 failed: %v", err)
	}
	if task1.Owner != "" || task1.Status != "pending" {
		t.Fatalf("expected task 1 untouched, got %+v", task1)
	}
}

func TestClaimTaskToolRejectsBlockedSpecificTaskID(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("dependency", "must finish first"); err != nil {
		t.Fatalf("create task 0 failed: %v", err)
	}
	if _, err := taskManager.Create("blocked", "cannot claim yet"); err != nil {
		t.Fatalf("create task 1 failed: %v", err)
	}
	if _, err := taskManager.Update(1, "", []int{0}, nil); err != nil {
		t.Fatalf("block task 1 failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	_, err = claimTaskTool(context.Background(), json.RawMessage(`{"task_id":1}`), agent)
	if err == nil {
		t.Fatal("expected blocked task claim to fail")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked error, got %v", err)
	}
}

func TestTeammateManagerWaitUntilIdleReturnsForIdleThreads(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	manager.Spawn("worker-1", "reviewer", "inspect core changes", "")

	manager.mu.Lock()
	manager.threads["worker-1"] = func() {}
	member := manager.findMemberLocked("worker-1")
	member.Status = teammateStatusIdle
	manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := manager.WaitUntilIdle(ctx); err != nil {
		t.Fatalf("WaitUntilIdle should return for idle teammate, got %v", err)
	}
}

func waitForCondition(t *testing.T, fn func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
