package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/openai/openai-go/v3"
)

func (a *Agent) Run(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	return a.runLoop(ctx, messages)
}

func (a *Agent) RunStructured(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	result, _, err := a.RunStructuredWithState(ctx, messages)
	return result, err
}

func (a *Agent) ContinueStructured(ctx context.Context, state *StructuredRunState, input string) (string, *StructuredRunState, error) {
	if state == nil {
		return "", nil, fmt.Errorf("structured run state unavailable")
	}
	switch strings.TrimSpace(state.Stage) {
	case "planner":
		if len(state.PlannerMessages) == 0 {
			return "", nil, fmt.Errorf("structured planner state unavailable")
		}
		log.Printf("[StructuredRun] agent=%s resuming planner stage: input_size=%d", agentLogName(a), len(strings.TrimSpace(input)))

		planner := a.cloneWithTools(a.plannerSystemPrompt(), plannerToolAllowlist)
		planner.runStage = "planner"
		planner.LiveOutputID = fmt.Sprintf("%s:planner", a.Name)
		planner.LiveOutputTitle = "Planner"

		messages := cloneChatMessages(state.PlannerMessages)
		if strings.TrimSpace(input) != "" {
			messages = append(messages, openai.UserMessage(input))
		}

		plan, resumeMessages, err := planner.runLoopWithState(ctx, messages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s resumed planner paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildPlannerState(state.BaseMessages, messages, resumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s resumed planner failed: %v", agentLogName(a), err)
			next := a.buildPlannerState(state.BaseMessages, messages, resumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "planner",
				Cause:  err,
				Resume: next,
			}
		}

		log.Printf("[StructuredRun] agent=%s resumed planner completed: plan_size=%d", agentLogName(a), len(strings.TrimSpace(plan)))
		a.reportStageOutput("planner", plan)

		baseMessages := cloneChatMessages(state.BaseMessages)
		if len(baseMessages) == 0 {
			return "", nil, fmt.Errorf("planner resume missing base messages for executor handoff")
		}

		executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
		executor.runStage = "executor"
		executor.LiveOutputID = fmt.Sprintf("%s:executor", a.Name)
		executor.LiveOutputTitle = "Executor"

		executorMessages := applySystemPrompt(baseMessages, executor.SystemPrompt)
		executorMessages = append(executorMessages, openai.UserMessage(a.executorContextMessage(plan, true)))

		result, executorResumeMessages, err := executor.runLoopWithState(ctx, executorMessages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s executor stage paused after planner resume: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildExecutorState(baseMessages, plan, executorMessages, executorResumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s executor stage failed after planner resume: %v", agentLogName(a), err)
			next := a.buildExecutorState(baseMessages, plan, executorMessages, executorResumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "executor",
				Cause:  err,
				Resume: next,
			}
		}
		log.Printf("[StructuredRun] agent=%s executor stage completed after planner resume: result_size=%d", agentLogName(a), len(strings.TrimSpace(result)))
		return result, nil, nil

	case "", "executor":
		if len(state.ExecutorMessages) == 0 {
			return "", nil, fmt.Errorf("structured executor state unavailable")
		}
		log.Printf("[StructuredRun] agent=%s resuming executor stage: input_size=%d", agentLogName(a), len(strings.TrimSpace(input)))

		executor := a.cloneWithTools(a.executorSystemPrompt(), nil)
		executor.runStage = "executor"
		executor.LiveOutputID = fmt.Sprintf("%s:executor", a.Name)
		executor.LiveOutputTitle = "Executor"

		messages := cloneChatMessages(state.ExecutorMessages)
		if strings.TrimSpace(input) != "" {
			messages = append(messages, openai.UserMessage(input))
		}

		result, resumeMessages, err := executor.runLoopWithState(ctx, messages)
		if err != nil {
			var pauseErr *runPausedError
			if errors.As(err, &pauseErr) {
				log.Printf("[StructuredRun] agent=%s resumed executor paused: %s", agentLogName(a), strings.TrimSpace(pauseErr.Question))
				next := a.buildExecutorState(state.BaseMessages, state.Plan, messages, resumeMessages, &StructuredPauseInfo{
					Kind:     pauseErr.Kind,
					Question: pauseErr.Question,
				})
				next.Status = RunPaused
				return pauseErr.Question, next, nil
			}
			log.Printf("[StructuredRun] agent=%s resumed executor failed: %v", agentLogName(a), err)
			next := a.buildExecutorState(state.BaseMessages, state.Plan, messages, resumeMessages, nil)
			return "", next, &StructuredRunError{
				Stage:  "executor",
				Cause:  err,
				Resume: next,
			}
		}
		log.Printf("[StructuredRun] agent=%s resumed executor completed: result_size=%d", agentLogName(a), len(strings.TrimSpace(result)))
		return result, nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported structured stage %q", state.Stage)
	}
}

func (a *Agent) runLoop(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, error) {
	result, _, err := a.runLoopWithState(ctx, messages)
	return result, err
}

func (a *Agent) runLoopWithState(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion) (string, []openai.ChatCompletionMessageParamUnion, error) {
	if a != nil && a.runLoopOverride != nil {
		result, err := a.runLoopOverride(a, ctx, messages)
		if err != nil {
			return "", cloneChatMessages(messages), err
		}
		return result, nil, nil
	}

	maxTurns := maxTurnsLimit()
	roundsSinceTodo := 0
	for turn := 0; turn < maxTurns; turn++ {
		var err error
		var compactErr error
		messages, compactErr = a.maybeAutoCompact(ctx, messages)
		if compactErr != nil {
			return "", cloneChatMessages(messages), fmt.Errorf("auto compact failed (turn=%d): %w", turn, compactErr)
		}
		stableMessages := cloneChatMessages(messages)
		turnAcks := &turnEventAcks{}
		messages = a.stageBackgroundNotifications(messages, turnAcks)
		messages, err = a.stageTeamInboxMessages(messages, turnAcks)
		if err != nil {
			_ = turnAcks.Rollback()
			return "", stableMessages, fmt.Errorf("stage turn events failed (turn=%d): %w", turn, err)
		}

		resp, err := a.streamChatCompletion(ctx, messages, turn)
		if err != nil {
			_ = turnAcks.Rollback()
			return "", stableMessages, err
		}
		if err := turnAcks.Commit(); err != nil {
			return "", stableMessages, fmt.Errorf("ack turn events failed (turn=%d): %w", turn, err)
		}
		usedTodo := false

		switch resp.FinishReason {
		case "stop":
			messages = append(messages, buildAssistantMessage(resp))
			return resp.Content, nil, nil

		case "tool_calls":
			messages = append(messages, buildAssistantMessage(resp))
			manualCompacted := false
			for _, tc := range resp.ToolCalls {
				toolName := tc.Name
				toolArgs := json.RawMessage(tc.Arguments)
				a.reportStageOutput(
					fmt.Sprintf("%s Tool %s", a.displayTitle(), toolName),
					fmt.Sprintf("开始执行:\n%s", compactToolDisplay(tc.Arguments, toolName)),
				)
				output, callErr := a.executeTool(ctx, toolName, toolArgs)
				if callErr != nil {
					output = "tool error: " + callErr.Error()
				}
				a.reportStageOutput(
					fmt.Sprintf("%s Tool %s", a.displayTitle(), toolName),
					fmt.Sprintf("执行结果:\n%s", strings.TrimSpace(toolResultCompact(output, toolName))),
				)

				// 回填 tool 消息，关联 tool_call_id
				messages = append(messages, openai.ToolMessage(output, tc.ID))
				if toolName == "ask_user" && callErr == nil {
					return output, cloneChatMessages(messages), &runPausedError{
						Stage:    strings.TrimSpace(a.runStage),
						Kind:     "ask_user",
						Question: strings.TrimSpace(output),
					}
				}
				if toolName == "handoff_to_executor" && callErr == nil && strings.TrimSpace(a.runStage) == "planner" {
					return output, cloneChatMessages(messages), nil
				}
				if countsAsPlanningTool(toolName) {
					usedTodo = true
				}

				if toolName == "compact" && callErr == nil {
					focus, parseErr := parseCompactFocus(toolArgs)
					if parseErr != nil {
						return "", cloneChatMessages(messages), fmt.Errorf("manual compact args parse failed (turn=%d): %w", turn, parseErr)
					}
					messages, err = a.forceAutoCompact(ctx, messages, focus)
					if err != nil {
						return "", cloneChatMessages(messages), fmt.Errorf("manual compact failed (turn=%d): %w", turn, err)
					}
					manualCompacted = true
					roundsSinceTodo = 0
					break
				}
			}
			if manualCompacted {
				continue
			}

		case "network_error":
			return "", stableMessages, fmt.Errorf("model interrupted with finish reason: %s", resp.FinishReason)

		default:
			return "", stableMessages, fmt.Errorf("unsupported finish reason: %s", resp.FinishReason)
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}
		if roundsSinceTodo >= planningReminderTurns {
			messages = append(messages, openai.UserMessage("<reminder>Update your task status or todos.</reminder>"))
		}
	}

	return "", cloneChatMessages(messages), fmt.Errorf("max turns reached without final answer")
}

func (a *Agent) streamChatCompletion(ctx context.Context, messages []openai.ChatCompletionMessageParamUnion, turn int) (*modelResponse, error) {
	if a == nil || a.provider == nil {
		return nil, fmt.Errorf("model provider unavailable")
	}
	return withOpenAIRateLimitRetry(ctx, "stream_chat_completion", func() (*modelResponse, error) {
		liveID, liveTitle := a.liveOutputIdentity()
		lastPreview := ""
		if liveID != "" {
			lastPreview = "思考中..."
			a.reportLiveOutput(liveID, liveTitle, lastPreview, false)
		}

		resp, err := a.provider.Generate(ctx, modelRequest{
			Model:    a.Model,
			Messages: messages,
			Tools:    collectToolList(a),
			Stream:   true,
		}, func(update modelResponse) {
			preview := renderStreamingPreview(update)
			if preview != "" && preview != lastPreview {
				lastPreview = preview
				a.reportLiveOutput(liveID, liveTitle, preview, false)
			}
		})
		if err != nil {
			a.reportLiveOutput(liveID, liveTitle, "", true)
			return nil, fmt.Errorf("chat completion failed (turn=%d): %w", turn, err)
		}
		if resp == nil {
			a.reportLiveOutput(liveID, liveTitle, "", true)
			return nil, fmt.Errorf("empty choices from model")
		}
		resp.ToolCalls = normalizeModelToolCalls(resp.ToolCalls)
		a.reportLiveOutput(liveID, liveTitle, renderStreamingPreview(*resp), true)
		recordTokenUsage(a, a.Model, "stream_chat_completion", turn, resp.FinishReason, resp.Usage)
		return resp, nil
	})
}
