package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/python"
)

const (
	repoMapMaxSymbols      = 200
	repoMapMaxOutputChars  = 30000
	repoMapDocPreviewRunes = 160
)

type RepoMapTool struct {
	queries map[string]*sitter.Query
	errors  map[string]error
}

type repoMapLanguageSpec struct {
	Name              string
	Query             string
	Language          *sitter.Language
	LineCommentPrefix string
	BlockComments     bool
}

type repoMapSymbol struct {
	Kind string
	Name string
	Line int
	Doc  string
}

var repoMapSpecs = map[string]repoMapLanguageSpec{
	".go": {
		Name: "go",
		Query: `
			(type_spec name: (_) @type.name) @type.def
			(function_declaration name: (_) @func.name) @func.def
			(method_declaration name: (_) @method.name) @method.def
		`,
		Language:          golang.GetLanguage(),
		LineCommentPrefix: "//",
		BlockComments:     true,
	},
	".py": {
		Name: "python",
		Query: `
			(class_definition name: (_) @class.name) @class.def
			(function_definition name: (_) @func.name) @func.def
		`,
		Language:          python.GetLanguage(),
		LineCommentPrefix: "#",
	},
	".java": {
		Name: "java",
		Query: `
			(class_declaration name: (_) @class.name) @class.def
			(interface_declaration name: (_) @interface.name) @interface.def
			(enum_declaration name: (_) @enum.name) @enum.def
			(record_declaration name: (_) @record.name) @record.def
			(annotation_type_declaration name: (_) @annotation_type.name) @annotation_type.def
			(method_declaration name: (_) @method.name) @method.def
			(constructor_declaration name: (_) @ctor.name) @ctor.def
			(field_declaration declarator: (variable_declarator name: (_) @field.name)) @field.def
		`,
		Language:          java.GetLanguage(),
		LineCommentPrefix: "//",
		BlockComments:     true,
	},
}

func NewRepoMapTool() *RepoMapTool {
	tool := &RepoMapTool{
		queries: make(map[string]*sitter.Query),
		errors:  make(map[string]error),
	}
	for ext, spec := range repoMapSpecs {
		query, err := sitter.NewQuery([]byte(spec.Query), spec.Language)
		if err != nil {
			tool.errors[ext] = fmt.Errorf("compile %s query: %w", spec.Name, err)
			continue
		}
		tool.queries[ext] = query
	}
	return tool
}

func (r *RepoMapTool) Name() string {
	return "repo_map"
}

func (r *RepoMapTool) Description() string {
	return "Return a semantic outline for one code file. Use this before reading a large unfamiliar file so you can see its types, classes, functions, methods, and nearby doc comments."
}

