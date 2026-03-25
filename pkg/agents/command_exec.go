package agents

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
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

func buildCommand(command string) (*exec.Cmd, error) {
	invocation, err := resolveCommandInvocation(command, runtime.GOOS)
	if err != nil {
		return nil, err
	}

	return exec.Command(invocation.name, invocation.args...), nil
}

func runCommand(ctx context.Context, command, dir string) ([]byte, error) {
	cmd, err := buildCommand(command)
	if err != nil {
		return nil, err
	}
	cmd.Dir = dir
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

func resolveCommandInvocation(command, goos string) (commandInvocation, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return commandInvocation{}, errors.New("empty command")
	}

	if args, ok := splitDirectCommand(command, goos); ok {
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
