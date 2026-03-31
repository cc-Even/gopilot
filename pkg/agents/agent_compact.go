package agents

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const autoCompactTriggerCharsEnv = "AUTO_COMPACT_TRIGGER_CHARS"

const autoCompactSummaryTokensEnv = "AUTO_COMPACT_SUMMARY_MAX_TOKENS"

const autoCompactTriggerCharsDefault = 100000

const autoCompactSummaryTokensDefault = 20000

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

func autoCompactTriggerThreshold() int {
	return intEnvOrDefault(autoCompactTriggerCharsEnv, autoCompactTriggerCharsDefault)
}

func autoCompactSummaryMaxTokens() int {
	return intEnvOrDefault(autoCompactSummaryTokensEnv, autoCompactSummaryTokensDefault)
}
