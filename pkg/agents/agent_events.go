package agents

import (
	"fmt"
	"log"
	"strings"

	"github.com/openai/openai-go/v3"
)

type StructuredRunState struct {
	Status           StructuredRunStatus
	Stage            string
	Plan             string
	BaseMessages     []openai.ChatCompletionMessageParamUnion
	PlannerMessages  []openai.ChatCompletionMessageParamUnion
	ExecutorMessages []openai.ChatCompletionMessageParamUnion
	Pause            *StructuredPauseInfo
}

type StructuredRunError struct {
	Stage  string
	Cause  error
	Resume *StructuredRunState
}

func (e *StructuredRunError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Stage) == "" {
		if e.Cause == nil {
			return "structured run failed"
		}
		return e.Cause.Error()
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s stage failed", e.Stage)
	}
	return fmt.Sprintf("%s stage failed: %v", e.Stage, e.Cause)
}

func (e *StructuredRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type turnEventAcks struct {
	commits   []func() error
	rollbacks []func() error
}

func (a *turnEventAcks) AddCommit(fn func() error) {
	if a == nil || fn == nil {
		return
	}
	a.commits = append(a.commits, fn)
}

func (a *turnEventAcks) AddRollback(fn func() error) {
	if a == nil || fn == nil {
		return
	}
	a.rollbacks = append(a.rollbacks, fn)
}

type runPausedError struct {
	Stage    string
	Kind     string
	Question string
}

func (e *runPausedError) Error() string {
	if e == nil {
		return ""
	}
	question := strings.TrimSpace(e.Question)
	if question == "" {
		question = "run paused"
	}
	return question
}

func (a *Agent) reportStageOutput(stage, content string) {
	log.Printf(
		"[StageOutput] agent=%s stage=%s content=%s",
		agentLogName(a),
		strings.TrimSpace(stage),
		truncate(strings.TrimSpace(content), 4000),
	)
	if a == nil || a.stageOutputReporter == nil {
		return
	}
	a.stageOutputReporter(stage, content)
}

func (a *Agent) reportLiveOutput(id, title, content string, done bool) {
	if done {
		trimmed := strings.TrimSpace(content)
		if trimmed != "" {
			log.Printf(
				"[LiveOutput] agent=%s id=%s title=%s state=final content=%s",
				agentLogName(a),
				strings.TrimSpace(id),
				strings.TrimSpace(title),
				truncate(trimmed, 4000),
			)
		}
	}
	if a == nil || a.liveOutputReporter == nil {
		return
	}
	a.liveOutputReporter(id, title, content, done)
}

func (a *turnEventAcks) Commit() error {
	if a == nil {
		return nil
	}
	for _, fn := range a.commits {
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			return err
		}
	}
	a.commits = nil
	a.rollbacks = nil
	return nil
}

func (a *turnEventAcks) Rollback() error {
	if a == nil {
		return nil
	}
	for i := len(a.rollbacks) - 1; i >= 0; i-- {
		fn := a.rollbacks[i]
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			return err
		}
	}
	a.commits = nil
	a.rollbacks = nil
	return nil
}