func (r *RepoMapTool) Call(ctx context.Context, input string, agent *Agent) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		log.Printf("[RepoMapTool] agent=%s Error parsing input: %v", agentLogName(agent), err)
		return "", fmt.Errorf("invalid input: %v", err)
	}
	if strings.TrimSpace(params.Path) == "" {
		return "", fmt.Errorf("path is required")
	}

	log.Printf("[RepoMapTool] agent=%s Analyzing file: %s", agentLogName(agent), params.Path)

	baseDir := agentWorkspaceDir(agent)
	filePath, err := safePath(baseDir, params.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", params.Path)
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	spec, ok := repoMapLanguageSpecForPath(params.Path)
	if !ok {
		return fmt.Sprintf("Unsupported file type for semantic mapping: %s", filepath.Ext(params.Path)), nil
	}
	query, err := r.queryForExtension(strings.ToLower(filepath.Ext(params.Path)))
	if err != nil {
		return "", err
	}

	parser := sitter.NewParser()
	parser.SetLanguage(spec.Language)
	tree, err := parser.ParseCtx(ctx, nil, content)
	if err != nil {
		return "", fmt.Errorf("parse file: %w", err)
	}
	if tree == nil {
		return "", fmt.Errorf("parse file: empty syntax tree")
	}

	cursor := sitter.NewQueryCursor()
	cursor.Exec(query, tree.RootNode())

	lines := strings.Split(string(content), "\n")
	symbols := make([]repoMapSymbol, 0, 32)
	for {
		match, ok := cursor.NextMatch()
		if !ok {
			break
		}

		symbol := repoMapSymbol{}
		var defNode *sitter.Node
		for _, capture := range match.Captures {
			name := query.CaptureNameForId(capture.Index)
			switch {
			case strings.HasSuffix(name, ".name"):
				symbol.Kind = strings.TrimSuffix(name, ".name")
				symbol.Name = strings.TrimSpace(capture.Node.Content(content))
				symbol.Line = int(capture.Node.StartPoint().Row) + 1
			case strings.HasSuffix(name, ".def"):
				if symbol.Kind == "" {
					symbol.Kind = strings.TrimSuffix(name, ".def")
				}
				if symbol.Line == 0 {
					symbol.Line = int(capture.Node.StartPoint().Row) + 1
				}
				defNode = capture.Node
			}
		}
		if symbol.Kind == "" || symbol.Name == "" {
			continue
		}
		if defNode != nil {
			symbol.Doc = repoMapDocPreview(extractLeadingDoc(lines, int(defNode.StartPoint().Row), spec))
		}
		symbols = append(symbols, symbol)
	}

	sort.SliceStable(symbols, func(i, j int) bool {
		if symbols[i].Line == symbols[j].Line {
			if symbols[i].Kind == symbols[j].Kind {
				return symbols[i].Name < symbols[j].Name
			}
			return symbols[i].Kind < symbols[j].Kind
		}
		return symbols[i].Line < symbols[j].Line
	})

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Semantic outline for %s (%s)\n", params.Path, spec.Name))
	if len(symbols) == 0 {
		result.WriteString("(no matching symbols found)")
		return result.String(), nil
	}

	limit := len(symbols)
	if limit > repoMapMaxSymbols {
		limit = repoMapMaxSymbols
	}
	for i := 0; i < limit; i++ {
		symbol := symbols[i]
		result.WriteString(fmt.Sprintf("- [%s] %s (line %d)", symbol.Kind, symbol.Name, symbol.Line))
		if symbol.Doc != "" {
			result.WriteString(" - ")
			result.WriteString(symbol.Doc)
		}
		result.WriteByte('\n')
		if result.Len() >= repoMapMaxOutputChars {
			result.WriteString("... (output truncated)\n")
			break
		}
	}
	if len(symbols) > limit {
		result.WriteString(fmt.Sprintf("... (%d more symbols omitted)\n", len(symbols)-limit))
	}

	summary := strings.TrimRight(result.String(), "\n")
	log.Printf("[RepoMapTool] agent=%s Analysis complete (symbols=%d length=%d)", agentLogName(agent), len(symbols), len(summary))
	return summary, nil
}

func (r *RepoMapTool) queryForExtension(ext string) (*sitter.Query, error) {
	query := r.queries[ext]
	err := r.errors[ext]

	if err != nil {
		return nil, err
	}
	if query == nil {
		return nil, fmt.Errorf("no compiled query for %s", ext)
	}
	return query, nil
}

func repoMapLanguageSpecForPath(path string) (repoMapLanguageSpec, bool) {
	spec, ok := repoMapSpecs[strings.ToLower(filepath.Ext(path))]
	return spec, ok
}

func extractLeadingDoc(lines []string, defRow int, spec repoMapLanguageSpec) string {
	if defRow <= 0 || defRow > len(lines)-1 {
		return ""
	}

	idx := defRow - 1
	if idx < 0 {
		return ""
	}
	line := strings.TrimSpace(lines[idx])
	if line == "" {
		return ""
	}

	switch {
	case spec.LineCommentPrefix != "" && strings.HasPrefix(line, spec.LineCommentPrefix):
		return extractLineCommentBlock(lines, idx, spec.LineCommentPrefix)
	case spec.BlockComments && strings.Contains(line, "*/"):
		return extractBlockComment(lines, idx)
	default:
		return ""
	}
}

func extractLineCommentBlock(lines []string, idx int, prefix string) string {
	var block []string
	for idx >= 0 {
		line := strings.TrimSpace(lines[idx])
		if !strings.HasPrefix(line, prefix) {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		block = append(block, text)
		idx--
	}
	reverseStrings(block)
	return strings.TrimSpace(strings.Join(block, "\n"))
}

func extractBlockComment(lines []string, idx int) string {
	var block []string
	for idx >= 0 {
		line := strings.TrimSpace(lines[idx])
		block = append(block, line)
		if strings.Contains(line, "/*") {
			break
		}
		idx--
	}
	reverseStrings(block)
	return normalizeBlockComment(strings.Join(block, "\n"))
}

func normalizeBlockComment(text string) string {
	text = strings.ReplaceAll(text, "/*", "")
	text = strings.ReplaceAll(text, "*/", "")
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "*")
		parts[i] = strings.TrimSpace(part)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func repoMapDocPreview(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= repoMapDocPreviewRunes {
		return text
	}
	return string(runes[:repoMapDocPreviewRunes]) + "..."
}
