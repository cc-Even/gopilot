package agents

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSplitCommandLineWindowsKeepsBackslashPaths(t *testing.T) {
	args, err := splitCommandLine(`go build -o C:\build\app.exe`, "windows")
	if err != nil {
		t.Fatalf("split command failed: %v", err)
	}

	want := []string{"go", "build", "-o", `C:\build\app.exe`}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestResolveCommandInvocationWindowsPathStaysDirectExec(t *testing.T) {
	invocation, err := resolveCommandInvocation(`go build -o C:\build\app.exe`, "windows", t.TempDir())
	if err != nil {
		t.Fatalf("resolve command failed: %v", err)
	}
	if invocation.viaShell {
		t.Fatalf("expected direct exec, got shell invocation: %+v", invocation)
	}

	wantArgs := []string{"build", "-o", `C:\build\app.exe`}
	if invocation.name != "go" {
		t.Fatalf("name = %q, want go", invocation.name)
	}
	if strings.Join(invocation.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("args = %#v, want %#v", invocation.args, wantArgs)
	}
}

func TestSplitCommandLineSingleQuoteBehaviorDiffersByPlatform(t *testing.T) {
	t.Run("windows treats single quotes literally", func(t *testing.T) {
		args, err := splitCommandLine(`tool 'two words'`, "windows")
		if err != nil {
			t.Fatalf("split command failed: %v", err)
		}

		want := []string{"tool", "'two", "words'"}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	})

	t.Run("unix treats single quotes as grouping", func(t *testing.T) {
		args, err := splitCommandLine(`tool 'two words'`, "linux")
		if err != nil {
			t.Fatalf("split command failed: %v", err)
		}

		want := []string{"tool", "two words"}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	})

	t.Run("windows allows unmatched single quote as literal", func(t *testing.T) {
		args, err := splitCommandLine(`tool 'unterminated`, "windows")
		if err != nil {
			t.Fatalf("split command failed: %v", err)
		}

		want := []string{"tool", "'unterminated"}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	})

	t.Run("unix rejects unmatched single quote", func(t *testing.T) {
		if _, err := splitCommandLine(`tool 'unterminated`, "linux"); err == nil {
			t.Fatal("expected unterminated quote error")
		}
	})
}

func TestSplitCommandLinePreservesEmptyQuotedArgs(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		command string
		want    []string
	}{
		{
			name:    "windows double quotes",
			goos:    "windows",
			command: `git commit -m ""`,
			want:    []string{"git", "commit", "-m", ""},
		},
		{
			name:    "unix double quotes",
			goos:    "linux",
			command: `git commit -m ""`,
			want:    []string{"git", "commit", "-m", ""},
		},
		{
			name:    "unix single quotes",
			goos:    "linux",
			command: `printf ''`,
			want:    []string{"printf", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := splitCommandLine(tt.command, tt.goos)
			if err != nil {
				t.Fatalf("split command failed: %v", err)
			}
			if strings.Join(args, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("args = %#v, want %#v", args, tt.want)
			}
		})
	}
}

func TestResolveCommandInvocationDirectExec(t *testing.T) {
	invocation, err := resolveCommandInvocation(`go env "GOOS"`, runtime.GOOS, t.TempDir())
	if err != nil {
		t.Fatalf("resolve command failed: %v", err)
	}
	if invocation.viaShell {
		t.Fatalf("expected direct exec, got shell invocation: %+v", invocation)
	}
	if invocation.name != "go" {
		t.Fatalf("expected go executable, got %q", invocation.name)
	}
	if len(invocation.args) != 2 || invocation.args[0] != "env" || invocation.args[1] != "GOOS" {
		t.Fatalf("unexpected args: %#v", invocation.args)
	}
}

func TestResolveCommandInvocationFallsBackToPlatformShell(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		command  string
		wantName string
		wantArgs []string
	}{
		{
			name:     "windows shell features",
			goos:     "windows",
			command:  "echo first && echo second",
			wantName: "cmd",
			wantArgs: []string{"/d", "/s", "/c", "echo first && echo second"},
		},
		{
			name:     "unix shell features",
			goos:     "linux",
			command:  "echo first && echo second",
			wantName: "sh",
			wantArgs: []string{"-lc", "echo first && echo second"},
		},
		{
			name:     "windows builtin command",
			goos:     "windows",
			command:  "dir",
			wantName: "cmd",
			wantArgs: []string{"/d", "/s", "/c", "dir"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invocation, err := resolveCommandInvocation(tt.command, tt.goos, t.TempDir())
			if err != nil {
				t.Fatalf("resolve command failed: %v", err)
			}
			if !invocation.viaShell {
				t.Fatalf("expected shell invocation for %q", tt.command)
			}
			if invocation.name != tt.wantName {
				t.Fatalf("name = %q, want %q", invocation.name, tt.wantName)
			}
			if strings.Join(invocation.args, "\x00") != strings.Join(tt.wantArgs, "\x00") {
				t.Fatalf("args = %#v, want %#v", invocation.args, tt.wantArgs)
			}
		})
	}
}

