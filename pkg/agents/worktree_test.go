package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeManagerCreateRemoveCompletesTask(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("Implement auth refactor", "split auth module"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}

	record, err := manager.Create("auth-refactor", intPtr(0))
	if err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}
	if record.Name != "auth-refactor" {
		t.Fatalf("unexpected worktree name: %+v", record)
	}
	if record.TaskID == nil || *record.TaskID != 0 {
		t.Fatalf("expected task binding, got %+v", record)
	}
	if _, err := os.Stat(record.Path); err != nil {
		t.Fatalf("expected worktree path to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".worktrees")); !os.IsNotExist(err) {
		t.Fatalf("expected assistant state to stay out of repo, stat err=%v", err)
	}

	task := mustLoadTask(t, taskManager, 0)
	if task.Status != "in_progress" {
		t.Fatalf("expected task to become in_progress, got %q", task.Status)
	}
	if task.Worktree != "auth-refactor" {
		t.Fatalf("expected task to bind worktree, got %+v", task)
	}

	removed, err := manager.Remove("auth-refactor", false, true)
	if err != nil {
		t.Fatalf("remove worktree failed: %v", err)
	}
	if removed.Status != worktreeStatusRemoved {
		t.Fatalf("expected removed status, got %+v", removed)
	}
	if _, err := os.Stat(record.Path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removed, stat err=%v", err)
	}

	task = mustLoadTask(t, taskManager, 0)
	if task.Status != "completed" {
		t.Fatalf("expected task completed, got %+v", task)
	}
	if task.Worktree != "" {
		t.Fatalf("expected task worktree unbound, got %+v", task)
	}

	indexRaw, err := os.ReadFile(filepath.Join(worktreeDir, "index.json"))
	if err != nil {
		t.Fatalf("read index failed: %v", err)
	}
	if !strings.Contains(string(indexRaw), `"status": "removed"`) {
		t.Fatalf("index missing removed status: %s", string(indexRaw))
	}

	eventsRaw, err := os.ReadFile(filepath.Join(worktreeDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events failed: %v", err)
	}
	events := string(eventsRaw)
	for _, want := range []string{
		`"event":"worktree.create.before"`,
		`"event":"worktree.create.after"`,
		`"event":"task.completed"`,
		`"event":"worktree.remove.after"`,
	} {
		if !strings.Contains(events, want) {
			t.Fatalf("expected event %s in %s", want, events)
		}
	}
}

func TestWorktreeManagerRebuildsIndexFromTasks(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("UI login polish", "update login screen"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}
	if _, err := manager.Create("ui-login", intPtr(0)); err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}

	indexPath := filepath.Join(worktreeDir, "index.json")
	if err := os.Remove(indexPath); err != nil {
		t.Fatalf("remove index failed: %v", err)
	}

	manager, err = NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("rebuild worktree manager failed: %v", err)
	}

	records, err := manager.List()
	if err != nil {
		t.Fatalf("list worktrees failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 rebuilt record, got %d", len(records))
	}
	record := records[0]
	if record.Name != "ui-login" || record.TaskID == nil || *record.TaskID != 0 {
		t.Fatalf("unexpected rebuilt record: %+v", record)
	}
	if record.Status != worktreeStatusActive {
		t.Fatalf("expected active rebuilt record, got %+v", record)
	}
}

func TestTeammateManagerClaimAssignsWorktreeDirectory(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")
	teamDir := filepath.Join(stateDir, "teams")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	worktreeManager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}

	base := &Agent{
		Name:            "lead",
		SystemPrompt:    "You are the lead agent.",
		Model:           "test-model",
		WorkDir:         repoDir,
		TaskManager:     taskManager,
		WorktreeManager: worktreeManager,
		tools:           map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	message, err := manager.nextIdleEvent(agent)
	if err != nil {
		t.Fatalf("nextIdleEvent failed: %v", err)
	}
	if !strings.Contains(message, `"owner": "worker-1"`) {
		t.Fatalf("expected claimed task payload, got %q", message)
	}
	if !strings.Contains(message, `"cwd":`) || !strings.Contains(message, `"worktree":`) {
		t.Fatalf("expected worktree metadata in payload, got %q", message)
	}
	if agent.WorkDir == repoDir {
		t.Fatalf("expected agent workdir to move to worktree, got %s", agent.WorkDir)
	}
	if _, err := os.Stat(filepath.Join(agent.WorkDir, "main.go")); err != nil {
		t.Fatalf("expected repo files in worktree dir: %v", err)
	}

	task := mustLoadTask(t, taskManager, 0)
	if task.Worktree == "" {
		t.Fatalf("expected task to record worktree binding, got %+v", task)
	}
}

