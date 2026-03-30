package agents

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type commandInvocation struct {
	name     string
	args     []string
	viaShell bool
}

const commandTerminationGracePeriod = 3 * time.Second

var commandExecLookPath = exec.LookPath

func buildCommand(command, dir string) (*exec.Cmd, error) {
	invocation, err := resolveCommandInvocation(command, runtime.GOOS, dir)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(invocation.name, invocation.args...)
	cmd.Dir = dir
	applyWorkspaceCommandEnv(cmd, dir)
	return cmd, nil
}

func runCommand(ctx context.Context, command, dir string) ([]byte, error) {
	cmd, err := buildCommand(command, dir)
	if err != nil {
		return nil, err
	}
	return runPreparedCommand(ctx, cmd)
}

func runPreparedCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}

	prepareCommand(cmd)

	buffer := &lockedBuffer{}
	cmd.Stdout = buffer
	cmd.Stderr = buffer

	if err := cmd.Start(); err != nil {
		return buffer.Bytes(), err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	if ctx == nil {
		err := <-waitCh
		return buffer.Bytes(), err
	}

	select {
	case err := <-waitCh:
		return buffer.Bytes(), err
	case <-ctx.Done():
		_ = terminateCommand(cmd)
		select {
		case <-waitCh:
			return buffer.Bytes(), ctx.Err()
		case <-time.After(commandTerminationGracePeriod):
			_ = forceKillCommand(cmd)
			<-waitCh
			return buffer.Bytes(), ctx.Err()
		}
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func resolveCommandInvocation(command, goos, dir string) (commandInvocation, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return commandInvocation{}, errors.New("empty command")
	}

	if args, ok := splitDirectCommand(command, goos); ok {
		args = rewritePythonCommandArgs(args, goos, dir)
		if goos == "windows" {
			args = addWindowsCurlSilentArgs(args)
		}
		return commandInvocation{
			name: args[0],
			args: args[1:],
		}, nil
	}

	switch goos {
	case "windows":
		return commandInvocation{
			name:     "cmd",
			args:     []string{"/d", "/s", "/c", command},
			viaShell: true,
		}, nil
	default:
		return commandInvocation{
			name:     "sh",
			args:     []string{"-lc", command},
			viaShell: true,
		}, nil
	}
}

func rewritePythonCommandArgs(args []string, goos, dir string) []string {
	if len(args) == 0 {
		return args
	}
	if !isBareCommandName(args[0]) {
		return args
	}

	switch normalizeCommandName(args[0], goos) {
	case "pip", "pip3":
		name, prefixArgs := preferredPythonInvocation(normalizeCommandName(args[0], goos), goos, dir)
		if strings.TrimSpace(name) == "" {
			return args
		}
		rewritten := make([]string, 0, len(prefixArgs)+len(args)+2)
		rewritten = append(rewritten, name)
		rewritten = append(rewritten, prefixArgs...)
		rewritten = append(rewritten, "-m", "pip")
		rewritten = append(rewritten, args[1:]...)
		return rewritten
	case "python", "python3":
		name, prefixArgs := preferredPythonInvocation(normalizeCommandName(args[0], goos), goos, dir)
		if strings.TrimSpace(name) == "" {
			return args
		}
		rewritten := make([]string, 0, len(prefixArgs)+len(args))
		rewritten = append(rewritten, name)
		rewritten = append(rewritten, prefixArgs...)
		rewritten = append(rewritten, args[1:]...)
		return rewritten
	default:
		return args
	}
}

func isBareCommandName(name string) bool {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return false
	}
	return filepath.Base(clean) == clean && !strings.ContainsAny(clean, `/\`)
}

func preferredPythonInvocation(requested, goos, dir string) (string, []string) {
	if venvPython := findWorkspacePythonExecutable(dir, goos); strings.TrimSpace(venvPython) != "" {
		return venvPython, nil
	}

	for _, candidate := range pythonLauncherCandidates(requested, goos) {
		path, err := commandExecLookPath(candidate.name)
		if err == nil && strings.TrimSpace(path) != "" {
			return path, append([]string(nil), candidate.args...)
		}
	}
	return "", nil
}

type pythonLauncherCandidate struct {
	name string
	args []string
}

func pythonLauncherCandidates(requested, goos string) []pythonLauncherCandidate {
	if goos == "windows" {
		switch requested {
		case "python3", "pip3":
			return []pythonLauncherCandidate{
				{name: "python3"},
				{name: "python"},
				{name: "py", args: []string{"-3"}},
			}
		default:
			return []pythonLauncherCandidate{
				{name: "python"},
				{name: "python3"},
				{name: "py"},
				{name: "py", args: []string{"-3"}},
			}
		}
	}

	switch requested {
	case "python3", "pip3":
		return []pythonLauncherCandidate{
			{name: "python3"},
			{name: "python"},
		}
	default:
		return []pythonLauncherCandidate{
			{name: "python"},
			{name: "python3"},
		}
	}
}

func findWorkspacePythonExecutable(dir, goos string) string {
	cleanDir := strings.TrimSpace(dir)
	if cleanDir == "" {
		return ""
	}

	baseDir := detectRepoRoot(cleanDir)
	candidates := []string{
		filepath.Join(".venv", platformBinDir(), platformPythonExecutable(goos)),
		filepath.Join("venv", platformBinDir(), platformPythonExecutable(goos)),
	}
	if goos != "windows" {
		candidates = append(candidates,
			filepath.Join(".venv", platformBinDir(), "python3"),
			filepath.Join("venv", platformBinDir(), "python3"),
		)
	}

	return findUpFile(cleanDir, baseDir, candidates...)
}

func platformPythonExecutable(goos string) string {
	if goos == "windows" {
		return "python.exe"
	}
	return "python"
}

func normalizeCommandName(name, goos string) string {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(name)))
	if goos == "windows" {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return base
}

func applyWorkspaceCommandEnv(cmd *exec.Cmd, dir string) {
	if cmd == nil {
		return
	}

	env := append([]string(nil), os.Environ()...)
	pythonPathValue := workspacePythonPathValue(dir, envValue(env, "PYTHONPATH"))
	if strings.TrimSpace(pythonPathValue) != "" {
		env = upsertEnvVar(env, "PYTHONPATH", pythonPathValue)
	}

	pythonPath := findWorkspacePythonExecutable(dir, runtime.GOOS)
	if strings.TrimSpace(pythonPath) != "" {
		binDir := filepath.Dir(pythonPath)
		virtualEnvDir := filepath.Dir(binDir)
		env = upsertEnvVar(env, "PATH", prependPathValue(binDir, envValue(env, "PATH")))
		env = upsertEnvVar(env, "VIRTUAL_ENV", virtualEnvDir)
	}

	cmd.Env = env
}

func workspacePythonPathValue(dir, existing string) string {
	cleanDir := strings.TrimSpace(dir)
	if cleanDir == "" {
		return existing
	}

	values := make([]string, 0, 3)
	appendUnique := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, current := range values {
			if samePath(current, value) {
				return
			}
		}
		values = append(values, value)
	}

	appendUnique(cleanDir)
	appendUnique(detectRepoRoot(cleanDir))

	for _, value := range strings.Split(existing, string(os.PathListSeparator)) {
		appendUnique(value)
	}

	return strings.Join(values, string(os.PathListSeparator))
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if envKeyMatches(k, key) {
			return v
		}
	}
	return ""
}

func upsertEnvVar(env []string, key, value string) []string {
	updated := false
	for i, entry := range env {
		k, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if envKeyMatches(k, key) {
			env[i] = key + "=" + value
			updated = true
		}
	}
	if !updated {
		env = append(env, key+"="+value)
	}
	return env
}

func envKeyMatches(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func prependPathValue(prefix, existing string) string {
	if strings.TrimSpace(existing) == "" {
		return prefix
	}
	return prefix + string(os.PathListSeparator) + existing
}

func splitDirectCommand(command, goos string) ([]string, bool) {
	if commandNeedsShell(command, goos) {
		return nil, false
	}

	args, err := splitCommandLine(command, goos)
	if err != nil || len(args) == 0 {
		return nil, false
	}
	if isShellBuiltin(args[0], goos) {
		return nil, false
	}
	return args, true
}

func isNoMatchSearchResult(command string, err error) bool {
	if strings.TrimSpace(command) == "" || err == nil {
		return false
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}

	lower := strings.ToLower(command)
	for _, marker := range []string{"grep ", "grep.exe", "findstr", "rg ", "rg.exe", "where ", "where.exe", "which ", "select-string"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func formatCommandError(command string, err error, decoded string) string {
	trimmed := strings.TrimSpace(decoded)
	if trimmed != "" {
		return trimmed
	}
	if err == nil {
		return "(no output)"
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message := "Error: command failed"
		if code := exitErr.ExitCode(); code >= 0 {
			message += " (exit code " + strconv.Itoa(code) + ")"
		}
		message += " with no output"
		if commandOutputLikelyRedirected(command) {
			message += "; output may have been redirected"
		}
		return message
	}

	return "Error: " + err.Error()
}

func commandOutputLikelyRedirected(command string) bool {
	lower := strings.ToLower(command)
	for _, marker := range []string{"2>nul", "1>nul", ">nul", "2>/dev/null", "1>/dev/null", ">/dev/null"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func addWindowsCurlSilentArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}

	name := strings.ToLower(strings.TrimSpace(args[0]))
	if name != "curl" && name != "curl.exe" {
		return args
	}

	for _, arg := range args[1:] {
		switch strings.TrimSpace(arg) {
		case "-s", "-S", "-sS", "-Ss", "--silent", "--show-error":
			return args
		}
	}

	updated := make([]string, 0, len(args)+1)
	updated = append(updated, args[0], "-sS")
	updated = append(updated, args[1:]...)
	return updated
}

func commandNeedsShell(command, goos string) bool {
	var inSingle bool
	var inDouble bool
	var escaped bool

	for _, r := range command {
		if escaped {
			escaped = false
			continue
		}

		switch r {
		case '\\':
			if goos != "windows" && !inSingle {
				escaped = true
			}
		case '\'':
			if goos != "windows" && !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		default:
			if inSingle || inDouble {
				continue
			}
			switch r {
			case '|', '&', ';', '<', '>', '(', ')', '$', '`', '\n', '\r', '*', '?', '[', ']', '{', '}', '~':
				return true
			case '%':
				if goos == "windows" {
					return true
				}
			}
		}
	}

	return inSingle || inDouble
}

