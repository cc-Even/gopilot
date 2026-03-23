package agents

import (
	"os"
	"path/filepath"
)

var WORKDIR, _ = os.Getwd()

var TASK_DIR = filepath.Join(WORKDIR, ".tasks")

var SKILL_DIR = filepath.Join(WORKDIR, "skills")

var TEAM_DIR = filepath.Join(WORKDIR, ".teams")

var WORKTREE_DIR = filepath.Join(WORKDIR, ".worktrees")

var TALK_LOG_PATH = filepath.Join(WORKDIR, "talk.txt")

var TEAM_AGENTS_TOOLS = map[string]struct{}{
	"bash":            {},
	"read_file":       {},
	"write_file":      {},
	"edit_file":       {},
	"task_get":        {},
	"task_update":     {},
	"worktree_keep":   {},
	"worktree_remove": {},
	"send_message":    {},
	"read_inbox":      {},
	"list_team":       {},
}
