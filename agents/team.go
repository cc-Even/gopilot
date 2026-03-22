package agents

import (
	"context"
	"encoding/json"
	"fmt"
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
)

var validMessageTypes = map[string]struct{}{
	"message":   {},
	"broadcast": {},
}

type TeamMessage struct {
	Type      string  `json:"type"`
	From      string  `json:"from"`
	Content   string  `json:"content"`
	Timestamp float64 `json:"timestamp"`
}

type MessageBus struct {
	mu      sync.RWMutex
	inboxes map[string]chan TeamMessage
	buffer  int
	logMu   sync.Mutex
	logPath string
}

func NewMessageBus(logPath string) *MessageBus {
	if strings.TrimSpace(logPath) == "" {
		logPath = TALK_LOG_PATH
	}
	return &MessageBus{
		inboxes: make(map[string]chan TeamMessage),
		buffer:  64,
		logPath: logPath,
	}
}

func (b *MessageBus) inbox(name string) chan TeamMessage {
	b.mu.RLock()
	inbox := b.inboxes[name]
	b.mu.RUnlock()
	if inbox != nil {
		return inbox
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if inbox = b.inboxes[name]; inbox == nil {
		inbox = make(chan TeamMessage, b.buffer)
		b.inboxes[name] = inbox
	}
	return inbox
}

func (b *MessageBus) peekInbox(name string) chan TeamMessage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.inboxes[name]
}

