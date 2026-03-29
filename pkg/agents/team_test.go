package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func TestMessageBusSendReadAndBroadcast(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)

	result, err := bus.Send("alice", "bob", "hello", "message", map[string]any{"topic": "test"})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
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

	broadcastResult, err := bus.Broadcast("alice", "team update", []string{"alice", "bob", "carol"})
	if err != nil {
		t.Fatalf("broadcast failed: %v", err)
	}
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

func TestMessageBusPersistsUnreadAcrossRestart(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")

	bus := NewMessageBus(talkPath)
	result, err := bus.Send("alice", "bob", "persist me", "message", nil)
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if !strings.Contains(result, "Sent message to bob") {
		t.Fatalf("unexpected send result: %s", result)
	}

	restarted := NewMessageBus(talkPath)
	messages := restarted.ReadInbox("bob")
	if len(messages) != 1 {
		t.Fatalf("expected 1 persisted inbox message, got %d", len(messages))
	}
	if messages[0].From != "alice" || messages[0].Content != "persist me" {
		t.Fatalf("unexpected persisted message: %+v", messages[0])
	}

	afterDrain := NewMessageBus(talkPath)
	if drained := afterDrain.ReadInbox("bob"); len(drained) != 0 {
		t.Fatalf("expected persisted inbox to drain, got %d messages", len(drained))
	}
}

func TestMessageBusPeekAndAck(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)
	if result, err := bus.Send("alice", "bob", "peek me", "message", nil); err != nil {
		t.Fatalf("send failed: %v", err)
	} else if !strings.Contains(result, "Sent message to bob") {
		t.Fatalf("unexpected send result: %s", result)
	}

	peeked, keys, err := bus.PeekInbox("bob")
	if err != nil {
		t.Fatalf("peek inbox failed: %v", err)
	}
	if len(peeked) != 1 || len(keys) != 1 {
		t.Fatalf("expected 1 peeked message and key, got messages=%d keys=%d", len(peeked), len(keys))
	}
	if peeked[0].Content != "peek me" {
		t.Fatalf("unexpected peeked message: %+v", peeked[0])
	}

	peekedAgain, _, err := bus.PeekInbox("bob")
	if err != nil {
		t.Fatalf("second peek inbox failed: %v", err)
	}
	if len(peekedAgain) != 1 {
		t.Fatalf("peek should not drain inbox, got %d messages", len(peekedAgain))
	}

	if err := bus.AckInbox("bob", keys); err != nil {
		t.Fatalf("ack inbox failed: %v", err)
	}
	if drained := bus.ReadInbox("bob"); len(drained) != 0 {
		t.Fatalf("expected inbox to be empty after ack, got %+v", drained)
	}
}

