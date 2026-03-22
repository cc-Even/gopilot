package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessageBusSendReadAndBroadcast(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)

	result := bus.Send("alice", "bob", "hello", "message", map[string]any{"topic": "test"})
	if !strings.Contains(result, "Sent message to bob") {
		t.Fatalf("unexpected send result: %s", result)
	}

	messages := bus.ReadInbox("bob")
	if len(messages) != 1 {
		t.Fatalf("expected 1 inbox message, got %d", len(messages))
	}
	if messages[0].From != "alice" || messages[0].Content != "hello" || messages[0].Type != "message" {
		t.Fatalf("unexpected message: %+v", messages[0])
	}

	drained := bus.ReadInbox("bob")
	if len(drained) != 0 {
		t.Fatalf("expected inbox to be drained, got %d messages", len(drained))
	}

	broadcastResult := bus.Broadcast("alice", "team update", []string{"alice", "bob", "carol"})
	if !strings.Contains(broadcastResult, "Broadcast to 2 teammates") {
		t.Fatalf("unexpected broadcast result: %s", broadcastResult)
	}
	if len(bus.ReadInbox("bob")) != 1 {
		t.Fatalf("expected broadcast for bob")
	}
	if len(bus.ReadInbox("carol")) != 1 {
		t.Fatalf("expected broadcast for carol")
	}

	raw, err := os.ReadFile(talkPath)
	if err != nil {
		t.Fatalf("read talk log failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 talk log lines, got %d: %q", len(lines), string(raw))
	}
	if !strings.Contains(lines[0], "from=alice") || !strings.Contains(lines[0], "to=bob") || !strings.Contains(lines[0], `content="hello"`) {
		t.Fatalf("unexpected first talk log line: %s", lines[0])
	}
	if !strings.Contains(lines[1], `content="team update"`) || !strings.Contains(lines[2], `content="team update"`) {
		t.Fatalf("unexpected broadcast talk log: %q", string(raw))
	}
}

func TestTeammateManagerSpawnPersistsConfigAndResetsStatus(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		if agent == nil {
			t.Fatalf("expected cloned agent")
		}
		if agent.Name != "worker-1" {
			t.Fatalf("unexpected agent name: %s", agent.Name)
		}
		if !strings.Contains(agent.SystemPrompt, "role \"reviewer\"") {
			t.Fatalf("system prompt missing teammate role: %s", agent.SystemPrompt)
		}
		if prompt != "inspect core changes" {
			t.Fatalf("unexpected prompt: %s", prompt)
		}
		return nil
	}

	result := manager.Spawn("worker-1", "reviewer", "inspect core changes", "")
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		member := manager.findMemberLocked("worker-1")
		return member != nil && member.Status == teammateStatusIdle
	})

	raw, err := os.ReadFile(filepath.Join(teamDir, "config.json"))
	if err != nil {
		t.Fatalf("read config failed: %v", err)
	}
	if !strings.Contains(string(raw), `"name": "worker-1"`) {
		t.Fatalf("config missing teammate entry: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"status": "idle"`) {
		t.Fatalf("config missing idle status: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"prompt": "inspect core changes"`) {
		t.Fatalf("config missing original prompt: %s", string(raw))
	}
}

func TestTeammateManagerWakeReusesOriginalPrompt(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)

	prompts := make(chan string, 2)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		prompts <- prompt
		return nil
	}

	result := manager.Spawn("worker-1", "reviewer", "inspect core changes", "")
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	select {
	case got := <-prompts:
		if got != "inspect core changes" {
			t.Fatalf("unexpected spawn prompt: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("spawn prompt not received")
	}

	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})

	wakeResult := manager.Wake("worker-1", "You received a new direct message. Read your inbox and respond if needed.")
	if !strings.Contains(wakeResult, `Woke "worker-1"`) {
		t.Fatalf("unexpected wake result: %s", wakeResult)
	}

	select {
	case got := <-prompts:
		if got != "inspect core changes" {
			t.Fatalf("wake should reuse original prompt, got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("wake prompt not received")
	}
}

func waitForCondition(t *testing.T, fn func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
