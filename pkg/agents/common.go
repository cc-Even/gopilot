package agents

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var WORKDIR, _ = os.Getwd()

var exePath, _ = os.Executable()
var TOOLDIR = filepath.Dir(exePath)

var REPO_ROOT = detectRepoRoot(WORKDIR)

var SESSION_ID = newSessionID()

var REPO_STATE_DIR = filepath.Join(resolveStateBaseDir(), repoStateNamespace(REPO_ROOT))

var STATE_DIR = sessionStateDir(REPO_STATE_DIR, SESSION_ID)

var TASK_DIR = filepath.Join(STATE_DIR, "tasks")

var SKILL_DIR = filepath.Join(TOOLDIR, "skills")

var SUBAGENT_DIR = filepath.Join(TOOLDIR, "subagents")

var TEAM_DIR = filepath.Join(STATE_DIR, "teams")

var WORKTREE_DIR = filepath.Join(STATE_DIR, "worktrees")

var TALK_LOG_PATH = filepath.Join(STATE_DIR, "talk.txt")

var TOKEN_LOG_PATH = filepath.Join(TOOLDIR, "token.log")

var TRANSCRIPT_DIR = filepath.Join(TOOLDIR, "transcripts")

func SetWorkspaceDir(dir string) (string, error) {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		return "", errors.New("workspace path is empty")
	}

	if !filepath.IsAbs(clean) {
		base := WORKDIR
		if strings.TrimSpace(base) == "" {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
		clean = filepath.Join(base, clean)
	}

	resolved, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", resolved)
	}
	if err := os.Chdir(resolved); err != nil {
		return "", err
	}

	WORKDIR = resolved
	REPO_ROOT = detectRepoRoot(resolved)
	REPO_STATE_DIR = filepath.Join(resolveStateBaseDir(), repoStateNamespace(REPO_ROOT))
	STATE_DIR = sessionStateDir(REPO_STATE_DIR, SESSION_ID)
	TASK_DIR = filepath.Join(STATE_DIR, "tasks")
	TEAM_DIR = filepath.Join(STATE_DIR, "teams")
	WORKTREE_DIR = filepath.Join(STATE_DIR, "worktrees")
	TALK_LOG_PATH = filepath.Join(STATE_DIR, "talk.txt")

	return resolved, nil
}

func detectRepoRoot(dir string) string {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		return ""
	}
	if root, err := gitRepoRoot(clean); err == nil && strings.TrimSpace(root) != "" {
		return root
	}
	if abs, err := filepath.Abs(clean); err == nil {
		return abs
	}
	return clean
}

func resolveStateBaseDir() string {
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return filepath.Join(root, "gopilot")
	}

	if runtime.GOOS == "windows" {
		if root := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); root != "" {
			return filepath.Join(root, "gopilot")
		}
		if root, err := os.UserConfigDir(); err == nil && strings.TrimSpace(root) != "" {
			return filepath.Join(root, "gopilot")
		}
	}

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "state", "gopilot")
	}

	return filepath.Join(TOOLDIR, ".gopilot-state")
}

func repoStateNamespace(repoRoot string) string {
	clean := strings.TrimSpace(repoRoot)
	if clean == "" {
		return "workspace-unknown"
	}

	base := sanitizeStateName(filepath.Base(clean))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "workspace"
	}

	sum := sha1.Sum([]byte(clean))
	return base + "-" + hex.EncodeToString(sum[:4])
}

func sanitizeStateName(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(name))
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}

	return strings.Trim(b.String(), "-")
}

func sessionStateDir(repoStateDir, sessionID string) string {
	cleanID := sanitizeStateName(sessionID)
	if cleanID == "" {
		cleanID = "default"
	}
	return filepath.Join(repoStateDir, "sessions", cleanID)
}

func newSessionID() string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err == nil {
		return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(suffix[:]))
	}

	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}

var TEAM_AGENTS_TOOLS = map[string]struct{}{
	"bash":                     {},
	"list_file":                {},
	"repo_map":                 {},
	"read_file":                {},
	"read_files":               {},
	"write_file":               {},
	"edit_file":                {},
	"task_list":                {},
	"task_get":                 {},
	"task_update":              {},
	"claim_task":               {},
	"complete_task_and_report": {},
	"worktree_keep":            {},
	"worktree_remove":          {},
	"send_message":             {},
	"read_inbox":               {},
	"list_team":                {},
}
