package main

import (
	"bufio"
	"claude-go/pkg/agents"
	"claude-go/pkg/version"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/rivo/tview"
)

const (
	LogFileName = "debug.log"
	ToolName    = "Gopilot"
)

const logo = `
  ____             _ _       _   
 / ___| ___  _ __ (_) | ___ | |_ 
| |  _ / _ \| '_ \| | |/ _ \| __|
| |_| | (_) | |_) | | | (_) | |_ 
 \____|\___/| .__/|_|_|\___/ \__|
            |_|                  
`

var (
	currentModel = "unknown"
	currentDir   = agents.WORKDIR
	modelEnvLine = regexp.MustCompile(`^(\s*)(export\s+)?MODEL\s*=`)
)

type cliSession struct {
	app            *tview.Application
	output         *tview.TextView
	logs           *tview.TextView
	updateHeader   func()
	envFile        string
	skillLoader    *agents.SkillLoader
	subAgentLoader *agents.SubAgentLoader
	systemPrompt   string
	agent          *agents.Agent
	history        []openai.ChatCompletionMessageParamUnion
	outputHistory  []string
	liveBlocks     map[string]liveOutputBlock
	liveOrder      []string
	running        bool
	runCancel      context.CancelFunc
	resumeState    *agents.StructuredRunState
	resumeHistory  []openai.ChatCompletionMessageParamUnion
}

type liveOutputBlock struct {
	Title   string
	Content string
}

func main() {
	envFile := detectEnvFile()
	if err := reloadEnvFile(envFile); err != nil {
		log.Printf("Warning: failed to load env file %q: %v", envFile, err)
	}

	currentModel = getenvOrDefault("MODEL", "gpt-4o-mini")
	currentDir = agents.WORKDIR

	app := tview.NewApplication()

	header := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	updateHeader := func() {
		headerText := fmt.Sprintf(
			"[yellow::b]%s [white::]%s | [gray]Model:[green] %s [gray]| Dir:[blue] %s",
			ToolName,
			version.Version,
			tview.Escape(currentModel),
			tview.Escape(currentDir),
		)
		header.SetText(headerText)
	}
	updateHeader()

	outputView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(true)
	outputView.SetBorder(true).SetTitle(" Output ")

	logView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(true)
	logView.SetBorder(true).SetTitle(" Logs ")

	logPath := filepath.Join(agents.TOOLDIR, LogFileName)
	if err := installUILogSink(app, logView, logPath); err != nil {
		log.Printf("Warning: failed to install UI log sink: %v", err)
	} else {
		log.Printf("Debug log file: %s", logPath)
	}

	inputField := tview.NewInputField().
		SetLabel("[purple::b]❯ [white]").
		SetFieldBackgroundColor(tcell.ColorDefault).
		SetFieldTextColor(tcell.ColorWhite)

	availableCommands := []string{
		"/model",
		"/tasks",
		"/team",
		"/stop",
		"/clear",
	}

	session := newCLISession(app, outputView, logView, updateHeader, envFile)
	session.showStartupLogo()

	inputField.SetAutocompleteFunc(func(currentText string) (entries []string) {
		if !strings.HasPrefix(currentText, "/") {
			return nil
		}
		for _, cmd := range availableCommands {
			if strings.HasPrefix(cmd, currentText) {
				entries = append(entries, cmd)
			}
		}
		return entries
	})

	inputField.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}

		userInput := strings.TrimSpace(inputField.GetText())
		if userInput == "" {
			return
		}

		inputField.SetText("")
		session.handleInput(userInput)
	})

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			app.Stop()
			return nil
		}
		return event
	})

	body := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(outputView, 0, 3, false).
		AddItem(logView, 0, 1, false)

	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(body, 0, 1, false).
		AddItem(inputField, 1, 0, true)

	if err := app.SetRoot(layout, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}
}

func newCLISession(app *tview.Application, output *tview.TextView, logs *tview.TextView, updateHeader func(), envFile string) *cliSession {
	skillLoader := agents.NewSkillLoader(agents.SKILL_DIR)
	subAgentLoader := agents.NewSubAgentLoader(agents.SUBAGENT_DIR)
	systemPrompt := buildSystemPrompt(skillLoader, subAgentLoader)

	session := &cliSession{
		app:            app,
		output:         output,
		logs:           logs,
		updateHeader:   updateHeader,
		envFile:        envFile,
		skillLoader:    skillLoader,
		subAgentLoader: subAgentLoader,
		systemPrompt:   systemPrompt,
		liveBlocks:     make(map[string]liveOutputBlock),
	}
	session.resetConversation()
	return session
}