func (b *MessageBus) Send(sender, to, content, msgType string, extra map[string]any) string {
	if b == nil {
		return "Error: message bus not initialized"
	}
	if msgType == "" {
		msgType = "message"
	}
	if _, ok := validMessageTypes[msgType]; !ok {
		return fmt.Sprintf("Error: Invalid type %q. Valid: %s", msgType, strings.Join(validMessageTypeList(), ", "))
	}
	_ = extra

	now := time.Now()
	msg := TeamMessage{
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(now.UnixNano()) / float64(time.Second),
	}

	inbox := b.inbox(to)
	select {
	case inbox <- msg:
		if err := b.appendTalkLog(now, sender, to, content); err != nil {
			log.Printf("[MessageBus] failed to append talk log: %v", err)
		}
		return fmt.Sprintf("Sent %s to %s", msgType, to)
	default:
		return fmt.Sprintf("Error: inbox for %q is full", to)
	}
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

	file, err := os.OpenFile(b.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	line := fmt.Sprintf("%s\tfrom=%s\tto=%s\tcontent=%s\n", ts.Format(time.RFC3339Nano), sender, receiver, string(encodedContent))
	_, err = file.WriteString(line)
	return err
}

func (b *MessageBus) ReadInbox(name string) []TeamMessage {
	if b == nil {
		return nil
	}

	inbox := b.peekInbox(name)
	if inbox == nil {
		return nil
	}

	messages := make([]TeamMessage, 0)
	for {
		select {
		case msg := <-inbox:
			messages = append(messages, msg)
		default:
			return messages
		}
	}
}

func (b *MessageBus) Broadcast(sender, content string, teammates []string) string {
	if b == nil {
		return "Error: message bus not initialized"
	}
	count := 0
	for _, name := range teammates {
		if name == "" || name == sender {
			continue
		}
		result := b.Send(sender, name, content, "broadcast", nil)
		if strings.HasPrefix(result, "Sent ") {
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count)
}

type TeamMember struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
	Prompt string `json:"prompt,omitempty"`
}

type TeamConfig struct {
	TeamName string       `json:"team_name"`
	Members  []TeamMember `json:"members"`
}

type teammateRunner func(context.Context, *Agent, string) error

type TeammateManager struct {
	dir        string
	configPath string
	baseAgent  *Agent
	bus        *MessageBus
	runner     teammateRunner

	mu      sync.Mutex
	config  TeamConfig
	threads map[string]context.CancelFunc
}

func NewTeammateManager(teamDir string, baseAgent *Agent) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0o755)
	manager := &TeammateManager{
		dir:        teamDir,
		configPath: filepath.Join(teamDir, "config.json"),
		baseAgent:  baseAgent,
		bus:        NewMessageBus(filepath.Join(filepath.Dir(teamDir), "talk.txt")),
		threads:    make(map[string]context.CancelFunc),
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

func (m *TeammateManager) Spawn(name, role, prompt, supervisor string) string {
	if m == nil {
		return "Error: teammate manager not initialized"
	}
	if strings.TrimSpace(name) == "" {
		return "Error: teammate name is required"
	}
	if strings.TrimSpace(prompt) == "" {
		return "Error: teammate prompt is required"
	}
	if len(supervisor) > 0 {
		prompt += "your supervisor is `" + supervisor + "`. "
	}

	m.mu.Lock()
	member := m.findMemberLocked(name)
	if member != nil {
		if member.Status != teammateStatusIdle && member.Status != teammateStatusShutdown {
			status := member.Status
			m.mu.Unlock()
			return fmt.Sprintf("Error: %q is currently %s", name, status)
		}
		member.Status = teammateStatusIdle
		member.Role = role
		member.Prompt = prompt
	} else {
		m.config.Members = append(m.config.Members, TeamMember{
			Name:   name,
			Role:   role,
			Status: teammateStatusIdle,
			Prompt: prompt,
		})
	}
	if err := m.saveConfigLocked(); err != nil {
		m.mu.Unlock()
		return fmt.Sprintf("Error: save config failed: %v", err)
	}
	m.mu.Unlock()

	return fmt.Sprintf("Spawned %q (role: %s)", name, role)
}

func (m *TeammateManager) Wake(name string) string {
	if m == nil {
		return "Error: teammate manager not initialized"
	}
	if strings.TrimSpace(name) == "" {
		return "Error: teammate name is required"
	}

	m.mu.Lock()
	member := m.findMemberLocked(name)
	if member == nil {
		m.mu.Unlock()
		return fmt.Sprintf("Error: unknown teammate %q", name)
	}
	if _, running := m.threads[name]; running {
		m.mu.Unlock()
		return fmt.Sprintf("Teammate %q already running", name)
	}

	member.Status = teammateStatusWorking
	if err := m.saveConfigLocked(); err != nil {
		m.mu.Unlock()
		return fmt.Sprintf("Error: save config failed: %v", err)
	}

	role := member.Role
	originalPrompt := member.Prompt
	ctx, cancel := context.WithCancel(context.Background())
	m.threads[name] = cancel
	m.mu.Unlock()

	log.Printf("[TeammateManager] Waking teammate: name=%s, role=%s, original_prompt_size=%d", name, role, len(originalPrompt))
	go m.teammateLoop(ctx, name, role, originalPrompt)
	return fmt.Sprintf("Woke %q", name)
}

func (m *TeammateManager) teammateLoop(ctx context.Context, name, role, prompt string) {
	log.Printf("[TeammateManager] Teammate loop started: name=%s, role=%s", name, role)
	agent := m.cloneAgent(name, role, prompt)
	runErr := m.runner(ctx, agent, prompt)
	if runErr != nil {
		log.Printf("[TeammateManager] Teammate loop ended with error: name=%s, err=%v", name, runErr)
	} else {
		log.Printf("[TeammateManager] Teammate loop ended: name=%s", name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.threads, name)
	member := m.findMemberLocked(name)
	if member != nil && member.Status != teammateStatusShutdown {
		member.Status = teammateStatusIdle
		if runErr != nil {
			member.Status = teammateStatusIdle
		}
	}
	_ = m.saveConfigLocked()
}

func (m *TeammateManager) WaitUntilIdle(ctx context.Context) error {
	if m == nil {
		return nil
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		running := len(m.threads)
		m.mu.Unlock()
		if running == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *TeammateManager) cloneAgent(name, role, prompt string) *Agent {
	base := m.baseAgent
	if base == nil {
		return nil
	}

	clonedTools := make(map[string]ToolDefinition, len(TEAM_AGENTS_TOOLS))
	for k, v := range base.tools {
		if _, ok := TEAM_AGENTS_TOOLS[k]; ok {
			clonedTools[k] = v
		}
	}

	toolIdx := 0
	clonedOrder := make([]string, len(TEAM_AGENTS_TOOLS))
	for _, toolName := range base.order {
		if _, ok := TEAM_AGENTS_TOOLS[toolName]; ok {
			clonedOrder[toolIdx] = toolName
			toolIdx++
		}
	}

	return &Agent{
		Name:         name,
		Description:  role,
		SystemPrompt: prompt,
		BaseUrl:      base.BaseUrl,
		ApiKey:       base.ApiKey,
		Model:        base.Model,
		SubAgents:    base.SubAgents,
		SkillLoader:  base.SkillLoader,
		TaskManager:  base.TaskManager,
		Background:   NewBackgroundManager(),
		TeamManager:  m,
		client:       base.client,
		tools:        clonedTools,
		order:        clonedOrder,
	}
}

func (m *TeammateManager) defaultRunner(ctx context.Context, agent *Agent, prompt string) error {
	if agent == nil {
		return fmt.Errorf("agent not initialized")
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(agent.SystemPrompt),
		openai.UserMessage(prompt),
	}

	const maxTurns = 50
	for turn := 0; turn < maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		messages = compactToolMessages(messages)
		var err error
		messages, err = agent.maybeAutoCompact(ctx, messages)
		if err != nil {
			return fmt.Errorf("auto compact failed (turn=%d): %w", turn, err)
		}
		messages = agent.appendBackgroundNotifications(messages)
		if inbox := m.bus.ReadInbox(agent.Name); len(inbox) > 0 {
			messages = append(messages, openai.UserMessage(formatInboxMessages(inbox)))
		}

		resp, err := agent.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    agent.Model,
			Messages: messages,
			Tools:    agent.openAITools(),
		})
		if err != nil {
			return fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
		}
		if len(resp.Choices) == 0 {
			return fmt.Errorf("empty choices from model")
		}

		choice := resp.Choices[0]
		messages = append(messages, choice.Message.ToParam())

		switch choice.FinishReason {
		case "stop":
			return nil
		case "tool_calls":
			for _, tc := range choice.Message.ToolCalls {
				output, callErr := agent.executeTool(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
				if callErr != nil {
					output = "tool error: " + callErr.Error()
				}
				messages = append(messages, openai.ToolMessage(output, tc.ID))
			}
		default:
			return fmt.Errorf("unsupported finish reason: %s", choice.FinishReason)
		}
	}

	return fmt.Errorf("max turns reached without final answer")
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
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", member.Name, member.Role, member.Status))
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
		lines = append(lines, msg.Content)
	}
	lines = append(lines, "</inbox>")
	return strings.Join(lines, "\n")
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
			Description: "Spawn or restart a persistent teammate agent. The teammate continues in the background.",
			Parameters: ObjectSchema(map[string]any{
				"name":   StringParam(),
				"role":   StringParam(),
				"prompt": StringParam(),
			}, "name", "role", "prompt"),
			Handler: spawnTeammateTool,
		},
		{
			Name:        "send_message",
			Description: "Send a direct message to a teammate inbox.",
			Parameters: ObjectSchema(map[string]any{
				"to":      StringParam(),
				"content": StringParam(),
			}, "to", "content"),
			Handler: sendMessageTool,
		},
		{
			Name:        "read_inbox",
			Description: "Read and drain your inbox.",
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
		Name   string `json:"name"`
		Role   string `json:"role"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		log.Printf("[SpawnTeammateTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid spawn_teammate args: %w", err)
	}
	log.Printf("[SpawnTeammateTool] agent=%s Spawning teammate: name=%s, role=%s, prompt_size=%d", agentLogName(agent), params.Name, params.Role, len(params.Prompt))
	result := agent.TeamManager.Spawn(params.Name, params.Role, params.Prompt, agent.Name)
	log.Printf("[SpawnTeammateTool] agent=%s Spawn completed: %s", agentLogName(agent), result)
	return result, nil
}

func sendMessageTool(ctx context.Context, args json.RawMessage, agent *Agent) (string, error) {
	if agent == nil || agent.TeamManager == nil || agent.TeamManager.bus == nil {
		return "", fmt.Errorf("message bus not initialized")
	}

	var params struct {
		To      string `json:"to"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		log.Printf("[SendMessageTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid send_message args: %w", err)
	}
	log.Printf("[SendMessageTool] agent=%s Sending message: from=%s, to=%s, content_size=%d", agentLogName(agent), agent.Name, params.To, len(params.Content))
	result := agent.TeamManager.bus.Send(agent.Name, params.To, params.Content, "message", nil)
	if !strings.HasPrefix(result, "Error:") {
		wakeResult := agent.TeamManager.Wake(params.To)
		log.Printf("[SendMessageTool] agent=%s Wake result for %s: %s", agentLogName(agent), params.To, wakeResult)
	}
	log.Printf("[SendMessageTool] agent=%s Send completed: %s", agentLogName(agent), result)
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
	log.Printf("[ReadInboxTool] agent=%s Inbox read completed (first 200 chars): %s", agentLogName(agent), truncate(result, 200))
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
	result := agent.TeamManager.bus.Broadcast(agent.Name, params.Content, names)
	for _, name := range names {
		if name == "" || name == agent.Name {
			continue
		}
		wakeResult := agent.TeamManager.Wake(name)
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
	log.Printf("[ListTeamTool] agent=%s Team list completed (first 200 chars): %s", agentLogName(agent), truncate(result, 200))
	return result, nil
}
