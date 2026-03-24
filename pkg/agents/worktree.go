package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	worktreeStatusCreating = "creating"
	worktreeStatusActive   = "active"
	worktreeStatusRemoving = "removing"
	worktreeStatusRemoved  = "removed"
	worktreeStatusKept     = "kept"
	worktreeStatusError    = "error"
)

type Worktree struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Status string `json:"status"`
	TaskID *int   `json:"task_id,omitempty"`
}

type worktreeIndex struct {
	Worktrees []*Worktree `json:"worktrees"`
}

type worktreeEvent struct {
	Event    string              `json:"event"`
	Task     *worktreeEventTask  `json:"task,omitempty"`
	Worktree *worktreeEventState `json:"worktree,omitempty"`
	Error    string              `json:"error,omitempty"`
	TS       int64               `json:"ts"`
}

type worktreeEventTask struct {
	ID       int    `json:"id"`
	Status   string `json:"status,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

type worktreeEventState struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

type WorktreeManager struct {
	rootDir    string
	repoRoot   string
	indexPath  string
	eventsPath string
	tasks      *TaskManager
	mu         sync.Mutex
}

func NewWorktreeManager(repoRoot, rootDir string, tasks *TaskManager) (*WorktreeManager, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("worktree root is required")
	}

	repoRoot, err := gitRepoRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create worktree root: %w", err)
	}

	wm := &WorktreeManager{
		rootDir:    rootDir,
		repoRoot:   repoRoot,
		indexPath:  filepath.Join(rootDir, "index.json"),
		eventsPath: filepath.Join(rootDir, "events.jsonl"),
		tasks:      tasks,
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	if err := wm.rebuildIndexLocked(); err != nil {
		return nil, err
	}

	return wm, nil
}

func (wm *WorktreeManager) Create(name string, taskID *int) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	return wm.createLocked(name, taskID)
}

func (wm *WorktreeManager) createLocked(name string, taskID *int) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}

	normalized := normalizeWorktreeName(name)
	if normalized == "" {
		return nil, fmt.Errorf("worktree name is required")
	}

	index, err := wm.loadIndexLocked()
	if err != nil {
		return nil, err
	}
	if existing := index.find(normalized); existing != nil && existing.Status != worktreeStatusRemoved {
		if reusable, changed := wm.reusableMissingWorktreeLocked(existing, taskID); reusable != nil {
			if changed {
				index.upsert(reusable)
				if err := wm.saveIndexLocked(index); err != nil {
					return nil, err
				}
			}
			return wm.createOrRecreateLocked(index, reusable, taskID)
		}
		return nil, fmt.Errorf("worktree %q already exists", normalized)
	}

	branch := fmt.Sprintf("wt/%s", normalized)
	path := filepath.Join(wm.rootDir, normalized)
	before := &Worktree{
		Name:   normalized,
		Path:   path,
		Branch: branch,
		Status: worktreeStatusCreating,
		TaskID: taskID,
	}
	return wm.createOrRecreateLocked(index, before, taskID)
}

func (wm *WorktreeManager) createOrRecreateLocked(index *worktreeIndex, before *Worktree, taskID *int) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}
	if before == nil {
		return nil, fmt.Errorf("worktree record is required")
	}
	normalized := before.Name
	path := before.Path
	branch := before.Branch

	var err error
	var task *Task
	if taskID != nil && wm.tasks != nil {
		task, err = wm.tasks.load(*taskID)
		if err != nil {
			return nil, err
		}
	}

	_ = wm.appendEventLocked(worktreeEvent{
		Event:    "worktree.create.before",
		Task:     eventTask(task),
		Worktree: eventWorktree(before),
		TS:       time.Now().Unix(),
	})
	index.upsert(cloneWorktree(before))
	if err := wm.saveIndexLocked(index); err != nil {
		return nil, err
	}

	if err := wm.addWorktreeLocked(branch, path); err != nil {
		before.Status = worktreeStatusError
		index.upsert(cloneWorktree(before))
		_ = wm.saveIndexLocked(index)
		_ = wm.appendEventLocked(worktreeEvent{
			Event:    "worktree.create.failed",
			Task:     eventTask(task),
			Worktree: eventWorktree(before),
			Error:    err.Error(),
			TS:       time.Now().Unix(),
		})
		return nil, err
	}

	if taskID != nil && wm.tasks != nil {
		task, err = wm.tasks.BindWorktree(*taskID, normalized)
		if err != nil {
			_ = wm.runGitLocked("worktree", "remove", "--force", path)
			before.Status = worktreeStatusError
			index.upsert(cloneWorktree(before))
			_ = wm.saveIndexLocked(index)
			_ = wm.appendEventLocked(worktreeEvent{
				Event:    "worktree.create.failed",
				Task:     &worktreeEventTask{ID: *taskID},
				Worktree: eventWorktree(before),
				Error:    err.Error(),
				TS:       time.Now().Unix(),
			})
			return nil, err
		}
	}

	record := &Worktree{
		Name:   normalized,
		Path:   path,
		Branch: branch,
		Status: worktreeStatusActive,
		TaskID: taskID,
	}
	index.upsert(record)
	if err := wm.saveIndexLocked(index); err != nil {
		return nil, err
	}

	if err := wm.appendEventLocked(worktreeEvent{
		Event:    "worktree.create.after",
		Task:     eventTask(task),
		Worktree: eventWorktree(record),
		TS:       time.Now().Unix(),
	}); err != nil {
		return nil, err
	}

	return cloneWorktree(record), nil
}

func (wm *WorktreeManager) EnsureForTask(task *Task) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	index, err := wm.loadIndexLocked()
	if err != nil {
		return nil, err
	}

	if task.Worktree != "" {
		name := normalizeWorktreeName(task.Worktree)
		if existing := index.find(name); existing != nil && existing.Status != worktreeStatusRemoved {
			record, changed, err := wm.reconcileTaskWorktreeLocked(existing, task)
			if err != nil {
				return nil, err
			}
			if changed {
				index.upsert(record)
				if err := wm.saveIndexLocked(index); err != nil {
					return nil, err
				}
			}
			return cloneWorktree(record), nil
		}

		record := inferWorktreeRecord(wm.rootDir, name, task.ID)
		record.Status = worktreeStatusError
		record, _, err = wm.reconcileTaskWorktreeLocked(record, task)
		if err != nil {
			return nil, err
		}
		index.upsert(record)
		if err := wm.saveIndexLocked(index); err != nil {
			return nil, err
		}
		return cloneWorktree(record), nil
	}

	name := defaultWorktreeName(task.ID, task.Subject)
	record, err := wm.createLocked(name, intPtr(task.ID))
	if err != nil {
		return nil, err
	}
	return cloneWorktree(record), nil
}

func (wm *WorktreeManager) Keep(name string) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	index, err := wm.loadIndexLocked()
	if err != nil {
		return nil, err
	}

	record := index.find(normalizeWorktreeName(name))
	if record == nil {
		return nil, fmt.Errorf("worktree %q not found", name)
	}
	record.Status = worktreeStatusKept

	if err := wm.saveIndexLocked(index); err != nil {
		return nil, err
	}
	if err := wm.appendEventLocked(worktreeEvent{
		Event:    "worktree.keep",
		Task:     eventTaskByID(record.TaskID, wm.tasks),
		Worktree: eventWorktree(record),
		TS:       time.Now().Unix(),
	}); err != nil {
		return nil, err
	}

	return cloneWorktree(record), nil
}

func (wm *WorktreeManager) Remove(name string, force bool, completeTask bool) (*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	index, err := wm.loadIndexLocked()
	if err != nil {
		return nil, err
	}

	record := index.find(normalizeWorktreeName(name))
	if record == nil {
		return nil, fmt.Errorf("worktree %q not found", name)
	}

	task := taskByID(record.TaskID, wm.tasks)
	if err := wm.appendEventLocked(worktreeEvent{
		Event:    "worktree.remove.before",
		Task:     eventTask(task),
		Worktree: eventWorktree(record),
		TS:       time.Now().Unix(),
	}); err != nil {
		return nil, err
	}
	record.Status = worktreeStatusRemoving
	index.upsert(record)
	if err := wm.saveIndexLocked(index); err != nil {
		return nil, err
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, record.Path)
	if err := wm.runGitLocked(args...); err != nil {
		record.Status = worktreeStatusError
		index.upsert(record)
		_ = wm.saveIndexLocked(index)
		_ = wm.appendEventLocked(worktreeEvent{
			Event:    "worktree.remove.failed",
			Task:     eventTask(task),
			Worktree: eventWorktree(record),
			Error:    err.Error(),
			TS:       time.Now().Unix(),
		})
		return nil, err
	}

	if record.TaskID != nil && wm.tasks != nil {
		if completeTask {
			taskJSON, updateErr := wm.tasks.Update(*record.TaskID, "completed", nil, nil)
			if updateErr != nil {
				record.Status = worktreeStatusError
				index.upsert(record)
				_ = wm.saveIndexLocked(index)
				return nil, updateErr
			}
			var completedTask Task
			if err := json.Unmarshal([]byte(taskJSON), &completedTask); err == nil {
				task = &completedTask
			}
			if err := wm.appendEventLocked(worktreeEvent{
				Event:    "task.completed",
				Task:     eventTask(task),
				Worktree: eventWorktree(record),
				TS:       time.Now().Unix(),
			}); err != nil {
				return nil, err
			}
		}

		task, err = wm.tasks.UnbindWorktree(*record.TaskID)
		if err != nil {
			record.Status = worktreeStatusError
			index.upsert(record)
			_ = wm.saveIndexLocked(index)
			return nil, err
		}
	}

	record.Status = worktreeStatusRemoved
	index.upsert(record)
	if err := wm.saveIndexLocked(index); err != nil {
		return nil, err
	}

	if err := wm.appendEventLocked(worktreeEvent{
		Event:    "worktree.remove.after",
		Task:     eventTask(task),
		Worktree: eventWorktree(record),
		TS:       time.Now().Unix(),
	}); err != nil {
		return nil, err
	}

	return cloneWorktree(record), nil
}

func (wm *WorktreeManager) List() ([]*Worktree, error) {
	if wm == nil {
		return nil, fmt.Errorf("worktree manager not initialized")
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	index, err := wm.loadIndexLocked()
	if err != nil {
		return nil, err
	}

	out := make([]*Worktree, 0, len(index.Worktrees))
	for _, record := range index.Worktrees {
		out = append(out, cloneWorktree(record))
	}
	return out, nil
}

func (wm *WorktreeManager) rebuildIndexLocked() error {
	index, err := wm.loadIndexLocked()
	if err != nil {
		return err
	}

	if wm.tasks == nil {
		return wm.saveIndexLocked(index)
	}

	tasks, err := wm.tasks.Snapshot()
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if strings.TrimSpace(task.Worktree) == "" {
			continue
		}

		name := normalizeWorktreeName(task.Worktree)
		record := index.find(name)
		if record == nil {
			record = inferWorktreeRecord(wm.rootDir, name, task.ID)
			record.Status = worktreeStatusError
		} else {
			record.Name = name
			if record.Path == "" {
				record.Path = filepath.Join(wm.rootDir, name)
			}
			if record.Branch == "" {
				record.Branch = inferBranch(record.Path, name)
			}
			if record.Status == "" {
				record.Status = worktreeStatusError
			}
			record.TaskID = intPtr(task.ID)
		}
		if record.Status == worktreeStatusRemoved && !pathExists(record.Path) {
			if _, err := wm.tasks.UnbindWorktree(task.ID); err != nil {
				return err
			}
			continue
		}
		reconciled, _, err := wm.reconcileTaskWorktreeLocked(record, task)
		if err != nil {
			return err
		}
		record = reconciled
		index.upsert(record)
	}

	return wm.saveIndexLocked(index)
}

func (wm *WorktreeManager) addWorktreeLocked(branch, path string) error {
	args := []string{"worktree", "add"}
	if wm.branchExistsLocked(branch) {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, "HEAD")
	}
	if err := wm.runGitLocked(args...); err != nil {
		if wm.isMissingButRegisteredWorktreeErr(err) {
			if pruneErr := wm.runGitLocked("worktree", "prune"); pruneErr != nil {
				return err
			}
			if retryErr := wm.runGitLocked(args...); retryErr == nil {
				return nil
			} else {
				err = retryErr
			}
		}
		return err
	}
	return nil
}

func (wm *WorktreeManager) branchExistsLocked(branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = wm.repoRoot
	return cmd.Run() == nil
}

func (wm *WorktreeManager) runGitLocked(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = wm.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("git %s failed: %s", strings.Join(args, " "), trimmed)
	}
	return nil
}

func (wm *WorktreeManager) loadIndexLocked() (*worktreeIndex, error) {
	data, err := os.ReadFile(wm.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &worktreeIndex{Worktrees: []*Worktree{}}, nil
		}
		return nil, fmt.Errorf("failed to read worktree index: %w", err)
	}

	var index worktreeIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse worktree index: %w", err)
	}
	if index.Worktrees == nil {
		index.Worktrees = []*Worktree{}
	}
	return &index, nil
}

func (wm *WorktreeManager) saveIndexLocked(index *worktreeIndex) error {
	if index == nil {
		index = &worktreeIndex{Worktrees: []*Worktree{}}
	}

	sort.Slice(index.Worktrees, func(i, j int) bool {
		return index.Worktrees[i].Name < index.Worktrees[j].Name
	})

	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal worktree index: %w", err)
	}
	if err := os.WriteFile(wm.indexPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write worktree index: %w", err)
	}
	return nil
}

func (wm *WorktreeManager) appendEventLocked(event worktreeEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal worktree event: %w", err)
	}

	file, err := os.OpenFile(wm.eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open worktree event log: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to append worktree event: %w", err)
	}
	return nil
}

func (idx *worktreeIndex) find(name string) *Worktree {
	if idx == nil {
		return nil
	}
	for _, record := range idx.Worktrees {
		if record.Name == name {
			return record
		}
	}
	return nil
}

func (idx *worktreeIndex) upsert(record *Worktree) {
	if idx == nil || record == nil {
		return
	}
	for i, existing := range idx.Worktrees {
		if existing.Name == record.Name {
			idx.Worktrees[i] = cloneWorktree(record)
			return
		}
	}
	idx.Worktrees = append(idx.Worktrees, cloneWorktree(record))
}

func cloneWorktree(record *Worktree) *Worktree {
	if record == nil {
		return nil
	}
	clone := *record
	if record.TaskID != nil {
		clone.TaskID = intPtr(*record.TaskID)
	}
	return &clone
}

func inferWorktreeRecord(rootDir, name string, taskID int) *Worktree {
	path := filepath.Join(rootDir, name)
	return &Worktree{
		Name:   name,
		Path:   path,
		Branch: inferBranch(path, name),
		Status: worktreeStatusActive,
		TaskID: intPtr(taskID),
	}
}

func inferBranch(path, name string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = path
	output, err := cmd.CombinedOutput()
	if err == nil {
		branch := strings.TrimSpace(string(output))
		if branch != "" {
			return branch
		}
	}
	return fmt.Sprintf("wt/%s", name)
}

func eventTask(task *Task) *worktreeEventTask {
	if task == nil {
		return nil
	}
	return &worktreeEventTask{
		ID:       task.ID,
		Status:   task.Status,
		Worktree: task.Worktree,
	}
}

func eventTaskByID(taskID *int, tasks *TaskManager) *worktreeEventTask {
	return eventTask(taskByID(taskID, tasks))
}

func taskByID(taskID *int, tasks *TaskManager) *Task {
	if taskID == nil || tasks == nil {
		return nil
	}
	task, err := tasks.load(*taskID)
	if err != nil {
		return nil
	}
	return task
}

func eventWorktree(record *Worktree) *worktreeEventState {
	if record == nil {
		return nil
	}
	return &worktreeEventState{
		Name:   record.Name,
		Status: record.Status,
		Path:   record.Path,
		Branch: record.Branch,
	}
}

func defaultWorktreeName(taskID int, subject string) string {
	base := normalizeWorktreeName(subject)
	if base == "" {
		return fmt.Sprintf("task-%d", taskID)
	}
	const maxBaseLen = 40
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
		base = strings.Trim(base, "-")
	}
	return fmt.Sprintf("task-%d-%s", taskID, base)
}

func normalizeWorktreeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}

	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}

	normalized := strings.Trim(b.String(), "-")
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}
	return normalized
}

func gitRepoRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to detect git repo root: %s", strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func intPtr(v int) *int {
	return &v
}

func (wm *WorktreeManager) reconcileTaskWorktreeLocked(record *Worktree, task *Task) (*Worktree, bool, error) {
	if record == nil || task == nil {
		return record, false, nil
	}

	changed := false
	record = cloneWorktree(record)
	record.TaskID = intPtr(task.ID)
	if record.Path == "" {
		record.Path = filepath.Join(wm.rootDir, record.Name)
		changed = true
	}
	if record.Branch == "" {
		record.Branch = inferBranch(record.Path, record.Name)
		changed = true
	}

	if pathExists(record.Path) {
		if record.Status != worktreeStatusActive && record.Status != worktreeStatusKept {
			record.Status = worktreeStatusActive
			changed = true
		}
		return record, changed, nil
	}

	record.Status = worktreeStatusCreating
	changed = true
	if err := wm.addWorktreeLocked(record.Branch, record.Path); err != nil {
		record.Status = worktreeStatusError
		return record, true, err
	}
	record.Status = worktreeStatusActive
	return record, true, nil
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (wm *WorktreeManager) reusableMissingWorktreeLocked(existing *Worktree, taskID *int) (*Worktree, bool) {
	if existing == nil || pathExists(existing.Path) {
		return nil, false
	}
	record := cloneWorktree(existing)
	changed := false
	if record.Path == "" {
		record.Path = filepath.Join(wm.rootDir, record.Name)
		changed = true
	}
	if record.Branch == "" {
		record.Branch = fmt.Sprintf("wt/%s", record.Name)
		changed = true
	}
	if taskID != nil {
		if record.TaskID == nil || *record.TaskID != *taskID {
			record.TaskID = intPtr(*taskID)
			changed = true
		}
	}
	if record.Status != worktreeStatusError && record.Status != worktreeStatusCreating {
		record.Status = worktreeStatusError
		changed = true
	}
	return record, changed
}

func (wm *WorktreeManager) isMissingButRegisteredWorktreeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "missing but already registered worktree")
}