func (s *cliSession) handleInput(input string) {
	if strings.HasPrefix(input, "/") {
		s.appendLine("[purple]User:[white] %s", tview.Escape(input))
		s.executeCommand(input)
		s.output.ScrollToEnd()
		return
	}

	if s.running {
		s.appendLine("[red]System:[white] 当前主 agent 正在执行任务，请等待本轮完成。")
		return
	}

	s.appendLine("[purple]User:[white] %s", tview.Escape(input))
	if s.resumeState != nil {
		s.appendLine("[green]Gopilot:[white] 正在从中断的 executor 继续...")
		s.output.ScrollToEnd()
		s.runResume(input)
		return
	}

	s.appendLine("[green]Gopilot:[white] 正在思考中...")
	s.output.ScrollToEnd()

	history := append([]openai.ChatCompletionMessageParamUnion{}, s.history...)
	history = append(history, openai.UserMessage(input))
	s.runStructured(history)
}

func (s *cliSession) executeCommand(input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "/model":
		if s.running {
			s.appendLine("[red]System:[white] 主 agent 运行中，暂时不能切换模型。")
			return
		}
		s.handleModelCommand(parts[1:])
	case "/tasks":
		s.handleTasksCommand()
	case "/team":
		s.handleTeamCommand()
	case "/stop":
		s.handleStopCommand()
	case "/clear":
		if s.running {
			s.appendLine("[red]System:[white] 主 agent 运行中，暂时不能清空并重置会话。")
			return
		}
		s.clearViews()
		s.resetConversation()
	case "/exit":
		s.app.Stop()
	default:
		s.appendLine("[red]System:[white] 未知命令: %s", tview.Escape(parts[0]))
	}
}

func (s *cliSession) handleModelCommand(args []string) {
	if len(args) == 0 {
		s.appendLine("[yellow]System:[white] 用法: /model <model-name>  当前模型: %s", tview.Escape(currentModel))
		return
	}

	modelName := strings.TrimSpace(args[0])
	if modelName == "" {
		s.appendLine("[yellow]System:[white] 模型名称不能为空。")
		return
	}

	if err := updateModelInEnvFile(s.envFile, modelName); err != nil {
		s.appendLine("[red]System:[white] 更新环境文件失败: %s", tview.Escape(err.Error()))
		return
	}
	if err := reloadEnvFile(s.envFile); err != nil {
		s.appendLine("[red]System:[white] 重新加载环境变量失败: %s", tview.Escape(err.Error()))
		return
	}

	s.rebuildAgent()
	s.appendLine(
		"[green]System:[white] 已切换模型为 %s，并重新加载 %s。",
		tview.Escape(currentModel),
		tview.Escape(s.envFile),
	)
}

func (s *cliSession) handleTasksCommand() {
	if s.agent == nil || s.agent.TaskManager == nil {
		s.appendLine("[red]System:[white] Task manager 未初始化。")
		return
	}

	result, err := s.agent.TaskManager.ListAll()
	if err != nil {
		s.appendLine("[red]System:[white] 读取任务列表失败: %s", tview.Escape(err.Error()))
		return
	}
	s.appendLine("[yellow]Tasks:[white]\n%s", tview.Escape(result))
}

func (s *cliSession) handleTeamCommand() {
	if s.agent == nil || s.agent.TeamManager == nil {
		s.appendLine("[red]System:[white] Team manager 未初始化。")
		return
	}

	s.appendLine("[yellow]Team:[white]\n%s", tview.Escape(s.agent.TeamManager.ListAll()))
}

func (s *cliSession) handleStopCommand() {
	if !s.running || s.runCancel == nil {
		s.appendLine("[yellow]System:[white] 当前没有可停止的主 agent 任务。")
		return
	}

	s.runCancel()
	s.appendLine("[yellow]System:[white] 已发送停止信号，等待主 agent 退出当前任务。")
}

func (s *cliSession) resetConversation() {
	s.rebuildAgent()
	s.history = []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(s.systemPrompt),
	}
	s.clearResumeState()
}

func (s *cliSession) rebuildAgent() {
	currentModel = getenvOrDefault("MODEL", "gpt-4o-mini")
	subAgents := s.subAgentLoader.BuildAgents(currentModel, agents.DefaultToolDefinitions(), s.skillLoader)
	s.agent = agents.NewOpenAIAgent(
		"supervisor",
		s.systemPrompt,
		currentModel,
		agents.WithToolList(agents.DefaultToolDefinitions()),
		agents.WithSubAgents(subAgents),
		agents.WithSkillLoader(s.skillLoader),
	)
	wireAgentReporters(s.agent,
		func(stage, content string) {
			s.app.QueueUpdateDraw(func() {
				switch stage {
				case "planner":
					s.appendLine("[yellow]Planner:[white] %s", tview.Escape(content))
				default:
					s.appendLine("[yellow]%s:[white] %s", tview.Escape(stage), tview.Escape(content))
				}
				s.output.ScrollToEnd()
			})
		},
		func(id, title, content string, done bool) {
			s.app.QueueUpdateDraw(func() {
				if done {
					s.clearLiveBlock(id)
				} else {
					s.setLiveBlock(id, title, content)
				}
				s.output.ScrollToEnd()
			})
		},
	)
	currentDir = s.agent.WorkDir
	s.updateHeader()
}

