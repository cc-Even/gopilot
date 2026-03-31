package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
)

func marshalSubagentOutcome(name, status, result string, err error) (string, error) {
	outcome := map[string]any{
		"sub_agent_name": strings.TrimSpace(name),
		"status":         strings.TrimSpace(status),
		"retryable":      false,
	}
	if trimmed := strings.TrimSpace(result); trimmed != "" {
		outcome["result"] = trimmed
	}
	if err != nil {
		outcome["error"] = err.Error()
		outcome["retryable"] = isOpenAITransientError(nil, err)
	}
	data, marshalErr := json.MarshalIndent(outcome, "", "  ")
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(data), nil
}

func resolveSubAgent(subAgents map[string]*Agent, requested string) (string, *Agent, bool) {
	if len(subAgents) == 0 {
		return "", nil, false
	}

	name := strings.TrimSpace(requested)
	if name == "" {
		return "", nil, false
	}
	if subAgent, ok := subAgents[name]; ok {
		return name, subAgent, true
	}

	target := normalizeSubAgentLookupKey(name)
	if target == "" {
		return "", nil, false
	}

	names := availableSubAgentNames(subAgents)
	for _, candidate := range names {
		if normalizeSubAgentLookupKey(candidate) == target {
			return candidate, subAgents[candidate], true
		}
	}
	return "", nil, false
}

func (a *Agent) stageBackgroundNotifications(messages []openai.ChatCompletionMessageParamUnion, acks *turnEventAcks) []openai.ChatCompletionMessageParamUnion {
	if a == nil || a.Background == nil {
		return messages
	}

	notifications := a.Background.PeekNotifications()
	if len(notifications) == 0 {
		return messages
	}

	lines := make([]string, 0, len(notifications)+2)
	lines = append(lines, "<background_notifications>")
	lines = append(lines, "Completed background tasks:")
	for _, notification := range notifications {
		lines = append(lines, fmt.Sprintf("- id=%s status=%s command=%q result=%q",
			notification.TaskID,
			notification.Status,
			notification.Command,
			notification.Result,
		))
	}
	lines = append(lines, "</background_notifications>")

	taskIDs := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		taskIDs = append(taskIDs, notification.TaskID)
	}
	acks.AddCommit(func() error {
		return a.Background.AckNotifications(taskIDs)
	})

	return append(messages, openai.UserMessage(strings.Join(lines, "\n")))
}

func (a *Agent) stageTeamInboxMessages(messages []openai.ChatCompletionMessageParamUnion, acks *turnEventAcks) ([]openai.ChatCompletionMessageParamUnion, error) {
	if a == nil || a.TeamManager == nil || a.TeamManager.bus == nil || strings.TrimSpace(a.Name) == "" {
		return messages, nil
	}

	inbox, keys, err := a.TeamManager.bus.PeekInbox(a.Name)
	if err != nil {
		return messages, err
	}
	if len(inbox) == 0 {
		return messages, nil
	}
	acks.AddCommit(func() error {
		return a.TeamManager.bus.AckInbox(a.Name, keys)
	})
	return append(messages, openai.UserMessage(formatInboxMessages(inbox))), nil
}
