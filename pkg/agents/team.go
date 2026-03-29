package agents

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

const (
	teammateStatusIdle     = "idle"
	teammateStatusWorking  = "working"
	teammateStatusShutdown = "shutdown"

	teammateIdlePollInterval = 5 * time.Second
	teammateIdleTimeout      = 60 * time.Second

	teammateFailureKindNetwork   = "network"
	teammateFailureKindTimeout   = "timeout"
	teammateFailureKindCancelled = "cancelled"
	teammateFailureKindProtocol  = "protocol"
	teammateFailureKindInternal  = "internal"
	teammateFailureKindReported  = "reported_failed"
)

var validMessageTypes = map[string]struct{}{
	"message":   {},
	"broadcast": {},
}

type TeamMessage struct {
	ID        string         `json:"id,omitempty"`
	Type      string         `json:"type"`
	From      string         `json:"from"`
	Content   string         `json:"content"`
	Timestamp float64        `json:"timestamp"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type MessageBus struct {
	mu       sync.Mutex
	logMu    sync.Mutex
	logPath  string
	inboxDir string
}

func NewMessageBus(logPath string) *MessageBus {
	if strings.TrimSpace(logPath) == "" {
		logPath = TALK_LOG_PATH
	}
	inboxDir := filepath.Join(filepath.Dir(logPath), "inboxes")
	_ = os.MkdirAll(inboxDir, 0o755)
	return &MessageBus{
		logPath:  logPath,
		inboxDir: inboxDir,
	}
}

func (b *MessageBus) inboxPath(name string) string {
	if b == nil {
		return ""
	}
	clean := strings.TrimSpace(name)
	if clean == "" {
		clean = "unknown"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_")
	clean = replacer.Replace(clean)
	return filepath.Join(b.inboxDir, clean+".jsonl")
}

func (b *MessageBus) appendInboxMessage(name string, msg TeamMessage) error {
	if b == nil {
		return fmt.Errorf("message bus not initialized")
	}
	if err := os.MkdirAll(b.inboxDir, 0o755); err != nil {
		return err
	}

	encoded, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(b.inboxPath(name), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file); err != nil {
		return err
	}
	defer func() {
		if err := unlockFile(file); err != nil {
			log.Printf("[MessageBus] failed to unlock inbox file %q: %v", file.Name(), err)
		}
	}()

	_, err = file.Write(append(encoded, '\n'))
	return err
}

func (b *MessageBus) Send(sender, to, content, msgType string, extra map[string]any) (string, error) {
	if b == nil {
		return "", fmt.Errorf("message bus not initialized")
	}
	if msgType == "" {
		msgType = "message"
	}
	if _, ok := validMessageTypes[msgType]; !ok {
		return "", fmt.Errorf("invalid type %q. Valid: %s", msgType, strings.Join(validMessageTypeList(), ", "))
	}
	now := time.Now()
	metadata := cloneTeamMetadata(extra)
	msg := TeamMessage{
		ID:        newTeamMessageID(),
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(now.UnixNano()) / float64(time.Second),
		Metadata:  metadata,
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.appendInboxMessage(to, msg); err != nil {
		return "", fmt.Errorf("persist inbox for %q failed: %w", to, err)
	}
	if err := b.appendTalkLog(now, sender, to, content); err != nil {
		log.Printf("[MessageBus] failed to append talk log: %v", err)
	}
	return fmt.Sprintf("Sent %s to %s", msgType, to), nil
}

func (b *MessageBus) appendTalkLog(ts time.Time, sender, receiver, content string) error {
	if b == nil || strings.TrimSpace(b.logPath) == "" {
		return nil
	}

	encodedContent, err := json.Marshal(content)
	if err != nil {
		return err
	}

	b.logMu.Lock()
	defer b.logMu.Unlock()

	file, err := os.OpenFile(b.logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file); err != nil {
		return err
	}
	defer func() {
		if err := unlockFile(file); err != nil {
			log.Printf("[MessageBus] failed to unlock talk log %q: %v", file.Name(), err)
		}
	}()

	line := fmt.Sprintf("%s\tfrom=%s\tto=%s\tcontent=%s\n", ts.Format(time.RFC3339Nano), sender, receiver, string(encodedContent))
	_, err = file.WriteString(line)
	return err
}

type inboxRecord struct {
	Key     string
	Message TeamMessage
}

func (b *MessageBus) PeekInbox(name string) ([]TeamMessage, []string, error) {
	if b == nil {
		return nil, nil, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.inboxPath(name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		log.Printf("[MessageBus] failed to read inbox %q: %v", name, err)
		return nil, nil, err
	}
	defer file.Close()
	if err := lockFile(file); err != nil {
		log.Printf("[MessageBus] failed to lock inbox %q: %v", name, err)
		return nil, nil, err
	}
	defer func() {
		if err := unlockFile(file); err != nil {
			log.Printf("[MessageBus] failed to unlock inbox %q: %v", name, err)
		}
	}()

	records, err := b.readInboxRecordsLocked(file, name)
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, nil
	}

	messages := make([]TeamMessage, 0, len(records))
	keys := make([]string, 0, len(records))
	for _, record := range records {
		messages = append(messages, record.Message)
		keys = append(keys, record.Key)
	}
	return messages, keys, nil
}

func (b *MessageBus) AckInbox(name string, keys []string) error {
	if b == nil || len(keys) == 0 {
		return nil
	}

	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		keySet[key] = struct{}{}
	}
	if len(keySet) == 0 {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.inboxPath(name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file); err != nil {
		return err
	}
	defer func() {
		if err := unlockFile(file); err != nil {
			log.Printf("[MessageBus] failed to unlock inbox %q: %v", name, err)
		}
	}()

	records, err := b.readInboxRecordsLocked(file, name)
	if err != nil {
		return err
	}
	kept := make([]TeamMessage, 0, len(records))
	for _, record := range records {
		if _, ok := keySet[record.Key]; ok {
			continue
		}
		kept = append(kept, record.Message)
	}
	return b.rewriteInboxLocked(file, kept)
}

func (b *MessageBus) ReadInbox(name string) []TeamMessage {
	messages, keys, err := b.PeekInbox(name)
	if err != nil {
		return nil
	}
	if len(keys) == 0 {
		return messages
	}
	if err := b.AckInbox(name, keys); err != nil {
		log.Printf("[MessageBus] failed to ack inbox %q: %v", name, err)
		return nil
	}
	return messages
}

func (b *MessageBus) readInboxRecordsLocked(file *os.File, name string) ([]inboxRecord, error) {
	if _, err := file.Seek(0, 0); err != nil {
		log.Printf("[MessageBus] failed to rewind inbox %q before read: %v", name, err)
		return nil, err
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		log.Printf("[MessageBus] failed to read inbox %q: %v", name, err)
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}

	records := make([]inboxRecord, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg TeamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("[MessageBus] failed to decode inbox message for %q: %v", name, err)
			continue
		}
		key := msg.ID
		if key == "" {
			sum := sha1.Sum([]byte(line))
			key = fmt.Sprintf("%x", sum[:])
		}
		records = append(records, inboxRecord{
			Key:     key,
			Message: msg,
		})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[MessageBus] failed to scan inbox %q: %v", name, err)
		return nil, err
	}
	return records, nil
}

func (b *MessageBus) rewriteInboxLocked(file *os.File, messages []TeamMessage) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	for _, msg := range messages {
		encoded, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func (b *MessageBus) Broadcast(sender, content string, teammates []string) (string, error) {
	if b == nil {
		return "", fmt.Errorf("message bus not initialized")
	}
	count := 0
	for _, name := range teammates {
		if name == "" || name == sender {
			continue
		}
		if _, err := b.Send(sender, name, content, "broadcast", nil); err != nil {
			return "", fmt.Errorf("broadcast to %q failed: %w", name, err)
		}
		count++
	}
	return fmt.Sprintf("Broadcast to %d teammates", count), nil
}

type TeamMember struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	Prompt     string `json:"prompt,omitempty"`
	Supervisor string `json:"supervisor,omitempty"`
	RunID      string `json:"run_id,omitempty"`
}

type TeamConfig struct {
	TeamName string       `json:"team_name"`
	Members  []TeamMember `json:"members"`
}

const (
	teammateRunStatusRunning   = "running"
	teammateRunStatusCompleted = "completed"
	teammateRunStatusFailed    = "failed"
	teammateRunStatusTimedOut  = "timed_out"
)

type TeammateRun struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Role            string  `json:"role"`
	Supervisor      string  `json:"supervisor,omitempty"`
	Prompt          string  `json:"prompt"`
	Status          string  `json:"status"`
	Result          string  `json:"result,omitempty"`
	Error           string  `json:"error,omitempty"`
	FailureKind     string  `json:"failure_kind,omitempty"`
	Retryable       bool    `json:"retryable,omitempty"`
	LastKnownStatus string  `json:"last_known_status,omitempty"`
	StartedAt       float64 `json:"started_at"`
	CompletedAt     float64 `json:"completed_at,omitempty"`
}

type teammateRunner func(context.Context, *Agent, string) error

type idleEvent struct {
	Message    string
	ShouldStop bool
	commit     func() error
	rollback   func() error
}

type TeammateManager struct {
	dir        string
	configPath string
	baseAgent  *Agent
	bus        *MessageBus
	runner     teammateRunner

	idlePollInterval time.Duration
	idleTimeout      time.Duration

	mu      sync.Mutex
	config  TeamConfig
	threads map[string]context.CancelFunc
	runs    map[string]*TeammateRun
	signals map[string]chan struct{}
}

func NewTeammateManager(teamDir string, baseAgent *Agent) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0o755)
	manager := &TeammateManager{
		dir:              teamDir,
		configPath:       filepath.Join(teamDir, "config.json"),
		baseAgent:        baseAgent,
		bus:              NewMessageBus(filepath.Join(filepath.Dir(teamDir), "talk.txt")),
		threads:          make(map[string]context.CancelFunc),
		runs:             make(map[string]*TeammateRun),
		signals:          make(map[string]chan struct{}),
		idlePollInterval: teammateIdlePollInterval,
		idleTimeout:      teammateIdleTimeout,
	}
	manager.config = manager.loadConfig()
	manager.runner = manager.defaultRunner
	return manager
}

func (m *TeammateManager) loadConfig() TeamConfig {
	raw, err := os.ReadFile(m.configPath)
	if err != nil {
		return TeamConfig{TeamName: "default", Members: []TeamMember{}}
	}

	var cfg TeamConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return TeamConfig{TeamName: "default", Members: []TeamMember{}}
	}
	if cfg.TeamName == "" {
		cfg.TeamName = "default"
	}
	if cfg.Members == nil {
		cfg.Members = []TeamMember{}
	}
	for i := range cfg.Members {
		if strings.TrimSpace(cfg.Members[i].Status) == "" {
			cfg.Members[i].Status = teammateStatusIdle
		}
	}
	return cfg
}

func (m *TeammateManager) saveConfigLocked() error {
	raw, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.configPath, raw, 0o644)
}

func (m *TeammateManager) findMemberLocked(name string) *TeamMember {
	for i := range m.config.Members {
		if m.config.Members[i].Name == name {
			return &m.config.Members[i]
		}
	}
	return nil
}

func (m *TeammateManager) Spawn(name, role, taskPrompt, supervisor string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("teammate name is required")
	}
	if strings.TrimSpace(taskPrompt) == "" {
		return "", fmt.Errorf("teammate prompt is required")
	}

	m.mu.Lock()
	if _, running := m.threads[name]; running {
		m.mu.Unlock()
		return "", fmt.Errorf("%q is already running", name)
	}
	member := m.findMemberLocked(name)
	if member != nil {
		if member.Status != teammateStatusIdle && member.Status != teammateStatusShutdown {
			status := member.Status
			m.mu.Unlock()
			return "", fmt.Errorf("%q is currently %s", name, status)
		}
		member.Status = teammateStatusWorking
		member.Role = role
		member.Prompt = taskPrompt
		member.Supervisor = supervisor
	} else {
		m.config.Members = append(m.config.Members, TeamMember{
			Name:       name,
			Role:       role,
			Status:     teammateStatusWorking,
			Prompt:     taskPrompt,
			Supervisor: supervisor,
		})
		member = &m.config.Members[len(m.config.Members)-1]
	}
	run := m.startRunLocked(member, role, taskPrompt, supervisor)
	if err := m.saveConfigLocked(); err != nil {
		delete(m.runs, run.ID)
		delete(m.signals, run.ID)
		if member != nil {
			member.RunID = ""
		}
		m.mu.Unlock()
		return "", fmt.Errorf("save config failed: %w", err)
	}
	m.startThreadLocked(name, role, taskPrompt)
	m.mu.Unlock()

	_ = supervisor
	return fmt.Sprintf("Spawned %q (role: %s, run_id: %s)", name, role, run.ID), nil
}

func (m *TeammateManager) Wake(name string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("teammate name is required")
	}

	m.mu.Lock()
	member := m.findMemberLocked(name)
	if member == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("unknown teammate %q", name)
	}
	if _, running := m.threads[name]; running {
		m.mu.Unlock()
		return fmt.Sprintf("Teammate %q already running", name), nil
	}

	member.Status = teammateStatusWorking
	taskPrompt := "You have new inbox activity. Read your inbox, follow the latest instructions, and use send_message if you need context or need to report results."
	if err := m.saveConfigLocked(); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("save config failed: %w", err)
	}

	role := member.Role
	m.startThreadLocked(name, role, taskPrompt)
	m.mu.Unlock()

	log.Printf("[TeammateManager] Waking teammate: name=%s, role=%s, task_prompt_size=%d", name, role, len(taskPrompt))
	return fmt.Sprintf("Woke %q", name), nil
}

func (m *TeammateManager) startThreadLocked(name, role, taskPrompt string) {
	ctx, cancel := context.WithCancel(context.Background())
	m.threads[name] = cancel
	go m.teammateLoop(ctx, name, role, taskPrompt)
}

func (m *TeammateManager) teammateLoop(ctx context.Context, name, role, taskPrompt string) {
	log.Printf("[TeammateManager] Teammate loop started: name=%s, role=%s", name, role)
	agent := m.cloneAgent(name, role, taskPrompt)
	runErr := m.runner(ctx, agent, taskPrompt)
	if runErr != nil {
		log.Printf("[TeammateManager] Teammate loop ended with error: name=%s, err=%v", name, runErr)
	} else {
		log.Printf("[TeammateManager] Teammate loop ended: name=%s", name)
	}

	var failedRun *TeammateRun
	m.mu.Lock()
	delete(m.threads, name)
	member := m.findMemberLocked(name)
	if member != nil && member.Status != teammateStatusShutdown {
		member.Status = teammateStatusIdle
		runID := member.RunID
		if runID != "" {
			run := m.runs[runID]
			if run != nil && run.Status == teammateRunStatusRunning {
				errMsg := "teammate stopped without explicit completion report"
				if runErr != nil {
					errMsg = runErr.Error()
				}
				failureKind, retryable := classifyTeammateFailure(runErr)
				m.completeRunLocked(runID, teammateRunStatusFailed, "", errMsg, failureKind, retryable)
				failedRun = cloneTeammateRun(run)
			}
		}
		member.RunID = ""
	}
	_ = m.saveConfigLocked()
	m.mu.Unlock()

	if failedRun != nil {
		m.notifySupervisorOfFailure(failedRun)
	}
}

func (m *TeammateManager) WaitUntilIdle(ctx context.Context) error {
	if m == nil {
		return nil
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		working := false
		for _, member := range m.config.Members {
			if member.Status == teammateStatusWorking {
				working = true
				break
			}
		}
		m.mu.Unlock()
		if !working {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *TeammateManager) WaitForRun(ctx context.Context, runID string, timeout time.Duration) (*TeammateRun, error) {
	if m == nil {
		return nil, fmt.Errorf("teammate manager not initialized")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("run_id is required")
	}

	m.mu.Lock()
	run, ok := m.runs[runID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown run_id %q", runID)
	}
	if run.Status != teammateRunStatusRunning {
		snapshot := cloneTeammateRun(run)
		m.mu.Unlock()
		return snapshot, nil
	}
	signal := m.signals[runID]
	m.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer:
		m.mu.Lock()
		defer m.mu.Unlock()
		run = m.runs[runID]
		if run == nil {
			return nil, fmt.Errorf("run %q disappeared", runID)
		}
		snapshot := cloneTeammateRun(run)
		snapshot.LastKnownStatus = snapshot.Status
		snapshot.Status = teammateRunStatusTimedOut
		return snapshot, nil
	case <-signal:
		m.mu.Lock()
		defer m.mu.Unlock()
		run = m.runs[runID]
		if run == nil {
			return nil, fmt.Errorf("run %q disappeared", runID)
		}
		return cloneTeammateRun(run), nil
	}
}

func (m *TeammateManager) startRunLocked(member *TeamMember, role, prompt, supervisor string) *TeammateRun {
	runID := newTeammateRunID()
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	run := &TeammateRun{
		ID:         runID,
		Name:       member.Name,
		Role:       role,
		Supervisor: supervisor,
		Prompt:     prompt,
		Status:     teammateRunStatusRunning,
		StartedAt:  now,
	}
	member.RunID = runID
	m.runs[runID] = run
	m.signals[runID] = make(chan struct{})
	return run
}

func (m *TeammateManager) activeRunIDLocked(name string) string {
	member := m.findMemberLocked(name)
	if member == nil {
		return ""
	}
	return strings.TrimSpace(member.RunID)
}

func (m *TeammateManager) resolveRunIDForSender(sender, explicitRunID string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	runID := strings.TrimSpace(explicitRunID)
	if runID == "" {
		runID = m.activeRunIDLocked(sender)
	}
	if runID == "" {
		return "", nil
	}
	run := m.runs[runID]
	if run == nil {
		return "", fmt.Errorf("unknown run_id %q", runID)
	}
	if run.Name != sender {
		return "", fmt.Errorf("run %q does not belong to %q", runID, sender)
	}
	return runID, nil
}

func (m *TeammateManager) ReportRun(runID, status, result string) error {
	if m == nil {
		return fmt.Errorf("teammate manager not initialized")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !validTeammateRunTerminalStatus(status) {
		return fmt.Errorf("invalid run status %q", status)
	}
	failureKind := ""
	retryable := false
	if status == teammateRunStatusFailed {
		failureKind = teammateFailureKindReported
	}
	if !m.completeRunLocked(runID, status, result, "", failureKind, retryable) {
		return fmt.Errorf("unknown run_id %q", runID)
	}
	return nil
}

func (m *TeammateManager) completeRunLocked(runID, status, result, errMsg, failureKind string, retryable bool) bool {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false
	}
	run := m.runs[runID]
	if run == nil {
		return false
	}
	if run.Status != teammateRunStatusRunning {
		return true
	}
	run.Status = status
	run.Result = result
	run.Error = errMsg
	run.FailureKind = failureKind
	run.Retryable = retryable
	run.LastKnownStatus = ""
	if status == teammateRunStatusCompleted {
		run.Error = ""
		run.FailureKind = ""
		run.Retryable = false
	}
	run.CompletedAt = float64(time.Now().UnixNano()) / float64(time.Second)
	if signal := m.signals[runID]; signal != nil {
		close(signal)
		delete(m.signals, runID)
	}
	return true
}

func (m *TeammateManager) cloneAgent(name, role, prompt string) *Agent {
	base := m.baseAgent
	if base == nil {
		return nil
	}
	_ = prompt

	m.mu.Lock()
	teamName := m.config.TeamName
	supervisor := ""
	if member := m.findMemberLocked(name); member != nil {
		supervisor = strings.TrimSpace(member.Supervisor)
	}
	m.mu.Unlock()

	sysLines := []string{
		fmt.Sprintf("You are %s.", name),
		fmt.Sprintf("Role: %s.", role),
		fmt.Sprintf("Team: %s.", teamName),
		"When you finish a work item, do not silently stop. Use complete_task_and_report when you finish a claimed task, then use idle.",
		"Use idle only after you have finished the current work item or are blocked waiting for new instructions.",
		"You may auto-claim tasks.",
		"When you are woken up, inspect inbox messages and act on them.",
		"After you write or edit code, run check_types on a relevant changed file before you report completion.",
		"If check_types fails, keep fixing the code and rerun it until the relevant checks pass or you can report a concrete toolchain blocker.",
	}
	if supervisor != "" {
		sysLines = append(sysLines, fmt.Sprintf("Supervisor: %s.", supervisor))
		sysLines = append(sysLines, "Use send_message to report intermediate results, blockers, and questions back to your supervisor.")
		sysLines = append(sysLines, "When your assigned task is done, prefer complete_task_and_report instead of manually chaining send_message, task_update, and worktree_remove.")
		sysLines = append(sysLines, "complete_task_and_report removes the worktree by default. Set keep_worktree=true only when the workspace must remain available after completion.")
		sysLines = append(sysLines, "Do not treat a plain assistant final answer as sufficient task or run completion reporting.")
	}
	sysPrompt := strings.Join(sysLines, " ")

	clonedTools := make(map[string]ToolDefinition, len(TEAM_AGENTS_TOOLS))
	for k, v := range base.tools {
		if _, ok := TEAM_AGENTS_TOOLS[k]; ok {
			clonedTools[k] = v
		}
	}

	clonedOrder := make([]string, 0, len(TEAM_AGENTS_TOOLS))
	for _, toolName := range base.order {
		if _, ok := TEAM_AGENTS_TOOLS[toolName]; ok {
			clonedOrder = append(clonedOrder, toolName)
		}
	}

	agent := &Agent{
		Name:            name,
		Description:     role,
		SystemPrompt:    sysPrompt,
		BaseUrl:         base.BaseUrl,
		ApiKey:          base.ApiKey,
		Model:           base.Model,
		WorkDir:         base.WorkDir,
		SubAgents:       base.SubAgents,
		SkillLoader:     base.SkillLoader,
		TaskManager:     base.TaskManager,
		WorktreeManager: base.WorktreeManager,
		Background:      NewBackgroundManager(),
		TeamManager:     m,
		client:          base.client,
		tools:           clonedTools,
		order:           clonedOrder,
	}
	agent.Background.SetDir(agent.WorkDir)
	if _, exists := agent.tools["idle"]; !exists {
		agent.tools["idle"] = idleToolDefinition()
		agent.order = append(agent.order, "idle")
	}
	return agent
}

func (m *TeammateManager) defaultRunner(ctx context.Context, agent *Agent, prompt string) error {
	if agent == nil {
		return fmt.Errorf("agent not initialized")
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(agent.SystemPrompt),
		openai.UserMessage(prompt),
	}

	maxTurns := maxTurnsLimit()
	var pendingIdleEvent *idleEvent
	for turn := 0; turn < maxTurns; {
		nextMessages, enterIdle, err := m.runWorkPhase(ctx, agent, messages, turn)
		if err != nil {
			if pendingIdleEvent != nil {
				_ = pendingIdleEvent.Rollback()
			}
			return err
		}
		if pendingIdleEvent != nil {
			if err := pendingIdleEvent.Commit(); err != nil {
				return err
			}
			pendingIdleEvent = nil
		}
		messages = nextMessages
		turn++
		if !enterIdle {
			continue
		}

		if err := m.setMemberStatus(agent.Name, teammateStatusIdle); err != nil {
			log.Printf("[TeammateManager] Failed to mark teammate idle: name=%s err=%v", agent.Name, err)
		}

		idleEvent, err := m.waitForIdleEvent(ctx, agent)
		if err != nil {
			return err
		}
		if idleEvent == nil {
			return fmt.Errorf("idle event missing")
		}
		if idleEvent.ShouldStop {
			if err := m.setMemberStatus(agent.Name, teammateStatusShutdown); err != nil {
				log.Printf("[TeammateManager] Failed to mark teammate shutdown: name=%s err=%v", agent.Name, err)
			}
			return nil
		}

		messages = append(messages, openai.UserMessage(idleEvent.Message))
		pendingIdleEvent = idleEvent
		if err := m.setMemberStatus(agent.Name, teammateStatusWorking); err != nil {
			log.Printf("[TeammateManager] Failed to mark teammate working: name=%s err=%v", agent.Name, err)
		}
	}

	return fmt.Errorf("max turns reached without shutdown")
}

func (m *TeammateManager) runWorkPhase(ctx context.Context, agent *Agent, messages []openai.ChatCompletionMessageParamUnion, turn int) ([]openai.ChatCompletionMessageParamUnion, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	default:
	}

	var err error
	messages, err = agent.maybeAutoCompact(ctx, messages)
	if err != nil {
		return nil, false, fmt.Errorf("auto compact failed (turn=%d): %w", turn, err)
	}
	messages = agent.injectIdentityBlockIfCompacted(messages)
	turnAcks := &turnEventAcks{}
	messages = agent.stageBackgroundNotifications(messages, turnAcks)
	messages, err = agent.stageTeamInboxMessages(messages, turnAcks)
	if err != nil {
		_ = turnAcks.Rollback()
		return nil, false, fmt.Errorf("stage turn events failed (turn=%d): %w", turn, err)
	}

	resp, err := withOpenAIRateLimitRetry(ctx, "teammate_work_phase", func() (*openai.ChatCompletion, error) {
		return agent.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    agent.Model,
			Messages: messages,
			Tools:    agent.openAITools(),
		})
	})
	if err != nil {
		_ = turnAcks.Rollback()
		return nil, false, fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
	}
	if err := turnAcks.Commit(); err != nil {
		return nil, false, fmt.Errorf("ack turn events failed (turn=%d): %w", turn, err)
	}
	if len(resp.Choices) == 0 {
		return nil, false, fmt.Errorf("empty choices from model")
	}

	choice := resp.Choices[0]
	messages = append(messages, choice.Message.ToParam())

	switch choice.FinishReason {
	case "stop":
		return messages, true, nil
	case "tool_calls":
		idleRequested := false
		for _, tc := range choice.Message.ToolCalls {
			output, callErr := agent.executeTool(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if callErr != nil {
				output = "tool error: " + callErr.Error()
			}
			messages = append(messages, openai.ToolMessage(output, tc.ID))
			if tc.Function.Name == "idle" {
				idleRequested = true
			}
		}
		return messages, idleRequested, nil
	case "network_error":
		return nil, false, fmt.Errorf("model interrupted with finish reason: %s", choice.FinishReason)
	default:
		return nil, false, fmt.Errorf("unsupported finish reason: %s", choice.FinishReason)
	}
}

func (m *TeammateManager) waitForIdleEvent(ctx context.Context, agent *Agent) (*idleEvent, error) {
	deadline := time.Now().Add(m.idleTimeout)
	for {
		event, err := m.nextIdleEventControlled(agent)
		if err != nil {
			return nil, err
		}
		if event != nil {
			return event, nil
		}
		if time.Now().After(deadline) {
			return &idleEvent{ShouldStop: true}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.idlePollInterval):
		}
	}
}

func (m *TeammateManager) nextIdleEvent(agent *Agent) (string, error) {
	event, err := m.nextIdleEventControlled(agent)
	if err != nil {
		return "", err
	}
	if event == nil {
		return "", nil
	}
	if err := event.Commit(); err != nil {
		return "", err
	}
	return event.Message, nil
}

func (m *TeammateManager) nextIdleEventControlled(agent *Agent) (*idleEvent, error) {
	if agent == nil {
		return nil, fmt.Errorf("agent not initialized")
	}

	if inbox, keys, err := m.bus.PeekInbox(agent.Name); err != nil {
		return nil, err
	} else if len(inbox) > 0 {
		return &idleEvent{
			Message: formatInboxMessages(inbox),
			commit: func() error {
				return m.bus.AckInbox(agent.Name, keys)
			},
		}, nil
	}

	if agent.TaskManager == nil {
		return nil, nil
	}

	return m.claimNextTaskEvent(agent, nil)
}

func (m *TeammateManager) claimNextTask(agent *Agent, taskID *int) (string, error) {
	event, err := m.claimNextTaskEvent(agent, taskID)
	if err != nil {
		return "", err
	}
	if event == nil {
		return "", nil
	}
	if err := event.Commit(); err != nil {
		return "", err
	}
	return event.Message, nil
}

func (m *TeammateManager) claimNextTaskEvent(agent *Agent, taskID *int) (*idleEvent, error) {
	if agent == nil {
		return nil, fmt.Errorf("agent not initialized")
	}
	if agent.TaskManager == nil {
		return nil, nil
	}

	var (
		task *Task
		err  error
	)
	if taskID != nil {
		task, err = agent.TaskManager.Claim(*taskID, agent.Name)
	} else {
		task, err = agent.TaskManager.ClaimNextAvailable(agent.Name)
	}
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, nil
	}

	var worktree *Worktree
	if agent.WorktreeManager != nil {
		worktree, err = agent.WorktreeManager.EnsureForTask(task)
		if err != nil {
			_, _ = agent.TaskManager.ResetClaim(task.ID)
			return nil, err
		}
		agent.WorkDir = worktree.Path
		if agent.Background != nil {
			agent.Background.SetDir(worktree.Path)
		}
		if refreshed, loadErr := agent.TaskManager.load(task.ID); loadErr == nil {
			task = refreshed
		}
	}

	return &idleEvent{
		Message: formatClaimedTask(task, worktree),
		rollback: func() error {
			_, err := agent.TaskManager.ResetClaim(task.ID)
			return err
		},
	}, nil
}

func (m *TeammateManager) setMemberStatus(name, status string) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	member := m.findMemberLocked(name)
	if member == nil {
		return fmt.Errorf("unknown teammate %q", name)
	}
	member.Status = status
	return m.saveConfigLocked()
}

func (m *TeammateManager) isManagedTeammate(name string) bool {
	if m == nil || strings.TrimSpace(name) == "" {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.findMemberLocked(name) != nil
}

func (m *TeammateManager) knowsParticipant(name string) bool {
	if m == nil || strings.TrimSpace(name) == "" {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.baseAgent != nil && strings.TrimSpace(m.baseAgent.Name) == name {
		return true
	}
	return m.findMemberLocked(name) != nil
}

func (m *TeammateManager) supervisorFor(name string) string {
	if m == nil || strings.TrimSpace(name) == "" {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	member := m.findMemberLocked(name)
	if member == nil {
		return ""
	}
	return strings.TrimSpace(member.Supervisor)
}

func (m *TeammateManager) notifySupervisorOfFailure(run *TeammateRun) {
	if m == nil || m.bus == nil || run == nil || strings.TrimSpace(run.Supervisor) == "" {
		return
	}
	if !m.knowsParticipant(run.Supervisor) {
		return
	}

	metadata := map[string]any{
		"run_id":       run.ID,
		"status":       teammateRunStatusFailed,
		"failure_kind": run.FailureKind,
		"retryable":    run.Retryable,
	}
	content := fmt.Sprintf(
		"Teammate %s failed.\nrole=%s\nrun_id=%s\nfailure_kind=%s\nretryable=%t\nerror=%s",
		run.Name,
		run.Role,
		run.ID,
		run.FailureKind,
		run.Retryable,
		strings.TrimSpace(run.Error),
	)
	if _, err := m.bus.Send(run.Name, run.Supervisor, content, "message", metadata); err != nil {
		log.Printf("[TeammateManager] failed to notify supervisor %s about run %s: %v", run.Supervisor, run.ID, err)
		return
	}
	if m.isManagedTeammate(run.Supervisor) {
		if wakeResult, err := m.Wake(run.Supervisor); err != nil {
			log.Printf("[TeammateManager] failed to wake supervisor %s after run %s failure: %v", run.Supervisor, run.ID, err)
		} else {
			log.Printf("[TeammateManager] Wake result for supervisor %s after run %s failure: %s", run.Supervisor, run.ID, wakeResult)
		}
	}
}

func (m *TeammateManager) ListAll() string {
	if m == nil {
		return "No teammates."
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.config.Members) == 0 {
		return "No teammates."
	}

	lines := []string{fmt.Sprintf("Team: %s", m.config.TeamName)}
	for _, member := range m.config.Members {
		runInfo := ""
		if strings.TrimSpace(member.RunID) != "" {
			runInfo = fmt.Sprintf(", run_id=%s", member.RunID)
		}
		lines = append(lines, fmt.Sprintf("  %s (%s): %s%s", member.Name, member.Role, member.Status, runInfo))
	}
	return strings.Join(lines, "\n")
}

func (m *TeammateManager) MemberNames() []string {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.config.Members))
	for _, member := range m.config.Members {
		names = append(names, member.Name)
	}
	return names
}

func formatInboxMessages(messages []TeamMessage) string {
	lines := []string{"<inbox>"}
	for _, msg := range messages {
		lines = append(lines, fmt.Sprintf("[%s] from=%s at=%s", msg.Type, msg.From, time.Unix(int64(msg.Timestamp), 0).Format(time.RFC3339)))
		if metadata := formatTeamMessageMetadata(msg.Metadata); metadata != "" {
			lines = append(lines, metadata)
		}
		lines = append(lines, msg.Content)
	}
	lines = append(lines, "</inbox>")
	return strings.Join(lines, "\n")
}

func newTeamMessageID() string {
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
}

func newTeammateRunID() string {
	return fmt.Sprintf("run_%d", time.Now().UnixNano())
}

func cloneTeamMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for k, v := range metadata {
		cloned[k] = v
	}
	return cloned
}

func cloneTeammateRun(run *TeammateRun) *TeammateRun {
	if run == nil {
		return nil
	}
	cloned := *run
	return &cloned
}

func classifyTeammateFailure(err error) (string, bool) {
	if err == nil {
		return teammateFailureKindProtocol, false
	}
	if errors.Is(err, context.Canceled) {
		return teammateFailureKindCancelled, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return teammateFailureKindTimeout, true
	}
	if isOpenAITransientError(nil, err) {
		return teammateFailureKindNetwork, true
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, "without explicit completion report"):
		return teammateFailureKindProtocol, false
	case strings.Contains(msg, "network_error"),
		strings.Contains(msg, "chat completion failed"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "unexpected eof"):
		return teammateFailureKindNetwork, true
	default:
		return teammateFailureKindInternal, false
	}
}

func validTeammateRunTerminalStatus(status string) bool {
	switch status {
	case teammateRunStatusCompleted, teammateRunStatusFailed:
		return true
	default:
		return false
	}
}

func formatTeamMessageMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}

	parts := make([]string, 0, 2)
	if runID, _ := metadata["run_id"].(string); strings.TrimSpace(runID) != "" {
		parts = append(parts, "run_id="+runID)
	}
	if status, _ := metadata["status"].(string); strings.TrimSpace(status) != "" {
		parts = append(parts, "status="+status)
	}
	if failureKind, _ := metadata["failure_kind"].(string); strings.TrimSpace(failureKind) != "" {
		parts = append(parts, "failure_kind="+failureKind)
	}
	if retryable, ok := metadata["retryable"].(bool); ok {
		parts = append(parts, fmt.Sprintf("retryable=%t", retryable))
	}
	if taskID, ok := metadata["task_id"]; ok {
		parts = append(parts, fmt.Sprintf("task_id=%v", taskID))
	}
	if len(parts) == 0 {
		return ""
	}
	return "metadata: " + strings.Join(parts, " ")
}

func resolveTaskForCompletion(agent *Agent, taskID *int) (*Task, error) {
	if agent == nil || agent.TaskManager == nil {
		return nil, fmt.Errorf("task manager not initialized")
	}

	if taskID != nil {
		task, err := agent.TaskManager.load(*taskID)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(agent.Name) != "" && task.Owner != "" && task.Owner != agent.Name {
			return nil, fmt.Errorf("task %d is owned by %s, not %s", task.ID, task.Owner, agent.Name)
		}
		return task, nil
	}

	tasks, err := agent.TaskManager.Snapshot()
	if err != nil {
		return nil, err
	}

	candidates := make([]*Task, 0)
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if task.Owner != agent.Name || task.Status != taskStatusInProgress {
			continue
		}
		candidates = append(candidates, task)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no in_progress task owned by %s", agent.Name)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	for _, task := range candidates {
		if taskMatchesAgentWorkdir(agent, task) {
			return task, nil
		}
	}
	return nil, fmt.Errorf("multiple in_progress tasks owned by %s; provide task_id", agent.Name)
}

func taskMatchesAgentWorkdir(agent *Agent, task *Task) bool {
	if agent == nil || task == nil {
		return false
	}
	wd := filepath.Clean(strings.TrimSpace(agent.WorkDir))
	if wd == "." || wd == "" || strings.TrimSpace(task.Worktree) == "" {
		return false
	}
	if filepath.Base(wd) == task.Worktree {
		return true
	}
	if agent.WorktreeManager == nil {
		return false
	}
	expected := filepath.Clean(filepath.Join(agent.WorktreeManager.rootDir, task.Worktree))
	return expected == wd
}

func formatClaimedTask(task *Task, worktree *Worktree) string {
	if task == nil {
		return ""
	}

	payload := map[string]any{
		"task": task,
	}
	if worktree != nil {
		payload["worktree"] = worktree
		payload["cwd"] = worktree.Path
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		data = []byte(fmt.Sprintf(`{"task_id": %d}`, task.ID))
	}

	return strings.Join([]string{
		"<task_claim>",
		string(data),
		"</task_claim>",
	}, "\n")
}

func validMessageTypeList() []string {
	out := make([]string, 0, len(validMessageTypes))
	for k := range validMessageTypes {
		out = append(out, k)
	}
	return out
}

func registerTeamTools(toolMap map[string]ToolDefinition, order []string) []string {
	if toolMap == nil {
		return order
	}

	teamTools := []ToolDefinition{
		{
			Name:        "spawn_teammate",
			Description: "Spawn or restart a persistent teammate agent. Returns a run_id in the tool result. If you need that teammate's output before continuing, call wait_teammate with the returned run_id.",
			Parameters: ObjectSchema(map[string]any{
				"name":        StringParam(),
				"role":        StringParam(),
				"task_prompt": StringParam(),
			}, "name", "role", "task_prompt"),
			Handler: spawnTeammateTool,
		},
		{
			Name:        "wait_teammate",
			Description: "Block until the specified teammate run_id reports completed or failed. Use this after spawn_teammate when your next step depends on that run's result.",
			Parameters: ObjectSchema(map[string]any{
				"run_id":          StringParam(),
				"timeout_seconds": IntegerParam(),
			}, "run_id"),
			Handler: waitTeammateTool,
		},
		{
			Name:        "claim_task",
			Description: "Actively claim a runnable task from the task board. Optionally provide task_id to claim a specific task.",
			Parameters: ObjectSchema(map[string]any{
				"task_id": IntegerParam(),
			}),
			Handler: claimTaskTool,
		},
		{
			Name:        "complete_task_and_report",
			Description: "Atomically finish a claimed task: send the final report, mark the task completed, and by default remove its worktree. Set keep_worktree=true only when the workspace should remain available after completion.",
			Parameters: ObjectSchema(map[string]any{
				"content":          StringParam(),
				"to":               StringParam(),
				"task_id":          IntegerParam(),
				"keep_worktree":    BoolParam(),
				"cleanup_worktree": BoolParam(),
				"force":            BoolParam(),
			}, "content"),
			Handler: completeTaskAndReportTool,
		},
		{
			Name:        "send_message",
			Description: "Send a direct message to a teammate inbox. When a teammate reports final run outcome to its supervisor, include status=completed or failed. run_id may be omitted if the sender has exactly one active assigned run.",
			Parameters: ObjectSchema(map[string]any{
				"to":      StringParam(),
				"content": StringParam(),
				"run_id":  StringParam(),
				"status":  StringParam(),
			}, "to", "content"),
			Handler: sendMessageTool,
		},
		{
			Name:        "read_inbox",
			Description: "Read and drain your inbox. Use this to inspect detailed teammate reports after send_message or after wait_teammate if you need the full message text.",
			Parameters:  ObjectSchema(map[string]any{}),
			Handler:     readInboxTool,
		},
		{
			Name:        "broadcast_message",
			Description: "Broadcast a message to all teammates except yourself.",
			Parameters: ObjectSchema(map[string]any{
				"content": StringParam(),
			}, "content"),
			Handler: broadcastMessageTool,
		},
		{
			Name:        "list_team",
			Description: "List known teammates and their status.",
			Parameters:  ObjectSchema(map[string]any{}),
			Handler:     listTeamTool,
		},
	}

	for _, tool := range teamTools {
		if _, exists := toolMap[tool.Name]; exists {
			continue
		}
		toolMap[tool.Name] = tool
		order = append(order, tool.Name)
	}
	return order
}

func spawnTeammateTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}

	var params struct {
		Name       string `json:"name"`
		Role       string `json:"role"`
		TaskPrompt string `json:"task_prompt"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		log.Printf("[SpawnTeammateTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid spawn_teammate args: %w", err)
	}
	log.Printf("[SpawnTeammateTool] agent=%s Spawning teammate: name=%s, role=%s, prompt_size=%d", agentLogName(agent), params.Name, params.Role, len(params.TaskPrompt))
	result, err := agent.TeamManager.Spawn(params.Name, params.Role, params.TaskPrompt, agent.Name)
	if err != nil {
		return "", err
	}
	log.Printf("[SpawnTeammateTool] agent=%s Spawn completed: %s", agentLogName(agent), result)
	return result, nil
}

func waitTeammateTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}

	var params struct {
		RunID          string `json:"run_id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid wait_teammate args: %w", err)
	}

	timeout := 2 * time.Minute
	if params.TimeoutSeconds > 0 {
		timeout = time.Duration(params.TimeoutSeconds) * time.Second
	}

	run, err := agent.TeamManager.WaitForRun(ctx, params.RunID, timeout)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func marshalSendOutcome(message string, extras map[string]any, warnings []string) (string, error) {
	result := map[string]any{
		"message": message,
	}
	for key, value := range extras {
		if value == nil {
			continue
		}
		result[key] = value
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func completeTaskAndReportTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil || agent.TeamManager.bus == nil {
		return "", fmt.Errorf("message bus not initialized")
	}
	if agent.TaskManager == nil {
		return "", fmt.Errorf("task manager not initialized")
	}

	var params struct {
		Content         string `json:"content"`
		To              string `json:"to"`
		TaskID          *int   `json:"task_id"`
		KeepWorktree    *bool  `json:"keep_worktree"`
		CleanupWorktree *bool  `json:"cleanup_worktree"`
		Force           bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid complete_task_and_report args: %w", err)
	}
	if strings.TrimSpace(params.Content) == "" {
		return "", fmt.Errorf("complete_task_and_report content is required")
	}

	keepWorktree := false
	if params.KeepWorktree != nil {
		keepWorktree = *params.KeepWorktree
	} else if params.CleanupWorktree != nil {
		keepWorktree = !*params.CleanupWorktree
	}

	task, err := resolveTaskForCompletion(agent, params.TaskID)
	if err != nil {
		return "", err
	}

	var worktreeRecord *Worktree
	if !keepWorktree && strings.TrimSpace(task.Worktree) != "" {
		if agent.WorktreeManager == nil {
			return "", fmt.Errorf("worktree manager not initialized")
		}
		worktreeRecord, err = agent.WorktreeManager.Remove(task.Worktree, params.Force, true)
		if err != nil {
			return "", err
		}
	} else {
		if _, err := agent.TaskManager.Update(task.ID, taskStatusCompleted, nil, nil); err != nil {
			return "", err
		}
	}

	finalTask, err := agent.TaskManager.load(task.ID)
	if err != nil {
		return "", err
	}

	to := strings.TrimSpace(params.To)
	if to == "" {
		to = agent.TeamManager.supervisorFor(agent.Name)
	}
	if to == "" {
		return "", fmt.Errorf("complete_task_and_report requires a recipient")
	}
	if !agent.TeamManager.knowsParticipant(to) {
		return "", fmt.Errorf("unknown team participant %q", to)
	}

	metadata := map[string]any{
		"status":  teammateRunStatusCompleted,
		"task_id": finalTask.ID,
	}
	runID, err := agent.TeamManager.resolveRunIDForSender(agent.Name, "")
	if err != nil {
		return "", err
	}
	if runID != "" {
		metadata["run_id"] = runID
	}

	log.Printf("[CompleteTaskAndReportTool] agent=%s Completing task_id=%d keep_worktree=%t", agentLogName(agent), finalTask.ID, keepWorktree)
	sendResult, err := agent.TeamManager.bus.Send(agent.Name, to, params.Content, "message", metadata)
	if err != nil {
		return "", err
	}
	warnings := make([]string, 0, 2)
	wakeResult := ""
	if runID != "" {
		if err := agent.TeamManager.ReportRun(runID, teammateRunStatusCompleted, params.Content); err != nil {
			warnings = append(warnings, fmt.Sprintf("run status not recorded after send: %v", err))
			log.Printf("[CompleteTaskAndReportTool] agent=%s report run warning for %s: %v", agentLogName(agent), runID, err)
		}
	}
	if agent.TeamManager.isManagedTeammate(to) {
		wakeResult, err = agent.TeamManager.Wake(to)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("recipient wake failed after send: %v", err))
			log.Printf("[CompleteTaskAndReportTool] agent=%s wake warning for %s: %v", agentLogName(agent), to, err)
		} else {
			log.Printf("[CompleteTaskAndReportTool] agent=%s Wake result for %s: %s", agentLogName(agent), to, wakeResult)
		}
	}

	result := map[string]any{
		"message": sendResult,
		"task":    finalTask,
	}
	if runID != "" {
		result["run_id"] = runID
	}
	if worktreeRecord != nil {
		result["worktree"] = worktreeRecord
	}
	if strings.TrimSpace(wakeResult) != "" {
		result["wake_result"] = wakeResult
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func sendMessageTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil || agent.TeamManager.bus == nil {
		return "", fmt.Errorf("message bus not initialized")
	}

	var params struct {
		To      string `json:"to"`
		Content string `json:"content"`
		RunID   string `json:"run_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		log.Printf("[SendMessageTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid send_message args: %w", err)
	}
	if !agent.TeamManager.knowsParticipant(params.To) {
		return "", fmt.Errorf("unknown team participant %q", params.To)
	}

	metadata := map[string]any{}
	runID, err := agent.TeamManager.resolveRunIDForSender(agent.Name, params.RunID)
	if err != nil {
		return "", err
	}
	if runID != "" {
		metadata["run_id"] = runID
	}
	status := strings.TrimSpace(params.Status)
	if status != "" {
		if !validTeammateRunTerminalStatus(status) {
			return "", fmt.Errorf("invalid send_message status %q", status)
		}
		if runID == "" {
			return "", fmt.Errorf("send_message status requires an active run_id")
		}
		metadata["status"] = status
	}
	if len(metadata) == 0 {
		metadata = nil
	}

	log.Printf("[SendMessageTool] agent=%s Sending message: from=%s, to=%s, content_size=%d", agentLogName(agent), agent.Name, params.To, len(params.Content))
	result, err := agent.TeamManager.bus.Send(agent.Name, params.To, params.Content, "message", metadata)
	if err != nil {
		return "", err
	}
	warnings := make([]string, 0, 2)
	wakeResult := ""
	if status != "" {
		if err := agent.TeamManager.ReportRun(runID, status, params.Content); err != nil {
			warnings = append(warnings, fmt.Sprintf("run status not recorded after send: %v", err))
			log.Printf("[SendMessageTool] agent=%s report run warning for %s: %v", agentLogName(agent), runID, err)
		}
	}
	if agent.TeamManager.isManagedTeammate(params.To) {
		wakeResult, err = agent.TeamManager.Wake(params.To)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("recipient wake failed after send: %v", err))
			log.Printf("[SendMessageTool] agent=%s wake warning for %s: %v", agentLogName(agent), params.To, err)
		} else {
			log.Printf("[SendMessageTool] agent=%s Wake result for %s: %s", agentLogName(agent), params.To, wakeResult)
		}
	}
	log.Printf("[SendMessageTool] agent=%s Send completed: %s", agentLogName(agent), result)
	extras := map[string]any{}
	if runID != "" {
		extras["run_id"] = runID
	}
	if strings.TrimSpace(status) != "" {
		extras["status"] = status
	}
	if strings.TrimSpace(wakeResult) != "" {
		extras["wake_result"] = wakeResult
	}
	return marshalSendOutcome(result, extras, warnings)
}

func claimTaskTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}

	var params struct {
		TaskID *int `json:"task_id"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid claim_task args: %w", err)
		}
	}

	if params.TaskID != nil {
		log.Printf("[ClaimTaskTool] agent=%s Claiming task_id=%d", agentLogName(agent), *params.TaskID)
	} else {
		log.Printf("[ClaimTaskTool] agent=%s Claiming next task", agentLogName(agent))
	}

	result, err := agent.TeamManager.claimNextTask(agent, params.TaskID)
	if err != nil {
		log.Printf("[ClaimTaskTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	if result == "" {
		result = "No claimable task."
	}
	log.Printf("[ClaimTaskTool] agent=%s Claim result (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
	return result, nil
}

func readInboxTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil || agent.TeamManager.bus == nil {
		return "", fmt.Errorf("message bus not initialized")
	}
	log.Printf("[ReadInboxTool] agent=%s Reading inbox", agentLogName(agent))
	messages := agent.TeamManager.bus.ReadInbox(agent.Name)
	raw, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		log.Printf("[ReadInboxTool] agent=%s Error: %v", agentLogName(agent), err)
		return "", err
	}
	result := string(raw)
	log.Printf("[ReadInboxTool] agent=%s Inbox read completed (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
	return result, nil
}

func broadcastMessageTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil || agent.TeamManager.bus == nil {
		return "", fmt.Errorf("message bus not initialized")
	}

	var params struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		log.Printf("[BroadcastMessageTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid broadcast_message args: %w", err)
	}
	log.Printf("[BroadcastMessageTool] agent=%s Broadcasting message: from=%s, content_size=%d", agentLogName(agent), agent.Name, len(params.Content))
	names := agent.TeamManager.MemberNames()
	result, err := agent.TeamManager.bus.Broadcast(agent.Name, params.Content, names)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if name == "" || name == agent.Name {
			continue
		}
		wakeResult, err := agent.TeamManager.Wake(name)
		if err != nil {
			return "", err
		}
		log.Printf("[BroadcastMessageTool] agent=%s Wake result for %s: %s", agentLogName(agent), name, wakeResult)
	}
	log.Printf("[BroadcastMessageTool] agent=%s Broadcast completed: %s", agentLogName(agent), result)
	return result, nil
}

func listTeamTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil {
		return "", fmt.Errorf("teammate manager not initialized")
	}
	log.Printf("[ListTeamTool] agent=%s Listing team", agentLogName(agent))
	result := agent.TeamManager.ListAll()
	log.Printf("[ListTeamTool] agent=%s Team list completed (first 20 chars): %s", agentLogName(agent), truncate(result, 20))
	return result, nil
}

func (e *idleEvent) Commit() error {
	if e == nil || e.commit == nil {
		return nil
	}
	return e.commit()
}

func (e *idleEvent) Rollback() error {
	if e == nil || e.rollback == nil {
		return nil
	}
	return e.rollback()
}
