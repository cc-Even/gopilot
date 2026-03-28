package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type CheckTypesTool struct{}

type typeCheckCommand struct {
	Dir  string
	Name string
	Args []string
}

var checkTypesExec = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return runPreparedCommand(ctx, cmd)
}

var checkTypesLookPath = exec.LookPath

func (c CheckTypesTool) Name() string {
	return "check_types"
}

func (c CheckTypesTool) Description() string {
	return "Run a language-appropriate type checker for one file path. This chooses a project-aware checker when possible, such as go build, pyright/mypy, tsc, Maven/Gradle, or javac."
}

func (c CheckTypesTool) Call(ctx context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}
	if strings.TrimSpace(params.Path) == "" {
		return "", fmt.Errorf("path is required")
	}

	baseDir := agentWorkspaceDir(agent)
	resolved, err := safePath(baseDir, params.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", params.Path)
	}

	command, unavailable := resolveTypeCheckCommand(baseDir, resolved)
	if unavailable != "" {
		return unavailable, nil
	}
	if command == nil {
		return fmt.Sprintf("Type checking is not supported for %s.", filepath.Ext(resolved)), nil
	}

	raw, err := checkTypesExec(ctx, command.Dir, command.Name, command.Args...)
	decoded := strings.TrimSpace(decodeCommandOutput(raw))
	displayPath := workspaceRelativePath(baseDir, resolved)
	displayCmd := command.display()

	if err != nil {
		return fmt.Sprintf("Type check failed for %s using %s\n%s", displayPath, displayCmd, formatCommandError(displayCmd, err, decoded)), nil
	}

	if decoded == "" {
		decoded = "(no output)"
	}
	return fmt.Sprintf("Type check passed for %s using %s\n%s", displayPath, displayCmd, decoded), nil
}

func (c typeCheckCommand) display() string {
	parts := append([]string{filepath.Base(c.Name)}, c.Args...)
	return strings.Join(parts, " ")
}

func resolveTypeCheckCommand(baseDir, filePath string) (*typeCheckCommand, string) {
	switch ext := strings.ToLower(filepath.Ext(filePath)); ext {
	case ".go":
		return resolveGoTypeCheck(baseDir, filePath)
	case ".py", ".pyi":
		return resolvePythonTypeCheck(baseDir, filePath)
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return resolveTypeScriptTypeCheck(baseDir, filePath)
	case ".java":
		return resolveJavaTypeCheck(baseDir, filePath)
	default:
		return nil, ""
	}
}

func resolveGoTypeCheck(baseDir, filePath string) (*typeCheckCommand, string) {
	goCmd := firstAvailableExecutable("go", "/usr/local/go/bin/go")
	if goCmd == "" {
		return nil, "No Go toolchain found for type checking."
	}

	moduleFile := findUpFile(filepath.Dir(filePath), baseDir, "go.mod")
	if moduleFile != "" {
		moduleDir := filepath.Dir(moduleFile)
		return &typeCheckCommand{
			Dir:  moduleDir,
			Name: goCmd,
			Args: []string{"build", "./..."},
		}, ""
	}

	return &typeCheckCommand{
		Dir:  filepath.Dir(filePath),
		Name: goCmd,
		Args: []string{"build", filepath.Base(filePath)},
	}, ""
}

func resolvePythonTypeCheck(baseDir, filePath string) (*typeCheckCommand, string) {
	startDir := filepath.Dir(filePath)
	checkDir := startDir
	if config := findUpFile(startDir, baseDir, "pyrightconfig.json", "mypy.ini", "setup.cfg", "pyproject.toml"); config != "" {
		checkDir = filepath.Dir(config)
	}
	target := relativeToDir(checkDir, filePath)

	if pyright := firstExistingPath(
		findUpFile(startDir, baseDir, filepath.Join("node_modules", ".bin", platformExecutableName("pyright"))),
		firstAvailableExecutable("pyright"),
	); pyright != "" {
		return &typeCheckCommand{
			Dir:  checkDir,
			Name: pyright,
			Args: []string{target},
		}, ""
	}

	if mypy := firstExistingPath(
		findUpFile(startDir, baseDir,
			filepath.Join(".venv", platformBinDir(), platformExecutableName("mypy")),
			filepath.Join("venv", platformBinDir(), platformExecutableName("mypy")),
		),
		firstAvailableExecutable("mypy"),
	); mypy != "" {
		return &typeCheckCommand{
			Dir:  checkDir,
			Name: mypy,
			Args: []string{target},
		}, ""
	}

	return nil, "No Python type checker found. Install pyright or mypy."
}