func TestWorktreeManagerEnsureForTaskRecreatesMissingDirectory(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("recreate me", "task with missing worktree"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}
	record, err := manager.Create("recreate-me", intPtr(0))
	if err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}

	runTestCmd(t, repoDir, "git", "worktree", "remove", "--force", record.Path)
	if _, err := os.Stat(record.Path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removed, got stat err=%v", err)
	}

	task := mustLoadTask(t, taskManager, 0)
	recovered, err := manager.EnsureForTask(task)
	if err != nil {
		t.Fatalf("ensure for task failed: %v", err)
	}
	if recovered.Status != worktreeStatusActive {
		t.Fatalf("expected recovered worktree active, got %+v", recovered)
	}
	if _, err := os.Stat(recovered.Path); err != nil {
		t.Fatalf("expected recovered worktree path to exist: %v", err)
	}
}

func TestWorktreeManagerEnsureForTaskPrunesMissingButRegisteredWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("stale registration", "task with missing but registered worktree"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}
	record, err := manager.Create("stale-registration", intPtr(0))
	if err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}

	if err := os.RemoveAll(record.Path); err != nil {
		t.Fatalf("remove worktree path failed: %v", err)
	}
	if _, err := os.Stat(record.Path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removed, got stat err=%v", err)
	}

	task := mustLoadTask(t, taskManager, 0)
	recovered, err := manager.EnsureForTask(task)
	if err != nil {
		t.Fatalf("ensure for task failed: %v", err)
	}
	if recovered.Status != worktreeStatusActive {
		t.Fatalf("expected recovered worktree active, got %+v", recovered)
	}
	if _, err := os.Stat(recovered.Path); err != nil {
		t.Fatalf("expected recovered worktree path to exist: %v", err)
	}
}

func TestWorktreeManagerCreateReusesMissingErrorRecord(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("reuse stale record", "task bound later"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}

	index := worktreeIndex{
		Worktrees: []*Worktree{{
			Name:   "reuse-stale-record",
			Path:   filepath.Join(worktreeDir, "reuse-stale-record"),
			Branch: "wt/reuse-stale-record",
			Status: worktreeStatusError,
		}},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "index.json"), data, 0o644); err != nil {
		t.Fatalf("write index failed: %v", err)
	}

	record, err := manager.Create("reuse stale record", intPtr(0))
	if err != nil {
		t.Fatalf("expected stale error record to be reusable, got %v", err)
	}
	if record.Status != worktreeStatusActive {
		t.Fatalf("expected active worktree, got %+v", record)
	}
	if _, err := os.Stat(record.Path); err != nil {
		t.Fatalf("expected worktree path to exist: %v", err)
	}
}

func TestWorktreeManagerRebuildUnbindsRemovedWorktreeTasks(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("cleanup stale binding", "task with removed worktree"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	manager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}
	record, err := manager.Create("cleanup-stale-binding", intPtr(0))
	if err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}

	runTestCmd(t, repoDir, "git", "worktree", "remove", "--force", record.Path)
	indexPath := filepath.Join(worktreeDir, "index.json")
	index := worktreeIndex{
		Worktrees: []*Worktree{{
			Name:   record.Name,
			Path:   record.Path,
			Branch: record.Branch,
			Status: worktreeStatusRemoved,
			TaskID: intPtr(0),
		}},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index failed: %v", err)
	}
	if err := os.WriteFile(indexPath, data, 0o644); err != nil {
		t.Fatalf("write index failed: %v", err)
	}

	if _, err := NewWorktreeManager(repoDir, worktreeDir, taskManager); err != nil {
		t.Fatalf("rebuild worktree manager failed: %v", err)
	}

	task := mustLoadTask(t, taskManager, 0)
	if task.Worktree != "" {
		t.Fatalf("expected stale removed worktree binding to be cleared, got %+v", task)
	}
	if task.Status != taskStatusPending {
		t.Fatalf("expected task reset to pending after stale binding cleanup, got %+v", task)
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()

	repoDir := t.TempDir()
	runTestCmd(t, repoDir, "git", "init")
	runTestCmd(t, repoDir, "git", "config", "user.name", "Test User")
	runTestCmd(t, repoDir, "git", "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(".tasks/\n.worktrees/\n.teams/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore failed: %v", err)
	}

	runTestCmd(t, repoDir, "git", "add", ".")
	runTestCmd(t, repoDir, "git", "commit", "-m", "init")
	return repoDir
}

func runTestCmd(t *testing.T, dir, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, string(output))
	}
	return string(output)
}

func mustLoadTask(t *testing.T, tm *TaskManager, taskID int) *Task {
	t.Helper()

	raw, err := tm.Get(taskID)
	if err != nil {
		t.Fatalf("get task %d failed: %v", taskID, err)
	}
	var task Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("parse task failed: %v", err)
	}
	return &task
}