func TestMessageBusSendWaitsForInboxFileLock(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)

	lockedFile, err := os.OpenFile(bus.inboxPath("bob"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open inbox lock file failed: %v", err)
	}
	defer lockedFile.Close()
	if err := lockFile(lockedFile); err != nil {
		t.Fatalf("lock inbox file failed: %v", err)
	}

	done := make(chan struct {
		result string
		err    error
	}, 1)
	go func() {
		result, err := bus.Send("alice", "bob", "wait for lock", "message", nil)
		done <- struct {
			result string
			err    error
		}{result: result, err: err}
	}()

	select {
	case send := <-done:
		t.Fatalf("send should block on locked inbox file, got result=%q err=%v", send.result, send.err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := unlockFile(lockedFile); err != nil {
		t.Fatalf("unlock inbox file failed: %v", err)
	}

	select {
	case send := <-done:
		if send.err != nil {
			t.Fatalf("send failed after unlock: %v", send.err)
		}
		result := send.result
		if !strings.Contains(result, "Sent message to bob") {
			t.Fatalf("unexpected send result after unlock: %s", result)
		}
	case <-time.After(time.Second):
		t.Fatal("send did not resume after unlock")
	}

	messages := bus.ReadInbox("bob")
	if len(messages) != 1 || messages[0].Content != "wait for lock" {
		t.Fatalf("expected locked send message to persist, got %+v", messages)
	}
}

func TestMessageBusReadWaitsForInboxFileLock(t *testing.T) {
	talkPath := filepath.Join(t.TempDir(), "talk.txt")
	bus := NewMessageBus(talkPath)
	if result, err := bus.Send("alice", "bob", "block reader", "message", nil); err != nil {
		t.Fatalf("send failed: %v", err)
	} else if !strings.Contains(result, "Sent message to bob") {
		t.Fatalf("unexpected send result: %s", result)
	}

	lockedFile, err := os.OpenFile(bus.inboxPath("bob"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open inbox lock file failed: %v", err)
	}
	defer lockedFile.Close()
	if err := lockFile(lockedFile); err != nil {
		t.Fatalf("lock inbox file failed: %v", err)
	}

	done := make(chan []TeamMessage, 1)
	go func() {
		done <- bus.ReadInbox("bob")
	}()

	select {
	case messages := <-done:
		t.Fatalf("read should block on locked inbox file, got %+v", messages)
	case <-time.After(100 * time.Millisecond):
	}

	if err := unlockFile(lockedFile); err != nil {
		t.Fatalf("unlock inbox file failed: %v", err)
	}

	select {
	case messages := <-done:
		if len(messages) != 1 || messages[0].Content != "block reader" {
			t.Fatalf("unexpected read result after unlock: %+v", messages)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not resume after unlock")
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
	release := make(chan struct{})
	runnerCalled := make(chan struct{}, 1)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		runnerCalled <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	select {
	case <-runnerCalled:
	case <-time.After(time.Second):
		t.Fatal("runner was not called")
	}

	manager.mu.Lock()
	member := manager.findMemberLocked("worker-1")
	if member == nil || member.Status == teammateStatusIdle {
		manager.mu.Unlock()
		t.Fatalf("expected spawned teammate to stay working, got %+v", member)
	}
	if member.Prompt != "inspect core changes" {
		manager.mu.Unlock()
		t.Fatalf("expected prompt persisted on member, got %+v", member)
	}
	if member.Supervisor != "lead" {
		manager.mu.Unlock()
		t.Fatalf("expected supervisor persisted on member, got %+v", member)
	}
	if len(manager.threads) != 1 {
		manager.mu.Unlock()
		t.Fatalf("spawn should register running thread, got %d", len(manager.threads))
	}
	manager.mu.Unlock()

	raw, err := os.ReadFile(filepath.Join(teamDir, "config.json"))
	if err != nil {
		t.Fatalf("read config failed: %v", err)
	}
	if !strings.Contains(string(raw), `"name": "worker-1"`) {
		t.Fatalf("config missing teammate entry: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"status": "working"`) {
		t.Fatalf("config missing working status: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"prompt": "inspect core changes"`) {
		t.Fatalf("config missing saved prompt: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"supervisor": "lead"`) {
		t.Fatalf("config missing saved supervisor: %s", string(raw))
	}

	close(release)
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		member := manager.findMemberLocked("worker-1")
		return len(manager.threads) == 0 && member != nil && member.Status == teammateStatusIdle
	})
}

func TestTeammateManagerWakeStartsInboxDrivenLoop(t *testing.T) {
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
	releases := make(chan chan struct{}, 2)
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		prompts <- prompt
		release := make(chan struct{})
		releases <- release
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	select {
	case got := <-prompts:
		if got != "inspect core changes" {
			t.Fatalf("spawn should use original prompt, got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("spawn prompt not received")
	}

	close(<-releases)
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})

	wakeResult, err := manager.Wake("worker-1")
	if err != nil {
		t.Fatalf("wake failed: %v", err)
	}
	if !strings.Contains(wakeResult, `Woke "worker-1"`) {
		t.Fatalf("unexpected wake result: %s", wakeResult)
	}

	select {
	case got := <-prompts:
		if !strings.Contains(got, "Read your inbox") {
			t.Fatalf("wake should provide inbox-driven prompt, got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("wake prompt not received")
	}

	close(<-releases)
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestSpawnTeammateAssignsRunID(t *testing.T) {
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
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) || !strings.Contains(result, "run_id: run_") {
		t.Fatalf("spawn should include run_id, got %s", result)
	}

	manager.mu.Lock()
	member := manager.findMemberLocked("worker-1")
	if member == nil || member.RunID == "" {
		manager.mu.Unlock()
		t.Fatalf("expected teammate run_id to be assigned, got %+v", member)
	}
	run := manager.runs[member.RunID]
	manager.mu.Unlock()
	if run == nil || run.Status != teammateRunStatusRunning {
		t.Fatalf("expected running teammate run, got %+v", run)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestSendMessageToolRejectsUnknownRecipient(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager

	_, err := sendMessageTool(context.Background(), json.RawMessage(`{"to":"ghost","content":"hello"}`), base)
	if err == nil {
		t.Fatal("expected unknown recipient to be rejected")
	}
	if !strings.Contains(err.Error(), `unknown team participant "ghost"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendMessageToolAllowsSupervisorAndLeadReadsInbox(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 1
	})

	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	sendResult, err := sendMessageTool(context.Background(), json.RawMessage(`{"to":"lead","content":"task complete"}`), worker)
	if err != nil {
		t.Fatalf("sendMessageTool failed: %v", err)
	}
	if !strings.Contains(sendResult, "Sent message to lead") {
		t.Fatalf("unexpected send result: %s", sendResult)
	}

	messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("continue")}
	updated := base.appendTeamInboxMessages(messages)
	if len(updated) != len(messages)+1 {
		t.Fatalf("expected appended inbox message, got %d messages", len(updated))
	}
	role, content, err := messageRoleAndContent(updated[len(updated)-1])
	if err != nil {
		t.Fatalf("read appended inbox message failed: %v", err)
	}
	if role != "user" || !strings.Contains(content, "<inbox>") || !strings.Contains(content, "task complete") {
		t.Fatalf("unexpected appended inbox payload: role=%s content=%q", role, content)
	}

	drained := base.appendTeamInboxMessages(updated)
	if len(drained) != len(updated) {
		t.Fatalf("expected inbox to be drained after append, got %d messages", len(drained))
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestSendMessageToolReportsRunCompletionAndWaitTeammateReturnsResult(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	runID := manager.findMemberLocked("worker-1").RunID
	manager.mu.Unlock()
	if runID == "" {
		t.Fatal("expected run_id to be assigned")
	}

	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	sendResult, err := sendMessageTool(context.Background(), json.RawMessage(`{"to":"lead","content":"task complete","status":"completed"}`), worker)
	if err != nil {
		t.Fatalf("sendMessageTool failed: %v", err)
	}
	if !strings.Contains(sendResult, "Sent message to lead") {
		t.Fatalf("unexpected send result: %s", sendResult)
	}

	waitResult, err := waitTeammateTool(context.Background(), json.RawMessage(fmt.Sprintf(`{"run_id":%q,"timeout_seconds":1}`, runID)), base)
	if err != nil {
		t.Fatalf("waitTeammateTool failed: %v", err)
	}

	var run TeammateRun
	if err := json.Unmarshal([]byte(waitResult), &run); err != nil {
		t.Fatalf("parse wait result failed: %v", err)
	}
	if run.Status != teammateRunStatusCompleted || run.Result != "task complete" {
		t.Fatalf("unexpected run state: %+v", run)
	}

	inbox := manager.bus.ReadInbox("lead")
	if len(inbox) != 1 {
		t.Fatalf("expected 1 inbox message, got %d", len(inbox))
	}
	if inbox[0].Metadata["run_id"] != runID || inbox[0].Metadata["status"] != teammateRunStatusCompleted {
		t.Fatalf("expected completion metadata on inbox message, got %+v", inbox[0].Metadata)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestWaitForRunReturnsTimedOutSnapshot(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	runID := manager.findMemberLocked("worker-1").RunID
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if runID == "" {
		t.Fatal("expected run_id to be assigned")
	}

	run, err := manager.WaitForRun(context.Background(), runID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForRun failed: %v", err)
	}
	if run == nil || run.Status != teammateRunStatusTimedOut || run.LastKnownStatus != teammateRunStatusRunning {
		t.Fatalf("expected timed out running snapshot, got %+v", run)
	}

	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestLeadInboxSurvivesManagerRestart(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})

	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	sendResult, err := sendMessageTool(context.Background(), json.RawMessage(`{"to":"lead","content":"survives restart"}`), worker)
	if err != nil {
		t.Fatalf("sendMessageTool failed: %v", err)
	}
	if !strings.Contains(sendResult, "Sent message to lead") {
		t.Fatalf("unexpected send result: %s", sendResult)
	}

	restartedLead := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}
	restartedManager := NewTeammateManager(teamDir, restartedLead)
	restartedLead.TeamManager = restartedManager

	messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("continue")}
	updated := restartedLead.appendTeamInboxMessages(messages)
	if len(updated) != len(messages)+1 {
		t.Fatalf("expected restarted lead to receive persisted inbox message, got %d messages", len(updated))
	}
	_, content, err := messageRoleAndContent(updated[len(updated)-1])
	if err != nil {
		t.Fatalf("read persisted inbox payload failed: %v", err)
	}
	if !strings.Contains(content, "survives restart") {
		t.Fatalf("unexpected persisted inbox content: %q", content)
	}
}

func TestTeammateLoopFailsRunWithoutExplicitCompletionReport(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	runID := manager.findMemberLocked("worker-1").RunID
	manager.mu.Unlock()
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
	if runID == "" {
		t.Fatal("expected run_id to be captured before teammate exits")
	}

	waitResult, err := waitTeammateTool(context.Background(), json.RawMessage(fmt.Sprintf(`{"run_id":%q,"timeout_seconds":1}`, runID)), base)
	if err != nil {
		t.Fatalf("waitTeammateTool failed: %v", err)
	}

	var run TeammateRun
	if err := json.Unmarshal([]byte(waitResult), &run); err != nil {
		t.Fatalf("parse wait result failed: %v", err)
	}
	if run.Status != teammateRunStatusFailed {
		t.Fatalf("expected failed run status, got %+v", run)
	}
	if !strings.Contains(run.Error, "without explicit completion report") {
		t.Fatalf("expected missing report failure, got %+v", run)
	}
	if run.FailureKind != teammateFailureKindProtocol || run.Retryable {
		t.Fatalf("expected protocol non-retryable failure, got %+v", run)
	}

	inbox := manager.bus.ReadInbox("lead")
	if len(inbox) != 1 {
		t.Fatalf("expected supervisor failure notification, got %d messages", len(inbox))
	}
	if inbox[0].Metadata["status"] != teammateRunStatusFailed || inbox[0].Metadata["failure_kind"] != teammateFailureKindProtocol {
		t.Fatalf("unexpected failure notification metadata: %+v", inbox[0].Metadata)
	}
	if retryable, ok := inbox[0].Metadata["retryable"].(bool); !ok || retryable {
		t.Fatalf("expected non-retryable failure notification, got %+v", inbox[0].Metadata)
	}
}

func TestTeammateLoopClassifiesRetryableFailureAndNotifiesSupervisor(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		return errors.New("chat completion failed (turn=0): i/o timeout")
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "lead")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	manager.mu.Lock()
	runID := manager.findMemberLocked("worker-1").RunID
	manager.mu.Unlock()
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})

	waitResult, err := waitTeammateTool(context.Background(), json.RawMessage(fmt.Sprintf(`{"run_id":%q,"timeout_seconds":1}`, runID)), base)
	if err != nil {
		t.Fatalf("waitTeammateTool failed: %v", err)
	}

	var run TeammateRun
	if err := json.Unmarshal([]byte(waitResult), &run); err != nil {
		t.Fatalf("parse wait result failed: %v", err)
	}
	if run.Status != teammateRunStatusFailed {
		t.Fatalf("expected failed run status, got %+v", run)
	}
	if run.FailureKind != teammateFailureKindNetwork || !run.Retryable {
		t.Fatalf("expected network retryable failure, got %+v", run)
	}

	inbox := manager.bus.ReadInbox("lead")
	if len(inbox) != 1 {
		t.Fatalf("expected supervisor failure notification, got %d messages", len(inbox))
	}
	if inbox[0].Metadata["run_id"] != runID || inbox[0].Metadata["failure_kind"] != teammateFailureKindNetwork {
		t.Fatalf("unexpected failure notification metadata: %+v", inbox[0].Metadata)
	}
	if retryable, ok := inbox[0].Metadata["retryable"].(bool); !ok || !retryable {
		t.Fatalf("expected retryable failure notification, got %+v", inbox[0].Metadata)
	}
	if !strings.Contains(inbox[0].Content, "Teammate worker-1 failed.") || !strings.Contains(inbox[0].Content, "retryable=true") {
		t.Fatalf("unexpected failure notification content: %+v", inbox[0])
	}
}

func TestCompleteTaskAndReportToolCompletesTaskAndReportsRun(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")
	teamDir := filepath.Join(stateDir, "teams")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("summarize team", "read and summarize team.go"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}
	worktreeManager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}

	base := &Agent{
		Name:            "supervisor",
		SystemPrompt:    "You are the lead agent.",
		Model:           "test-model",
		TaskManager:     taskManager,
		WorktreeManager: worktreeManager,
		tools:           map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "supervisor")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	if _, err := claimTaskTool(context.Background(), json.RawMessage(`{"task_id":0}`), worker); err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}

	output, err := completeTaskAndReportTool(context.Background(), json.RawMessage(`{"content":"done"}`), worker)
	if err != nil {
		t.Fatalf("completeTaskAndReportTool failed: %v", err)
	}
	if !strings.Contains(output, `"status": "completed"`) {
		t.Fatalf("expected completed task output, got %s", output)
	}

	task := mustLoadTask(t, taskManager, 0)
	if task.Status != taskStatusCompleted {
		t.Fatalf("expected task completed, got %+v", task)
	}
	if task.Worktree != "" {
		t.Fatalf("expected task worktree to be unbound, got %+v", task)
	}

	manager.mu.Lock()
	runID := manager.runs[manager.findMemberLocked("worker-1").RunID]
	manager.mu.Unlock()
	if runID == nil || runID.Status != teammateRunStatusCompleted {
		t.Fatalf("expected completed run, got %+v", runID)
	}

	inbox := manager.bus.ReadInbox("supervisor")
	if len(inbox) != 1 {
		t.Fatalf("expected one supervisor inbox message, got %d", len(inbox))
	}
	if inbox[0].Metadata["status"] != teammateRunStatusCompleted || inbox[0].Metadata["task_id"] != float64(0) && inbox[0].Metadata["task_id"] != 0 {
		t.Fatalf("expected completed metadata, got %+v", inbox[0].Metadata)
	}

	records, err := worktreeManager.List()
	if err != nil {
		t.Fatalf("list worktrees failed: %v", err)
	}
	if len(records) == 0 || records[0].Status != worktreeStatusRemoved {
		t.Fatalf("expected removed worktree record, got %+v", records)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestCompleteTaskAndReportToolKeepsWorktreeWhenRequested(t *testing.T) {
	repoDir := initTestRepo(t)
	stateDir := t.TempDir()
	taskDir := filepath.Join(stateDir, "tasks")
	worktreeDir := filepath.Join(stateDir, "worktrees")
	teamDir := filepath.Join(stateDir, "teams")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("summarize worktree", "read and summarize worktree.go"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}
	worktreeManager, err := NewWorktreeManager(repoDir, worktreeDir, taskManager)
	if err != nil {
		t.Fatalf("create worktree manager failed: %v", err)
	}

	base := &Agent{
		Name:            "supervisor",
		SystemPrompt:    "You are the lead agent.",
		Model:           "test-model",
		TaskManager:     taskManager,
		WorktreeManager: worktreeManager,
		tools:           map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	base.TeamManager = manager
	manager.runner = func(ctx context.Context, agent *Agent, prompt string) error {
		<-ctx.Done()
		return nil
	}

	if result, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", "supervisor"); err != nil {
		t.Fatalf("spawn failed: %v", err)
	} else if !strings.Contains(result, `Spawned "worker-1"`) {
		t.Fatalf("unexpected spawn result: %s", result)
	}

	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	if _, err := claimTaskTool(context.Background(), json.RawMessage(`{"task_id":0}`), worker); err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}

	output, err := completeTaskAndReportTool(context.Background(), json.RawMessage(`{"content":"done","keep_worktree":true}`), worker)
	if err != nil {
		t.Fatalf("completeTaskAndReportTool failed: %v", err)
	}
	if !strings.Contains(output, `"status": "completed"`) {
		t.Fatalf("expected completed task output, got %s", output)
	}

	task := mustLoadTask(t, taskManager, 0)
	if task.Status != taskStatusCompleted {
		t.Fatalf("expected task completed, got %+v", task)
	}
	if task.Worktree == "" {
		t.Fatalf("expected task worktree to remain bound, got %+v", task)
	}

	records, err := worktreeManager.List()
	if err != nil {
		t.Fatalf("list worktrees failed: %v", err)
	}
	if len(records) == 0 || records[0].Status != worktreeStatusActive {
		t.Fatalf("expected active worktree record, got %+v", records)
	}
	if _, err := os.Stat(records[0].Path); err != nil {
		t.Fatalf("expected worktree path to remain: %v", err)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	manager.mu.Unlock()
	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
}

func TestCloneAgentIncludesTypeCheckWorkflowAndTool(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		tools: map[string]ToolDefinition{
			"check_types": {Name: "check_types"},
			"read_file":   {Name: "read_file"},
		},
		order: []string{"read_file", "check_types"},
	}

	manager := NewTeammateManager(teamDir, base)
	worker := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	if worker == nil {
		t.Fatal("expected cloned worker")
	}
	if !strings.Contains(worker.SystemPrompt, "run check_types") {
		t.Fatalf("worker system prompt missing type-check workflow: %q", worker.SystemPrompt)
	}
	if _, ok := worker.tools["check_types"]; !ok {
		t.Fatalf("worker tools missing check_types: %+v", worker.tools)
	}
}

func TestTeammateManagerNextIdleEventPrefersInbox(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	if _, err := manager.bus.Send("lead", "worker-1", "check inbox first", "message", nil); err != nil {
		t.Fatalf("seed inbox failed: %v", err)
	}
	message, err := manager.nextIdleEvent(agent)
	if err != nil {
		t.Fatalf("nextIdleEvent failed: %v", err)
	}
	if !strings.Contains(message, "<inbox>") || !strings.Contains(message, "check inbox first") {
		t.Fatalf("expected inbox message, got %q", message)
	}

	taskJSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	var task Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		t.Fatalf("parse task failed: %v", err)
	}
	if task.Owner != "" || task.Status != "pending" {
		t.Fatalf("inbox should not claim task yet, got %+v", task)
	}
}

func TestTeammateManagerNextIdleEventClaimsTask(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	message, err := manager.nextIdleEvent(agent)
	if err != nil {
		t.Fatalf("nextIdleEvent failed: %v", err)
	}
	if !strings.Contains(message, "<task_claim>") || !strings.Contains(message, `"owner": "worker-1"`) {
		t.Fatalf("expected claimed task payload, got %q", message)
	}

	taskJSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task failed: %v", err)
	}
	var task Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		t.Fatalf("parse task failed: %v", err)
	}
	if task.Owner != "worker-1" || task.Status != "in_progress" {
		t.Fatalf("expected claimed task persisted, got %+v", task)
	}
}

func TestClaimTaskToolClaimsTaskEvenWithInboxMessages(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("claim me", "ready"); err != nil {
		t.Fatalf("create task failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")
	if _, err := manager.bus.Send("lead", "worker-1", "please read later", "message", nil); err != nil {
		t.Fatalf("seed inbox failed: %v", err)
	}

	result, err := claimTaskTool(context.Background(), json.RawMessage(`{}`), agent)
	if err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}
	if !strings.Contains(result, "<task_claim>") || !strings.Contains(result, `"owner": "worker-1"`) {
		t.Fatalf("expected claimed task payload, got %q", result)
	}

	messages := manager.bus.ReadInbox("worker-1")
	if len(messages) != 1 || messages[0].Content != "please read later" {
		t.Fatalf("claim_task should not drain inbox, got %+v", messages)
	}
}

func TestClaimTaskToolClaimsSpecificTaskID(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("first", "blocked task"); err != nil {
		t.Fatalf("create task 0 failed: %v", err)
	}
	if _, err := taskManager.Create("second", "claim this one"); err != nil {
		t.Fatalf("create task 1 failed: %v", err)
	}
	if _, err := taskManager.Update(1, "", []int{0}, nil); err != nil {
		t.Fatalf("block task 1 failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	result, err := claimTaskTool(context.Background(), json.RawMessage(`{"task_id":0}`), agent)
	if err != nil {
		t.Fatalf("claimTaskTool failed: %v", err)
	}
	if !strings.Contains(result, "<task_claim>") || !strings.Contains(result, `"id": 0`) {
		t.Fatalf("expected claimed task 0 payload, got %q", result)
	}

	task0JSON, err := taskManager.Get(0)
	if err != nil {
		t.Fatalf("get task 0 failed: %v", err)
	}
	var task0 Task
	if err := json.Unmarshal([]byte(task0JSON), &task0); err != nil {
		t.Fatalf("parse task 0 failed: %v", err)
	}
	if task0.Owner != "worker-1" || task0.Status != "in_progress" {
		t.Fatalf("expected task 0 claimed, got %+v", task0)
	}

	task1JSON, err := taskManager.Get(1)
	if err != nil {
		t.Fatalf("get task 1 failed: %v", err)
	}
	var task1 Task
	if err := json.Unmarshal([]byte(task1JSON), &task1); err != nil {
		t.Fatalf("parse task 1 failed: %v", err)
	}
	if task1.Owner != "" || task1.Status != "pending" {
		t.Fatalf("expected task 1 untouched, got %+v", task1)
	}
}

func TestClaimTaskToolRejectsBlockedSpecificTaskID(t *testing.T) {
	tempDir := t.TempDir()
	teamDir := filepath.Join(tempDir, ".teams")
	taskDir := filepath.Join(tempDir, ".tasks")

	taskManager, err := NewTaskManager(taskDir)
	if err != nil {
		t.Fatalf("create task manager failed: %v", err)
	}
	if _, err := taskManager.Create("dependency", "must finish first"); err != nil {
		t.Fatalf("create task 0 failed: %v", err)
	}
	if _, err := taskManager.Create("blocked", "cannot claim yet"); err != nil {
		t.Fatalf("create task 1 failed: %v", err)
	}
	if _, err := taskManager.Update(1, "", []int{0}, nil); err != nil {
		t.Fatalf("block task 1 failed: %v", err)
	}

	base := &Agent{
		Name:         "lead",
		SystemPrompt: "You are the lead agent.",
		Model:        "test-model",
		TaskManager:  taskManager,
		tools:        map[string]ToolDefinition{},
	}

	manager := NewTeammateManager(teamDir, base)
	agent := manager.cloneAgent("worker-1", "reviewer", "inspect core changes")

	_, err = claimTaskTool(context.Background(), json.RawMessage(`{"task_id":1}`), agent)
	if err == nil {
		t.Fatal("expected blocked task claim to fail")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked error, got %v", err)
	}
}

func TestTeammateManagerWaitUntilIdleReturnsForIdleThreads(t *testing.T) {
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
		<-ctx.Done()
		return nil
	}
	if _, err := manager.Spawn("worker-1", "reviewer", "inspect core changes", ""); err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	manager.mu.Lock()
	stopRunner := manager.threads["worker-1"]
	member := manager.findMemberLocked("worker-1")
	member.Status = teammateStatusIdle
	manager.mu.Unlock()

	ctx, cancelWait := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelWait()
	if err := manager.WaitUntilIdle(ctx); err != nil {
		t.Fatalf("WaitUntilIdle should return for idle teammate, got %v", err)
	}

	if stopRunner != nil {
		stopRunner()
	}
	waitForCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.threads) == 0
	})
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
