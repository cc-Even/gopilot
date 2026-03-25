package agents

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Task represents a single task with dependencies
type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	BlockedBy   []int  `json:"blockedBy"`
	Blocks      []int  `json:"blocks"`
	Owner       string `json:"owner"`
	Worktree    string `json:"worktree"`
}

// TaskManager handles CRUD operations and persistence of tasks with dependency graph
type TaskManager struct {
	dir    string
	nextID int
	mu     sync.RWMutex
}

type BackgroundTask struct {
	Status  string `json:"status"`
	Result  string `json:"result"`
	Command string `json:"command"`
}

type BackgroundNotification struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Command string `json:"command"`
	Result  string `json:"result"`
}

type BackgroundManager struct {
	mu                sync.RWMutex
	tasks             map[string]*BackgroundTask
	notificationQueue []BackgroundNotification
	dir               string
}

const (
	backgroundCommandPreviewLimit = 80
	backgroundResultLimit         = 50000
	backgroundNotificationLimit   = 500
	backgroundTimeout             = 300 * time.Second

	taskStatusPending    = "pending"
	taskStatusInProgress = "in_progress"
	taskStatusCompleted  = "completed"
)

var validTaskStatuses = map[string]struct{}{
	taskStatusPending:    {},
	taskStatusInProgress: {},
	taskStatusCompleted:  {},
}

func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{
		tasks: make(map[string]*BackgroundTask),
		dir:   WORKDIR,
	}
}

func (bm *BackgroundManager) SetDir(dir string) {
	if bm == nil {
		return
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if strings.TrimSpace(dir) == "" {
		bm.dir = WORKDIR
		return
	}
	bm.dir = dir
}

func (bm *BackgroundManager) workDir() string {
	if bm == nil {
		return WORKDIR
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	if strings.TrimSpace(bm.dir) == "" {
		return WORKDIR
	}
	return bm.dir
}

func (bm *BackgroundManager) Run(command string) string {
	if bm == nil {
		return "Error: background manager not initialized"
	}

	taskID := newBackgroundTaskID()

	bm.mu.Lock()
	bm.tasks[taskID] = &BackgroundTask{
		Status:  "running",
		Command: command,
	}
	bm.mu.Unlock()

	go bm.execute(taskID, command)

	return fmt.Sprintf("Background task %s started: %s", taskID, truncateForDisplay(command, backgroundCommandPreviewLimit))
}

func (bm *BackgroundManager) execute(taskID, command string) {
	status := "completed"
	output := "(no output)"

	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()

	rawOutput, err := runCommand(ctx, command, bm.workDir())
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
			output = fmt.Sprintf("Error: Timeout (%ds)", int(backgroundTimeout/time.Second))
		} else {
			status = "error"
			if len(rawOutput) > 0 {
				output = decodeCommandOutput(rawOutput)
			} else {
				output = formatCommandError(command, err, "")
			}
		}
	} else if len(rawOutput) > 0 {
		output = decodeCommandOutput(rawOutput)
	}

	output = truncateForDisplay(strings.TrimSpace(output), backgroundResultLimit)
	if output == "" {
		output = "(no output)"
	}

	bm.finishTask(taskID, command, status, output)
}

func (bm *BackgroundManager) finishTask(taskID, command, status, output string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	task := bm.tasks[taskID]
	if task == nil {
		task = &BackgroundTask{Command: command}
		bm.tasks[taskID] = task
	}
	task.Status = status
	task.Result = output

	bm.notificationQueue = append(bm.notificationQueue, BackgroundNotification{
		TaskID:  taskID,
		Status:  status,
		Command: truncateForDisplay(command, backgroundCommandPreviewLimit),
		Result:  truncateForDisplay(output, backgroundNotificationLimit),
	})
}

func (bm *BackgroundManager) Check(taskID string) string {
	if bm == nil {
		return "Error: background manager not initialized"
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	if taskID != "" {
		task, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Error: Unknown task %s", taskID)
		}

		result := task.Result
		if result == "" {
			result = "(running)"
		}
		return fmt.Sprintf("[%s] %s\n%s", task.Status, truncateForDisplay(task.Command, 60), result)
	}

	if len(bm.tasks) == 0 {
		return "No background tasks."
	}

	ids := make([]string, 0, len(bm.tasks))
	for id := range bm.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		task := bm.tasks[id]
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", id, task.Status, truncateForDisplay(task.Command, 60)))
	}

	return strings.Join(lines, "\n")
}

