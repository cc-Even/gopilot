package agents

import (
	"os"
	"path/filepath"
)

var WORKDIR, _ = os.Getwd()

var TASK_DIR = filepath.Join(WORKDIR, ".tasks")

var SKILL_DIR = filepath.Join(WORKDIR, "skills")

var TEAM_DIR = filepath.Join(WORKDIR, ".teams")

var TALK_LOG_PATH = filepath.Join(WORKDIR, "talk.txt")

var TEAM_AGENTS_TOOLS = map[string]struct{}{
	"bash":         {},
	"read_file":    {},
	"write_file":   {},
	"edit_file":    {},
	"send_message": {},
	"read_inbox":   {},
}