func (s *cliSession) runStructured(snapshot []openai.ChatCompletionMessageParamUnion) {
	agent := s.agent
	runCtx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.runCancel = cancel

	go func(historySnapshot []openai.ChatCompletionMessageParamUnion, activeAgent *agents.Agent) {
		response, state, err := activeAgent.RunStructuredWithState(runCtx, historySnapshot)
		s.app.QueueUpdateDraw(func() {
			defer s.finishRun(activeAgent)

			if err != nil {
				s.captureResumeState(historySnapshot, state)
				s.reportRunError(err)
				return
			}

			s.clearResumeState()
			s.history = append(copyMessages(historySnapshot), openai.AssistantMessage(response))
			s.appendLine("[green]Gopilot:[white] %s", tview.Escape(response))
		})
	}(copyMessages(snapshot), agent)
}

func (s *cliSession) runResume(input string) {
	state := s.resumeState
	if state == nil {
		return
	}

	agent := s.agent
	baseHistory := copyMessages(s.resumeHistory)
	runCtx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.runCancel = cancel

	go func(resumeInput string, historySnapshot []openai.ChatCompletionMessageParamUnion, resumeState *agents.StructuredRunState, activeAgent *agents.Agent) {
		response, nextState, err := activeAgent.ContinueStructured(runCtx, resumeState, resumeInput)
		s.app.QueueUpdateDraw(func() {
			defer s.finishRun(activeAgent)

			if err != nil {
				nextHistory := append(copyMessages(historySnapshot), openai.UserMessage(resumeInput))
				s.captureResumeState(nextHistory, nextState)
				s.reportRunError(err)
				return
			}

			updatedHistory := append(copyMessages(historySnapshot), openai.UserMessage(resumeInput))
			updatedHistory = append(updatedHistory, openai.AssistantMessage(response))
			s.clearResumeState()
			s.history = updatedHistory
			s.appendLine("[green]Gopilot:[white] %s", tview.Escape(response))
		})
	}(input, baseHistory, state, agent)
}

func (s *cliSession) finishRun(activeAgent *agents.Agent) {
	s.running = false
	s.runCancel = nil
	currentDir = activeAgent.WorkDir
	currentModel = activeAgent.Model
	s.updateHeader()
	s.output.ScrollToEnd()
}

func (s *cliSession) captureResumeState(history []openai.ChatCompletionMessageParamUnion, state *agents.StructuredRunState) {
	if state == nil {
		s.clearResumeState()
		return
	}
	s.resumeState = &agents.StructuredRunState{
		Plan:             state.Plan,
		ExecutorMessages: copyMessages(state.ExecutorMessages),
	}
	s.resumeHistory = copyMessages(history)
}

func (s *cliSession) clearResumeState() {
	s.resumeState = nil
	s.resumeHistory = nil
}

func (s *cliSession) reportRunError(err error) {
	s.appendLine("[red]Gopilot Error:[white] %s", tview.Escape(err.Error()))

	var runErr *agents.StructuredRunError
	if errors.As(err, &runErr) && runErr.Resume != nil {
		s.appendLine("[yellow]System:[white] 可输入 continue 或补充说明，从中断的 executor 位置继续；如需放弃本次现场可使用 /clear。")
	}
}

func (s *cliSession) appendLine(format string, args ...any) {
	s.outputHistory = append(s.outputHistory, fmt.Sprintf(format, args...))
	s.renderOutput()
}

func (s *cliSession) showStartupLogo() {
	s.appendLine("[green]%s[white]", tview.Escape(strings.TrimPrefix(logo, "\n")))
	s.output.ScrollToBeginning()
}

func (s *cliSession) appendLogLine(line string) {
	if s.logs == nil {
		return
	}
	fmt.Fprintf(s.logs, "[gray]Log:[white] %s\n", tview.Escape(line))
	s.logs.ScrollToEnd()
}

func (s *cliSession) clearViews() {
	s.outputHistory = nil
	s.liveBlocks = make(map[string]liveOutputBlock)
	s.liveOrder = nil
	s.renderOutput()
	if s.logs != nil {
		s.logs.Clear()
	}
}

func (s *cliSession) setLiveBlock(id, title, content string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	if _, exists := s.liveBlocks[id]; !exists {
		s.liveOrder = append(s.liveOrder, id)
	}
	s.liveBlocks[id] = liveOutputBlock{Title: title, Content: content}
	s.renderOutput()
}