func (bm *BackgroundManager) PeekNotifications() []BackgroundNotification {
	if bm == nil {
		return nil
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	return append([]BackgroundNotification(nil), bm.notificationQueue...)
}

func (bm *BackgroundManager) AckNotifications(taskIDs []string) error {
	if bm == nil || len(taskIDs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		if strings.TrimSpace(taskID) == "" {
			continue
		}
		seen[taskID] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	kept := bm.notificationQueue[:0]
	for _, notification := range bm.notificationQueue {
		if _, ok := seen[notification.TaskID]; ok {
			continue
		}
		kept = append(kept, notification)
	}
	bm.notificationQueue = append([]BackgroundNotification(nil), kept...)
	return nil
}

func (bm *BackgroundManager) DrainNotifications() []BackgroundNotification {
	if bm == nil {
		return nil
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	notifications := append([]BackgroundNotification(nil), bm.notificationQueue...)
	bm.notificationQueue = nil
	return notifications
}

// NewTaskManager creates a new TaskManager with the specified directory
func NewTaskManager(tasksDir string) (*TaskManager, error) {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(tasksDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create tasks directory: %w", err)
	}

	tm := &TaskManager{
		dir:    tasksDir,
		nextID: 0, // Will be updated below
	}

	// Initialize nextID based on existing tasks
	maxID, err := tm.maxID()
	if err != nil {
		return nil, err
	}
	tm.nextID = maxID + 1

	return tm, nil
}

// maxID returns the maximum task ID currently in use
func (tm *TaskManager) maxID() (int, error) {
	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return -1, fmt.Errorf("failed to read tasks directory: %w", err)
	}

	maxID := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		// Extract ID from filename: task_123.json
		idStr := strings.TrimPrefix(name, "task_")
		idStr = strings.TrimSuffix(idStr, ".json")

		var id int
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			continue
		}

		if id > maxID {
			maxID = id
		}
	}

	return maxID, nil
}

// load reads a task from disk by ID
func (tm *TaskManager) load(taskID int) (*Task, error) {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", taskID))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("task %d not found", taskID)
		}
		return nil, fmt.Errorf("failed to read task file: %w", err)
	}

	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("failed to parse task: %w", err)
	}

	return &task, nil
}

// save writes a task to disk
func (tm *TaskManager) save(task *Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", task.ID))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write task file: %w", err)
	}

	return nil
}

// Create creates a new task with the given subject and description
func (tm *TaskManager) Create(subject, description string) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      taskStatusPending,
		BlockedBy:   []int{},
		Blocks:      []int{},
		Owner:       "",
		Worktree:    "",
	}

	if err := tm.save(task); err != nil {
		return "", err
	}

	tm.nextID++

	data, _ := json.MarshalIndent(task, "", "  ")
	return string(data), nil
}

// Get retrieves a task by ID
func (tm *TaskManager) Get(taskID int) (string, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	task, err := tm.load(taskID)
	if err != nil {
		return "", err
	}

	data, _ := json.MarshalIndent(task, "", "  ")
	return string(data), nil
}

// Update updates a task's status and/or dependencies
func (tm *TaskManager) Update(taskID int, status string, addBlockedBy, addBlocks []int) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return "", err
	}

	// Update status
	if status != "" {
		if !isValidTaskStatus(status) {
			return "", fmt.Errorf("invalid status: %s", status)
		}

		task.Status = status

		// When a task is completed, remove it from all other tasks' blockedBy
		if status == taskStatusCompleted {
			if err := tm.clearDependency(taskID); err != nil {
				return "", err
			}
		}
	}

	// Add blockedBy dependencies
	if len(addBlockedBy) > 0 {
		if err := tm.addBlockedByLocked(task, addBlockedBy); err != nil {
			return "", err
		}
	}

	// Add blocks dependencies (with bidirectional updates)
	if len(addBlocks) > 0 {
		if err := tm.addBlocksLocked(task, addBlocks); err != nil {
			return "", err
		}
	}

	if err := tm.save(task); err != nil {
		return "", err
	}

	data, _ := json.MarshalIndent(task, "", "  ")
	return string(data), nil
}