func resolveTypeScriptTypeCheck(baseDir, filePath string) (*typeCheckCommand, string) {
	startDir := filepath.Dir(filePath)
	tsconfig := findUpFile(startDir, baseDir, "tsconfig.json")
	if tsconfig == "" {
		return nil, "No tsconfig.json found for TypeScript type checking."
	}

	projectDir := filepath.Dir(tsconfig)
	if tsc := firstExistingPath(
		findUpFile(startDir, baseDir, filepath.Join("node_modules", ".bin", platformExecutableName("tsc"))),
		firstAvailableExecutable("tsc"),
	); tsc != "" {
		return &typeCheckCommand{
			Dir:  projectDir,
			Name: tsc,
			Args: []string{"--noEmit", "-p", filepath.Base(tsconfig)},
		}, ""
	}

	if npx := firstAvailableExecutable("npx"); npx != "" {
		return &typeCheckCommand{
			Dir:  projectDir,
			Name: npx,
			Args: []string{"tsc", "--noEmit", "-p", filepath.Base(tsconfig)},
		}, ""
	}

	return nil, "No TypeScript checker found. Install tsc or make npx available."
}

func resolveJavaTypeCheck(baseDir, filePath string) (*typeCheckCommand, string) {
	startDir := filepath.Dir(filePath)
	if pom := findUpFile(startDir, baseDir, "pom.xml"); pom != "" {
		projectDir := filepath.Dir(pom)
		if wrapper := firstExistingPath(filepath.Join(projectDir, platformWrapperName("mvnw"))); wrapper != "" {
			return &typeCheckCommand{
				Dir:  projectDir,
				Name: wrapper,
				Args: []string{"-q", "-DskipTests", "compile"},
			}, ""
		}
		if mvn := firstAvailableExecutable("mvn"); mvn != "" {
			return &typeCheckCommand{
				Dir:  projectDir,
				Name: mvn,
				Args: []string{"-q", "-DskipTests", "compile"},
			}, ""
		}
		return nil, "No Maven executable found for Java type checking."
	}

	if gradleBuild := findUpFile(startDir, baseDir, "build.gradle", "build.gradle.kts"); gradleBuild != "" {
		projectDir := filepath.Dir(gradleBuild)
		if wrapper := firstExistingPath(filepath.Join(projectDir, platformWrapperName("gradlew"))); wrapper != "" {
			return &typeCheckCommand{
				Dir:  projectDir,
				Name: wrapper,
				Args: []string{"--quiet", "compileJava"},
			}, ""
		}
		if gradle := firstAvailableExecutable("gradle"); gradle != "" {
			return &typeCheckCommand{
				Dir:  projectDir,
				Name: gradle,
				Args: []string{"--quiet", "compileJava"},
			}, ""
		}
		return nil, "No Gradle executable found for Java type checking."
	}

	if javac := firstAvailableExecutable("javac"); javac != "" {
		return &typeCheckCommand{
			Dir:  filepath.Dir(filePath),
			Name: javac,
			Args: []string{filepath.Base(filePath)},
		}, ""
	}

	return nil, "No Java type checker found. Install javac or use Maven/Gradle."
}

func firstAvailableExecutable(names ...string) string {
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if filepath.IsAbs(name) {
			if _, err := os.Stat(name); err == nil {
				return name
			}
			continue
		}
		if path, err := checkTypesLookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func firstExistingPath(paths ...string) string {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func findUpFile(startDir, baseDir string, names ...string) string {
	current := startDir
	baseDir = filepath.Clean(baseDir)
	for {
		for _, name := range names {
			candidate := filepath.Join(current, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		if samePath(current, baseDir) {
			return ""
		}
		parent := filepath.Dir(current)
		if samePath(parent, current) {
			return ""
		}
		current = parent
	}
}

func relativeToDir(dir, target string) string {
	relative, err := filepath.Rel(dir, target)
	if err != nil {
		return target
	}
	return relative
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func platformExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".cmd"
	}
	return name
}

func platformWrapperName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".cmd"
	}
	return name
}

func platformBinDir() string {
	if runtime.GOOS == "windows" {
		return "Scripts"
	}
	return "bin"
}
