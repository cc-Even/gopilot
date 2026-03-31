package agents

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var tokenLogMu sync.Mutex

func recordTokenUsage(agent *Agent, model, kind string, turn int, finishReason string, usage tokenUsage) {
	stage := ""
	agentName := agentLogName(agent)
	if agent != nil {
		stage = strings.TrimSpace(agent.runStage)
		if strings.TrimSpace(model) == "" {
			model = agent.Model
		}
	}
	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}
	if strings.TrimSpace(kind) == "" {
		kind = "chat_completion"
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "unknown"
	}

	line := fmt.Sprintf(
		"[TokenUsage] agent=%s stage=%s kind=%s turn=%s model=%s finish_reason=%s prompt_tokens=%d completion_tokens=%d total_tokens=%d reasoning_tokens=%d cached_tokens=%d input_audio_tokens=%d output_audio_tokens=%d accepted_prediction_tokens=%d rejected_prediction_tokens=%d",
		agentName,
		stage,
		kind,
		formatTokenUsageTurn(turn),
		strings.TrimSpace(model),
		strings.TrimSpace(finishReason),
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.TotalTokens,
		usage.ReasoningTokens,
		usage.CachedTokens,
		usage.InputAudioTokens,
		usage.OutputAudioTokens,
		usage.AcceptedPredictionTokens,
		usage.RejectedPredictionTokens,
	)
	log.Print(line)
	if err := appendTokenUsageLine(line); err != nil {
		log.Printf("[TokenUsage] failed to append token log %q: %v", TOKEN_LOG_PATH, err)
	}
}

func formatTokenUsageTurn(turn int) string {
	if turn < 0 {
		return "-"
	}
	return fmt.Sprintf("%d", turn+1)
}

func appendTokenUsageLine(line string) error {
	path := strings.TrimSpace(TOKEN_LOG_PATH)
	if path == "" {
		return fmt.Errorf("token log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tokenLogMu.Lock()
	defer tokenLogMu.Unlock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339Nano), line)
	return err
}
