package agents

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func openAIRateLimitRetryDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv(openAIRateLimitRetryEnv))
	if raw == "" {
		return 0
	}

	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("[OpenAI429Retry] invalid %s value %q: %v", openAIRateLimitRetryEnv, raw, err)
		return 0
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func isOpenAIRateLimitError(err error) bool {
	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return geminiErr.Code == http.StatusTooManyRequests
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests
	}
	return false
}

func isOpenAITransientError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}

	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return geminiErr.Code == http.StatusRequestTimeout || geminiErr.Code >= http.StatusInternalServerError
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusRequestTimeout || apiErr.StatusCode >= http.StatusInternalServerError
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) && (ctx == nil || ctx.Err() == nil) {
		return true
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "temporary failure"),
		strings.Contains(msg, "temporarily unavailable"),
		strings.Contains(msg, "unexpected eof"):
		return true
	default:
		return false
	}
}

func withOpenAIRateLimitRetry[T any](ctx context.Context, label string, fn func() (T, error)) (T, error) {
	var zero T
	attempt := 1
	for {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		if isOpenAIRateLimitError(err) {
			delay := openAIRateLimitRetryDelay()
			if delay <= 0 {
				return result, err
			}

			log.Printf(
				"[OpenAI429Retry] call=%s attempt=%d wait_seconds=%.3f",
				strings.TrimSpace(label),
				attempt,
				delay.Seconds(),
			)
			if sleepErr := rateLimitSleep(ctx, delay); sleepErr != nil {
				return zero, sleepErr
			}
			attempt++
			continue
		}

		if !isOpenAITransientError(ctx, err) || attempt >= openAITransientRetryMaxAttempts {
			return result, err
		}

		delay := openAITransientRetryDelay(attempt)
		log.Printf(
			"[OpenAITransientRetry] call=%s attempt=%d wait_seconds=%.3f err=%v",
			strings.TrimSpace(label),
			attempt,
			delay.Seconds(),
			err,
		)
		if sleepErr := rateLimitSleep(ctx, delay); sleepErr != nil {
			return zero, sleepErr
		}
		attempt++
	}
}