// ClaimNextAvailable atomically claims the first runnable unowned pending task.
func (tm *TaskManager) ClaimNextAvailable(owner string) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("owner is required")
	}

	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks directory: %w", err)
	}

	taskIDs := make([]int, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		taskIDs = append(taskIDs, extractTaskID(name))
	}

	sort.Ints(taskIDs)
	for _, taskID := range taskIDs {
		task, err := tm.load(taskID)
		if err != nil {
			continue
		}
		if !claimableTask(task) {
			continue
		}
		return tm.claimLocked(task, owner)
	}

	return nil, nil
}

func (tm *TaskManager) Claim(taskID int, owner string) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("owner is required")
	}

	task, err := tm.load(taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != taskStatusPending {
		return nil, fmt.Errorf("task %d is not pending", taskID)
	}
	if task.Owner != "" {
		return nil, fmt.Errorf("task %d is already owned by %s", taskID, task.Owner)
	}
	if len(task.BlockedBy) > 0 {
		return nil, fmt.Errorf("task %d is blocked by %v", taskID, task.BlockedBy)
	}

	return tm.claimLocked(task, owner)
}

func (tm *TaskManager) claimLocked(task *Task, owner string) (*Task, error) {
	if task == nil {
		return nil, nil
	}

	task.Owner = owner
	task.Status = taskStatusInProgress
	if err := tm.save(task); err != nil {
		return nil, err
	}
	return task, nil
}

func claimableTask(task *Task) bool {
	if task == nil {
		return false
	}
	return task.Status == taskStatusPending && task.Owner == "" && len(task.BlockedBy) == 0
}

func (tm *TaskManager) BindWorktree(taskID int, worktree string) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return nil, err
	}

	task.Worktree = worktree
	if task.Status == taskStatusPending {
		task.Status = taskStatusInProgress
	}

	if err := tm.save(task); err != nil {
		return nil, err
	}

	return task, nil
}

func (tm *TaskManager) UnbindWorktree(taskID int) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return nil, err
	}

	task.Worktree = ""
	if task.Status != taskStatusCompleted {
		task.Owner = ""
		task.Status = taskStatusPending
	}
	if err := tm.save(task); err != nil {
		return nil, err
	}

	return task, nil
}

func (tm *TaskManager) ResetClaim(taskID int) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return nil, err
	}

	task.Owner = ""
	if task.Status == taskStatusInProgress {
		task.Status = taskStatusPending
	}

	if err := tm.save(task); err != nil {
		return nil, err
	}

	return task, nil
}

func (tm *TaskManager) Snapshot() ([]*Task, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	return tm.snapshotLocked()
}

func (tm *TaskManager) snapshotLocked() ([]*Task, error) {
	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks directory: %w", err)
	}

	tasks := make([]*Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		task, err := tm.load(extractTaskID(name))
		if err != nil {
			continue
		}
		tasks = append(tasks, task)
	}

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

// clearDependency removes a completed task ID from all other tasks' blockedBy lists
func (tm *TaskManager) clearDependency(completedID int) error {
	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return fmt.Errorf("failed to read tasks directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "task_") || !strings.HasSuffix(name, ".json") {
			continue
		}

		task, err := tm.load(extractTaskID(name))
		if err != nil {
			continue
		}

		if containsInt(task.BlockedBy, completedID) {
			task.BlockedBy = removeInt(task.BlockedBy, completedID)
			if err := tm.save(task); err != nil {
				return err
			}
		}
	}

	return nil
}

// Delete removes a task by ID
func (tm *TaskManager) Delete(taskID int) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", taskID))
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("task %d not found", taskID)
		}
		return fmt.Errorf("failed to delete task: %w", err)
	}

	if err := tm.removeTaskReferencesLocked(taskID); err != nil {
		return err
	}

	return nil
}

