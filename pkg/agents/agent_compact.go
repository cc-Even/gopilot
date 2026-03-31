package agents

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
)

const autoCompactTriggerCharsEnv = "AUTO_COMPACT_TRIGGER_CHARS"

const autoCompactSummaryTokensEnv = "AUTO_COMPACT_SUMMARY_MAX_TOKENS"

const autoCompactTriggerCharsDefault = 100000

const autoCompactSummaryTokensDefault = 20000

const autoCompactToolArgPreviewRunes = 240

const autoCompactToolResultPreviewRunes = 320

func compactToolDisplay(payload, toolName string) string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return ""
	}

	switch toolName {
	case "write_file":
		return compactWriteFileDisplay(trimmed)
	default:
		return trimmed
	}
}

func compactWriteFileDisplay(payload string) string {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(payload), &params); err != nil {
		return payload
	}

	type writeFileDisplay struct {
		Path           string `json:"path,omitempty"`
		ContentBytes   int    `json:"content_bytes"`
		ContentPreview string `json:"content_preview,omitempty"`
	}

	display := writeFileDisplay{
		Path:         strings.TrimSpace(params.Path),
		ContentBytes: len(params.Content),
	}

	const previewLimit = 200
	preview := params.Content
	if utf8.RuneCountInString(preview) > previewLimit {
		preview = truncateByRunes(preview, previewLimit) + "... (truncated)"
	}
	if strings.TrimSpace(preview) != "" {
		display.ContentPreview = preview
	}

	data, err := json.MarshalIndent(display, "", "  ")
	if err != nil {
		return payload
	}
	return string(data)
}

func parseCompactFocus(args json.RawMessage) (string, error) {
	if len(args) == 0 || string(args) == "null" {
		return "", nil
	}
	type paramsStruct struct {
		Focus string `json:"focus"`
	}
	params := paramsStruct{}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", err
	}
	return params.Focus, nil
}

func marshalConversationForAutoCompact(messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return "", err
	}

	var payload []map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}

	toolNamesByID := map[string]string{}
	for _, msg := range payload {
		compactMessageForAutoCompact(msg, toolNamesByID)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func compactMessageForAutoCompact(message map[string]any, toolNamesByID map[string]string) {
	role := strings.TrimSpace(stringValue(message["role"]))
	switch role {
	case "assistant":
		compactAssistantToolCallsForAutoCompact(message, toolNamesByID)
	case "tool":
		compactToolMessagePayloadForAutoCompact(message, toolNamesByID)
	}
}

func compactAssistantToolCallsForAutoCompact(message map[string]any, toolNamesByID map[string]string) {
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok || len(toolCalls) == 0 {
		return
	}

	for _, item := range toolCalls {
		toolCall, ok := item.(map[string]any)
		if !ok {
			continue
		}

		toolCallID := strings.TrimSpace(stringValue(toolCall["id"]))
		function, ok := toolCall["function"].(map[string]any)
		if !ok {
			continue
		}

		toolName := strings.TrimSpace(stringValue(function["name"]))
		if toolCallID != "" && toolName != "" {
			toolNamesByID[toolCallID] = toolName
		}

		if arguments := compactToolArgumentsForAutoCompact(stringValue(function["arguments"]), toolName); arguments != "" {
			function["arguments"] = arguments
		}
	}
}

func compactToolMessagePayloadForAutoCompact(message map[string]any, toolNamesByID map[string]string) {
	toolCallID := strings.TrimSpace(stringValue(message["tool_call_id"]))
	toolName := strings.TrimSpace(toolNamesByID[toolCallID])
	message["content"] = compactToolMessageForAutoCompact(stringValue(message["content"]), toolName, toolCallID)
}

func compactToolArgumentsForAutoCompact(arguments, toolName string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}

	type compactToolArgumentsPayload struct {
		Tool    string `json:"tool,omitempty"`
		Chars   int    `json:"chars"`
		Summary string `json:"summary,omitempty"`
	}

	summary := compactToolDisplay(trimmed, toolName)
	if strings.TrimSpace(summary) == "" {
		summary = trimmed
	}
	if utf8.RuneCountInString(summary) > autoCompactToolArgPreviewRunes {
		summary = truncateByRunes(summary, autoCompactToolArgPreviewRunes) + "... (truncated)"
	}

	payload := compactToolArgumentsPayload{
		Tool:    strings.TrimSpace(toolName),
		Chars:   utf8.RuneCountInString(trimmed),
		Summary: strings.TrimSpace(summary),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return trimmed
	}
	return "compact_tool_arguments " + string(data)
}

func compactToolMessageForAutoCompact(content, toolName, toolCallID string) string {
	trimmed := strings.TrimSpace(content)

	type compactToolMessagePayload struct {
		Tool       string `json:"tool,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
		Status     string `json:"status"`
		Chars      int    `json:"chars"`
		Lines      int    `json:"lines"`
		Preview    string `json:"preview,omitempty"`
	}

	status := "ok"
	if strings.HasPrefix(trimmed, "tool error:") {
		status = "error"
	}

	preview := trimmed
	if utf8.RuneCountInString(preview) > autoCompactToolResultPreviewRunes {
		preview = truncateByRunes(preview, autoCompactToolResultPreviewRunes) + "... (truncated)"
	}

	payload := compactToolMessagePayload{
		Tool:       strings.TrimSpace(toolName),
		ToolCallID: strings.TrimSpace(toolCallID),
		Status:     status,
		Chars:      utf8.RuneCountInString(trimmed),
		Lines:      countCompactLines(trimmed),
		Preview:    strings.TrimSpace(preview),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return trimmed
	}
	return "compact_tool_message " + string(data)
}

func countCompactLines(input string) int {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func autoCompactTriggerThreshold() int {
	return intEnvOrDefault(autoCompactTriggerCharsEnv, autoCompactTriggerCharsDefault)
}

func autoCompactSummaryMaxTokens() int {
	return intEnvOrDefault(autoCompactSummaryTokensEnv, autoCompactSummaryTokensDefault)
}