func TestResolveCommandInvocationRewritesPipToWorkspacePython(t *testing.T) {
	root := t.TempDir()
	pythonPath := writeCommandExecFile(t, root, filepath.Join(".venv", platformBinDir(), platformPythonExecutable(runtime.GOOS)), "")

	invocation, err := resolveCommandInvocation("pip install demo", runtime.GOOS, root)
	if err != nil {
		t.Fatalf("resolve command failed: %v", err)
	}
	if invocation.name != pythonPath {
		t.Fatalf("name = %q, want %q", invocation.name, pythonPath)
	}
	wantArgs := []string{"-m", "pip", "install", "demo"}
	if strings.Join(invocation.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("args = %#v, want %#v", invocation.args, wantArgs)
	}
}

func TestResolveCommandInvocationRewritesPipToPreferredPythonWithoutVirtualenv(t *testing.T) {
	restore := stubCommandExecLookPath(t)
	defer restore()

	commandExecLookPath = func(file string) (string, error) {
		switch file {
		case "python":
			return "/usr/bin/python", nil
		case "python3":
			return "/usr/bin/python3", nil
		default:
			return "", os.ErrNotExist
		}
	}

	invocation, err := resolveCommandInvocation("pip install demo", "linux", t.TempDir())
	if err != nil {
		t.Fatalf("resolve command failed: %v", err)
	}
	if invocation.name != "/usr/bin/python" {
		t.Fatalf("name = %q, want %q", invocation.name, "/usr/bin/python")
	}
	wantArgs := []string{"-m", "pip", "install", "demo"}
	if strings.Join(invocation.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("args = %#v, want %#v", invocation.args, wantArgs)
	}
}

func TestBuildCommandInjectsWorkspacePythonPath(t *testing.T) {
	root := t.TempDir()

	cmd, err := buildCommand("pytest tests", root)
	if err != nil {
		t.Fatalf("build command failed: %v", err)
	}

	got := envValue(cmd.Env, "PYTHONPATH")
	if got == "" {
		t.Fatal("expected PYTHONPATH to be set")
	}
	parts := strings.Split(got, string(os.PathListSeparator))
	if len(parts) == 0 || parts[0] != root {
		t.Fatalf("PYTHONPATH = %q, want first entry %q", got, root)
	}
}

func TestRunBashSupportsDirectExecAndShellFallback(t *testing.T) {
	t.Run("direct exec", func(t *testing.T) {
		output := strings.TrimSpace(RunBash("go env GOOS", t.TempDir()))
		if output != runtime.GOOS {
			t.Fatalf("output = %q, want %q", output, runtime.GOOS)
		}
	})

	t.Run("shell fallback", func(t *testing.T) {
		output := strings.TrimSpace(RunBash("echo first && echo second", t.TempDir()))
		if !strings.Contains(output, "first") || !strings.Contains(output, "second") {
			t.Fatalf("unexpected shell fallback output: %q", output)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("cancellation timing is validated by windows cross-compilation in this environment")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		output := RunBashContext(ctx, "sleep 10", t.TempDir())
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("expected cancellation to return quickly, took %s", elapsed)
		}
		if !strings.Contains(output, "interrupted") {
			t.Fatalf("expected interrupted output, got %q", output)
		}
	})

	t.Run("no match search result", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("grep-based assertion is for unix-like environments")
		}

		output := strings.TrimSpace(RunBash("grep definitely_missing_pattern /dev/null", t.TempDir()))
		if output != "(no output)" {
			t.Fatalf("expected no output marker, got %q", output)
		}
	})

	t.Run("shell fallback prefers workspace virtualenv", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("shell script assertion is for unix-like environments")
		}

		root := t.TempDir()
		writeCommandExecFile(t, root, filepath.Join(".venv", "bin", "python"), "#!/bin/sh\necho workspace-python \"$@\"\n")
		writeCommandExecFile(t, root, filepath.Join(".venv", "bin", "pip"), "#!/bin/sh\necho workspace-pip \"$@\"\n")

		output := strings.TrimSpace(RunBash("python alpha && pip beta", root))
		if !strings.Contains(output, "workspace-python alpha") {
			t.Fatalf("expected virtualenv python in output, got %q", output)
		}
		if !strings.Contains(output, "workspace-pip beta") {
			t.Fatalf("expected virtualenv pip in output, got %q", output)
		}
	})

	t.Run("shell fallback exposes PYTHONPATH", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("shell quoting assertion is for unix-like environments")
		}

		root := t.TempDir()
		output := strings.TrimSpace(RunBash(`python -c "import os; print(os.getenv('PYTHONPATH', ''))"`, root))
		if output != root {
			t.Fatalf("PYTHONPATH = %q, want %q", output, root)
		}
	})
}

func stubCommandExecLookPath(t *testing.T) func() {
	t.Helper()
	old := commandExecLookPath
	return func() {
		commandExecLookPath = old
	}
}

func writeCommandExecFile(t *testing.T, root, relPath, content string) string {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
