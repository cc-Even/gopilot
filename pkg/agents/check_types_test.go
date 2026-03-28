package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTypesToolGoUsesModuleBuild(t *testing.T) {
	root := t.TempDir()
	writeCheckTypesFile(t, root, "go.mod", "module example.com/test\n\ngo 1.25\n")
	writeCheckTypesFile(t, root, "main.go", "package main\n\nfunc main() {}\n")

	restore := stubCheckTypesDeps(t)
	defer restore()

	var gotDir, gotName string
	var gotArgs []string
	checkTypesLookPath = func(file string) (string, error) {
		if file == "go" {
			return "go", nil
		}
		return "", exec.ErrNotFound
	}
	checkTypesExec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotDir = dir
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("ok"), nil
	}

	tool := CheckTypesTool{}
	output, err := tool.Call(context.Background(), `{"path":"main.go"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("check_types failed: %v", err)
	}
	if gotDir != root {
		t.Fatalf("dir = %q, want %q", gotDir, root)
	}
	if gotName != "go" {
		t.Fatalf("name = %q, want go", gotName)
	}
	if strings.Join(gotArgs, " ") != "build -buildvcs=false ./..." {
		t.Fatalf("args = %q, want %q", strings.Join(gotArgs, " "), "build -buildvcs=false ./...")
	}
	if !strings.Contains(output, "Type check passed for main.go using go build -buildvcs=false ./...") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestCheckTypesToolGoFailureIncludesCompilerOutput(t *testing.T) {
	root := t.TempDir()
	writeCheckTypesFile(t, root, "go.mod", "module example.com/test\n\ngo 1.25\n")
	writeCheckTypesFile(t, root, "main.go", "package main\n\nfunc main() {}\n")

	restore := stubCheckTypesDeps(t)
	defer restore()

	checkTypesLookPath = func(file string) (string, error) {
		if file == "go" {
			return "go", nil
		}
		return "", exec.ErrNotFound
	}
	checkTypesExec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return []byte("error obtaining VCS status: exit status 128\n\tUse -buildvcs=false to disable VCS stamping.\n"), &exec.ExitError{}
	}

	tool := CheckTypesTool{}
	output, err := tool.Call(context.Background(), `{"path":"main.go"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("check_types failed: %v", err)
	}
	if !strings.Contains(output, "Type check failed for main.go using go build -buildvcs=false ./...") {
		t.Fatalf("unexpected output header: %s", output)
	}
	if !strings.Contains(output, "error obtaining VCS status") {
		t.Fatalf("expected compiler output in failure: %s", output)
	}
}

func TestCheckTypesToolPythonReportsUnavailableChecker(t *testing.T) {
	root := t.TempDir()
	writeCheckTypesFile(t, root, "service.py", "def build():\n    return 1\n")

	restore := stubCheckTypesDeps(t)
	defer restore()

	checkTypesLookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}

	tool := CheckTypesTool{}
	output, err := tool.Call(context.Background(), `{"path":"service.py"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("check_types failed: %v", err)
	}
	if !strings.Contains(output, "No Python type checker found") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestCheckTypesToolJavaUsesMavenWrapper(t *testing.T) {
	root := t.TempDir()
	writeCheckTypesFile(t, root, "pom.xml", "<project></project>")
	wrapper := writeCheckTypesFile(t, root, platformWrapperName("mvnw"), "")
	writeCheckTypesFile(t, root, "src/main/java/App.java", "class App {}\n")

	restore := stubCheckTypesDeps(t)
	defer restore()

	var gotDir, gotName string
	var gotArgs []string
	checkTypesLookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}
	checkTypesExec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotDir = dir
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("compiled"), nil
	}

	tool := CheckTypesTool{}
	output, err := tool.Call(context.Background(), `{"path":"src/main/java/App.java"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("check_types failed: %v", err)
	}
	if gotDir != root {
		t.Fatalf("dir = %q, want %q", gotDir, root)
	}
	if gotName != wrapper {
		t.Fatalf("name = %q, want %q", gotName, wrapper)
	}
	if strings.Join(gotArgs, " ") != "-q -DskipTests compile" {
		t.Fatalf("args = %q, want %q", strings.Join(gotArgs, " "), "-q -DskipTests compile")
	}
	if !strings.Contains(output, "Type check passed for src/main/java/App.java") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func stubCheckTypesDeps(t *testing.T) func() {
	t.Helper()
	oldExec := checkTypesExec
	oldLookPath := checkTypesLookPath
	return func() {
		checkTypesExec = oldExec
		checkTypesLookPath = oldLookPath
	}
}

func writeCheckTypesFile(t *testing.T, root, relPath, content string) string {
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