func splitCommandLine(command, goos string) ([]string, error) {
	var (
		args     []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
		started  bool
	)

	flush := func() {
		if !started {
			return
		}
		args = append(args, current.String())
		current.Reset()
		started = false
	}

	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			started = true
			continue
		}

		switch r {
		case '\\':
			if goos != "windows" && !inSingle {
				escaped = true
				continue
			}
			current.WriteRune(r)
			started = true
		case '\'':
			if goos == "windows" {
				current.WriteRune(r)
				started = true
				continue
			}
			if inDouble {
				current.WriteRune(r)
				started = true
				continue
			}
			inSingle = !inSingle
			started = true
		case '"':
			if inSingle {
				current.WriteRune(r)
				started = true
				continue
			}
			inDouble = !inDouble
			started = true
		case ' ', '\t':
			if inSingle || inDouble {
				current.WriteRune(r)
				started = true
				continue
			}
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}

	if escaped {
		current.WriteRune('\\')
		started = true
	}
	if inSingle || inDouble {
		return nil, errors.New("unterminated quote")
	}
	flush()

	return args, nil
}

func isShellBuiltin(name, goos string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch goos {
	case "windows":
		_, ok := windowsShellBuiltins[name]
		return ok
	default:
		_, ok := unixShellBuiltins[name]
		return ok
	}
}