func (s *cliSession) clearLiveBlock(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	if _, exists := s.liveBlocks[id]; !exists {
		return
	}
	delete(s.liveBlocks, id)
	next := make([]string, 0, len(s.liveOrder))
	for _, existing := range s.liveOrder {
		if existing != id {
			next = append(next, existing)
		}
	}
	s.liveOrder = next
	s.renderOutput()
}

func (s *cliSession) renderOutput() {
	if s.output == nil {
		return
	}
	s.output.Clear()
	for _, line := range s.outputHistory {
		fmt.Fprintln(s.output, line)
	}
	for _, id := range s.liveOrder {
		block, ok := s.liveBlocks[id]
		if !ok {
			continue
		}
		fmt.Fprintf(
			s.output,
			"[yellow]%s:[white] %s\n",
			tview.Escape(block.Title),
			tview.Escape(block.Content),
		)
	}
}

func wireAgentReporters(
	agent *agents.Agent,
	stageReporter func(stage, content string),
	liveReporter func(id, title, content string, done bool),
) {
	if agent == nil {
		return
	}
	agent.SetStageOutputReporter(stageReporter)
	agent.SetLiveOutputReporter(liveReporter)
	for _, subAgent := range agent.SubAgents {
		wireAgentReporters(subAgent, stageReporter, liveReporter)
	}
}

func copyMessages(messages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]openai.ChatCompletionMessageParamUnion, len(messages))
	copy(cloned, messages)
	return cloned
}

func buildSystemPrompt(skillLoader *agents.SkillLoader, subAgentLoader *agents.SubAgentLoader) string {
	return fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks and summarize results. ", agents.WORKDIR) +
		"The runtime may invoke you in planner or executor stage; obey the current stage instructions exactly. " +
		"For complex tasks, use the task board to keep the plan and execution state explicit. " +
		"When you need to inspect a group of related files, prefer read_files so you can batch reads under one budgeted call; use read_file when you are drilling into a single file or rereading a narrow slice. " +
		"When you spawn a teammate, capture the returned run_id. If later steps depend on that teammate's work, call wait_teammate with the run_id before continuing or giving a final answer. Do not assume background teammates finish before you do. " +
		"After wait_teammate returns, inspect the returned run status and any inbox report, then decide the next step. " +
		"Use the todo tool for short checklists. " +
		fmt.Sprintf("Skills: %s. ", skillLoader.GetDescriptions()) +
		fmt.Sprintf("Sub-agents: %s", subAgentLoader.GetDescriptions())
}

func detectEnvFile() string {
	for _, candidate := range []string{".env", "setting.env"} {
		envPath := filepath.Join(agents.TOOLDIR, candidate)
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}
	return ".env"
}

func reloadEnvFile(envFile string) error {
	if strings.TrimSpace(envFile) == "" {
		return nil
	}
	if _, err := os.Stat(envFile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return godotenv.Overload(envFile)
}

func updateModelInEnvFile(envFile, model string) error {
	if strings.TrimSpace(envFile) == "" {
		envFile = ".env"
	}

	content, err := os.ReadFile(envFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := make([]string, 0)
	found := false

	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := scanner.Text()
		if updated, ok := replaceModelEnvLine(line, model); ok {
			lines = append(lines, updated)
			found = true
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if !found {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "MODEL="+model)
	}

	return os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func replaceModelEnvLine(line, model string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return line, false
	}

	matches := modelEnvLine.FindStringSubmatch(line)
	if len(matches) == 0 {
		return line, false
	}

	return fmt.Sprintf("%s%sMODEL=%s", matches[1], matches[2], model), true
}

func getenvOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func installUILogSink(app *tview.Application, logView *tview.TextView, logPath string) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}

	os.Stdout = writer
	os.Stderr = writer
	log.SetOutput(writer)

	go func() {
		defer logFile.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if _, err := fmt.Fprintln(logFile, line); err != nil {
				app.QueueUpdateDraw(func() {
					fmt.Fprintf(logView, "[red]Log Error:[white] failed to write log file: %s\n", tview.Escape(err.Error()))
					logView.ScrollToEnd()
				})
			}
			app.QueueUpdateDraw(func() {
				fmt.Fprintf(logView, "[gray]Log:[white] %s\n", tview.Escape(line))
				logView.ScrollToEnd()
			})
		}

		if err := scanner.Err(); err != nil {
			app.QueueUpdateDraw(func() {
				fmt.Fprintf(logView, "[red]Log Error:[white] %s\n", tview.Escape(err.Error()))
				logView.ScrollToEnd()
			})
		}
	}()

	return nil
}
