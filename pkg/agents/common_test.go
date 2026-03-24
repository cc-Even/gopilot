package agents

import (
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