var windowsShellBuiltins = map[string]struct{}{
	"assoc":    {},
	"break":    {},
	"call":     {},
	"cd":       {},
	"chdir":    {},
	"cls":      {},
	"copy":     {},
	"date":     {},
	"del":      {},
	"dir":      {},
	"echo":     {},
	"endlocal": {},
	"erase":    {},
	"for":      {},
	"ftype":    {},
	"goto":     {},
	"if":       {},
	"md":       {},
	"mkdir":    {},
	"mklink":   {},
	"move":     {},
	"path":     {},
	"pause":    {},
	"popd":     {},
	"prompt":   {},
	"pushd":    {},
	"rd":       {},
	"rem":      {},
	"ren":      {},
	"rename":   {},
	"rmdir":    {},
	"set":      {},
	"setlocal": {},
	"shift":    {},
	"start":    {},
	"time":     {},
	"title":    {},
	"type":     {},
	"ver":      {},
	"verify":   {},
	"vol":      {},
}

var unixShellBuiltins = map[string]struct{}{
	".":       {},
	"alias":   {},
	"bg":      {},
	"cd":      {},
	"eval":    {},
	"exec":    {},
	"exit":    {},
	"export":  {},
	"fg":      {},
	"jobs":    {},
	"set":     {},
	"source":  {},
	"test":    {},
	"ulimit":  {},
	"umask":   {},
	"unalias": {},
	"unset":   {},
	"wait":    {},
}