// ListAll returns a formatted string of all tasks
func (tm *TaskManager) ListAll() (string, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	entries, err := os.ReadDir(tm.dir)
	if err != nil {
		return "", fmt.Errorf("failed to read tasks directory: %w", err)
	}

	if len(entries) == 0 {
		return "No tasks.", nil
	}

	tasks, err := tm.snapshotLocked()
	if err != nil {
		return "", err
	}

	var lines []string
	for _, task := range tasks {
		marker := statusMarker(task.Status)
		var blockedStr string
		if len(task.BlockedBy) > 0 {
			blockedStr = fmt.Sprintf(" (blocked by: %v)", task.BlockedBy)
		}
		line := fmt.Sprintf("%s #%d: %s%s", marker, task.ID, task.Subject, blockedStr)
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}

// Helper functions

func extractTaskID(filename string) int {
	idStr := strings.TrimPrefix(filename, "task_")
	idStr = strings.TrimSuffix(idStr, ".json")
	var id int
	fmt.Sscanf(idStr, "%d", &id)
	return id
}

func statusMarker(status string) string {
	switch status {
	case taskStatusPending:
		return "[ ]"
	case taskStatusInProgress:
		return "[>]"
	case taskStatusCompleted:
		return "[x]"
	default:
		return "[?]"
	}
}

func containsInt(slice []int, value int) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func removeInt(slice []int, value int) []int {
	var result []int
	for _, v := range slice {
		if v != value {
			result = append(result, v)
		}
	}
	return result
}

func uniqueIntSlice(slice []int) []int {
	seen := make(map[int]bool)
	var result []int
	for _, v := range slice {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	sort.Ints(result)
	return result
}

func isValidTaskStatus(status string) bool {
	_, ok := validTaskStatuses[status]
	return ok
}

func (tm *TaskManager) addBlockedByLocked(task *Task, blockerIDs []int) error {
	relations, err := tm.validateDependencyIDs(task.ID, blockerIDs)
	if err != nil {
		return err
	}

	task.BlockedBy = uniqueIntSlice(append(task.BlockedBy, blockerIDs...))
	for _, blocker := range relations {
		if containsInt(blocker.Blocks, task.ID) {
			continue
		}
		blocker.Blocks = uniqueIntSlice(append(blocker.Blocks, task.ID))
		if err := tm.save(blocker); err != nil {
			return err
		}
	}

	return nil
}

func (tm *TaskManager) addBlocksLocked(task *Task, blockedIDs []int) error {
	relations, err := tm.validateDependencyIDs(task.ID, blockedIDs)
	if err != nil {
		return err
	}

	task.Blocks = uniqueIntSlice(append(task.Blocks, blockedIDs...))
	for _, blocked := range relations {
		if containsInt(blocked.BlockedBy, task.ID) {
			continue
		}
		blocked.BlockedBy = uniqueIntSlice(append(blocked.BlockedBy, task.ID))
		if err := tm.save(blocked); err != nil {
			return err
		}
	}

	return nil
}

func (tm *TaskManager) validateDependencyIDs(taskID int, dependencyIDs []int) ([]*Task, error) {
	uniqueIDs := uniqueIntSlice(dependencyIDs)
	relations := make([]*Task, 0, len(uniqueIDs))

	for _, dependencyID := range uniqueIDs {
		if dependencyID == taskID {
			return nil, fmt.Errorf("task %d cannot depend on itself", taskID)
		}

		dependency, err := tm.load(dependencyID)
		if err != nil {
			return nil, err
		}
		relations = append(relations, dependency)
	}

	return relations, nil
}

func (tm *TaskManager) removeTaskReferencesLocked(taskID int) error {
	tasks, err := tm.snapshotLocked()
	if err != nil {
		return err
	}

	for _, task := range tasks {
		updated := false
		if containsInt(task.BlockedBy, taskID) {
			task.BlockedBy = removeInt(task.BlockedBy, taskID)
			updated = true
		}
		if containsInt(task.Blocks, taskID) {
			task.Blocks = removeInt(task.Blocks, taskID)
			updated = true
		}
		if !updated {
			continue
		}
		if err := tm.save(task); err != nil {
			return err
		}
	}

	return nil
}

func newBackgroundTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

func truncateForDisplay(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
