package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStateDirScopesBySession(t *testing.T) {
	repoStateDir := filepath.Join(t.TempDir(), "repo-state")

	first := sessionStateDir(repoStateDir, "session-a")
	second := sessionStateDir(repoStateDir, "session-b")

	if first == second {
		t.Fatalf("expected different session dirs, got %q", first)
	}
	if filepath.Dir(filepath.Dir(first)) != repoStateDir {
		t.Fatalf("expected session dir to live under repo state dir, got %q", first)
	}
}

func TestSessionStateDirSanitizesSessionID(t *testing.T) {
	repoStateDir := filepath.Join(t.TempDir(), "repo-state")

	got := sessionStateDir(repoStateDir, "Session 01/alpha")
	want := filepath.Join(repoStateDir, "sessions", "session-01-alpha")
	if got != want {
		t.Fatalf("unexpected session dir: got %q want %q", got, want)
	}
}

func TestSetWorkspaceDirUpdatesRuntimeState(t *testing.T) {
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}

	originalWorkdir := WORKDIR
	originalRepoRoot := REPO_ROOT
	originalRepoStateDir := REPO_STATE_DIR
	originalStateDir := STATE_DIR
	originalTaskDir := TASK_DIR
	originalTeamDir := TEAM_DIR
	originalWorktreeDir := WORKTREE_DIR
	originalTalkLogPath := TALK_LOG_PATH
	originalTokenLogPath := TOKEN_LOG_PATH
	root := t.TempDir()

	t.Cleanup(func() {
		WORKDIR = originalWorkdir
		REPO_ROOT = originalRepoRoot
		REPO_STATE_DIR = originalRepoStateDir
		STATE_DIR = originalStateDir
		TASK_DIR = originalTaskDir
		TEAM_DIR = originalTeamDir
		WORKTREE_DIR = originalWorktreeDir
		TALK_LOG_PATH = originalTalkLogPath
		TOKEN_LOG_PATH = originalTokenLogPath
		_ = os.Chdir(originalWD)
	})

	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatalf("mkdir child failed: %v", err)
	}

	resolvedRoot, err := SetWorkspaceDir(root)
	if err != nil {
		t.Fatalf("set workspace root failed: %v", err)
	}
	if resolvedRoot != root {
		t.Fatalf("unexpected resolved root: got %q want %q", resolvedRoot, root)
	}
	if WORKDIR != root {
		t.Fatalf("unexpected workdir: got %q want %q", WORKDIR, root)
	}
	if REPO_ROOT != root {
		t.Fatalf("unexpected repo root: got %q want %q", REPO_ROOT, root)
	}
	if STATE_DIR != sessionStateDir(filepath.Join(resolveStateBaseDir(), repoStateNamespace(root)), SESSION_ID) {
		t.Fatalf("unexpected state dir: %q", STATE_DIR)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd after root switch failed: %v", err)
	}
	if cwd != root {
		t.Fatalf("unexpected cwd after root switch: got %q want %q", cwd, root)
	}

	resolvedChild, err := SetWorkspaceDir("child")
	if err != nil {
		t.Fatalf("set relative workspace failed: %v", err)
	}
	if resolvedChild != child {
		t.Fatalf("unexpected resolved child: got %q want %q", resolvedChild, child)
	}
	if WORKDIR != child {
		t.Fatalf("unexpected child workdir: got %q want %q", WORKDIR, child)
	}
	if REPO_ROOT != child {
		t.Fatalf("unexpected child repo root: got %q want %q", REPO_ROOT, child)
	}
	if TASK_DIR != filepath.Join(STATE_DIR, "tasks") {
		t.Fatalf("unexpected task dir: %q", TASK_DIR)
	}
	if TEAM_DIR != filepath.Join(STATE_DIR, "teams") {
		t.Fatalf("unexpected team dir: %q", TEAM_DIR)
	}
	if WORKTREE_DIR != filepath.Join(STATE_DIR, "worktrees") {
		t.Fatalf("unexpected worktree dir: %q", WORKTREE_DIR)
	}
	if TALK_LOG_PATH != filepath.Join(STATE_DIR, "talk.txt") {
		t.Fatalf("unexpected talk log path: %q", TALK_LOG_PATH)
	}
	if TOKEN_LOG_PATH != filepath.Join(TOOLDIR, "token.log") {
		t.Fatalf("unexpected token log path: %q", TOKEN_LOG_PATH)
	}
}
