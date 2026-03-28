package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMapToolForGoFileIncludesTypesFunctionsMethodsAndDocs(t *testing.T) {
	root := t.TempDir()
	path := writeTestFile(t, root, "sample.go", `package sample

// Widget keeps business state.
type Widget struct{}

/* Build creates a widget value. */
func Build() Widget {
	return Widget{}
}

// Name returns the widget name.
func (w Widget) Name() string {
	return "widget"
}
`)

	tool := NewRepoMapTool()
	output, err := tool.Call(context.Background(), `{"path":"sample.go"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("repo_map failed for %s: %v", path, err)
	}

	for _, want := range []string{
		"Semantic outline for sample.go (go)",
		"[type] Widget (line 4) - Widget keeps business state.",
		"[func] Build (line 7) - Build creates a widget value.",
		"[method] Name (line 12) - Name returns the widget name.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("repo_map output missing %q:\n%s", want, output)
		}
	}
}

func TestRepoMapToolForPythonFileIncludesClassesAndFunctions(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "sample.py", `# Service handles calls.
class Service:
    pass

# build creates a service.
def build():
    return Service()
`)

	tool := NewRepoMapTool()
	output, err := tool.Call(context.Background(), `{"path":"sample.py"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("repo_map failed: %v", err)
	}

	for _, want := range []string{
		"Semantic outline for sample.py (python)",
		"[class] Service (line 2) - Service handles calls.",
		"[func] build (line 6) - build creates a service.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("repo_map output missing %q:\n%s", want, output)
		}
	}
}

func TestRepoMapToolForJavaFileIncludesTypesMethodsConstructorsAndDocs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "Sample.java", `package sample;

// User stores identity fields.
record User(String name, int age) {}

// Marker marks classes.
@interface Marker {}

// Service handles requests.
class Service {
    // Service builds a default instance.
    Service() {}

    /* run executes the service. */
    void run() {}

    // host and port configure the endpoint.
    private String host, port;
}

// Named defines a contract.
interface Named {}

// Mode lists operating modes.
enum Mode {
    ON, OFF
}
`)

	tool := NewRepoMapTool()
	output, err := tool.Call(context.Background(), `{"path":"Sample.java"}`, &Agent{WorkDir: root})
	if err != nil {
		t.Fatalf("repo_map failed: %v", err)
	}

	for _, want := range []string{
		"Semantic outline for Sample.java (java)",
		"[record] User (line 4) - User stores identity fields.",
		"[annotation_type] Marker (line 7) - Marker marks classes.",
		"[class] Service (line 10) - Service handles requests.",
		"[ctor] Service (line 12) - Service builds a default instance.",
		"[method] run (line 15) - run executes the service.",
		"[field] host (line 18) - host and port configure the endpoint.",
		"[field] port (line 18) - host and port configure the endpoint.",
		"[interface] Named (line 22) - Named defines a contract.",
		"[enum] Mode (line 25) - Mode lists operating modes.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("repo_map output missing %q:\n%s", want, output)
		}
	}
}

func TestRepoMapToolRejectsPathOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	tool := NewRepoMapTool()
	_, err := tool.Call(context.Background(), `{"path":"../outside.go"}`, &Agent{WorkDir: root})
	if err == nil || !strings.Contains(err.Error(), "Path escapes workspace") {
		t.Fatalf("expected workspace escape error, got %v", err)
	}
}

func writeTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
