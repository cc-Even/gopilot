package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SubAgentSpec struct {
	Name         string
	Description  string
	SystemPrompt string
	Model        string
	Path         string
}

// SubAgentLoader scans subagents/<name>/SUBAGENT.md and builds sub-agent specs.
type SubAgentLoader struct {
	subAgentsDir string
	specs        map[string]SubAgentSpec
	order        []string
}

func NewSubAgentLoader(subAgentsDir string) *SubAgentLoader {
	l := &SubAgentLoader{
		subAgentsDir: subAgentsDir,
		specs:        make(map[string]SubAgentSpec),
	}
	l.loadAll()
	return l
}

func (l *SubAgentLoader) loadAll() {
	if l == nil || l.subAgentsDir == "" {
		return
	}
	info, err := os.Stat(l.subAgentsDir)
	if err != nil || !info.IsDir() {
		return
	}

	var files []string
	_ = filepath.WalkDir(l.subAgentsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "SUBAGENT.md" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		meta, body := parseFrontmatter(string(raw))
		name := strings.TrimSpace(meta["name"])
		if name == "" {
			name = filepath.Base(filepath.Dir(f))
		}
		if name == "" {
			continue
		}

		if _, exists := l.specs[name]; !exists {
			l.order = append(l.order, name)
		}
		l.specs[name] = SubAgentSpec{
			Name:         name,
			Description:  strings.TrimSpace(meta["description"]),
			SystemPrompt: strings.TrimSpace(body),
			Model:        strings.TrimSpace(meta["model"]),
			Path:         f,
		}
	}
}

func (l *SubAgentLoader) GetDescriptions() string {
	if l == nil || len(l.specs) == 0 {
		return "(no sub-agents available)"
	}

	lines := make([]string, 0, len(l.order))
	for _, name := range l.order {
		spec := l.specs[name]
		desc := spec.Description
		if desc == "" {
			desc = "No description"
		}
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if spec.Model != "" {
			line += fmt.Sprintf(" [model=%s]", spec.Model)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (l *SubAgentLoader) BuildAgents(baseModel string, toolList []ToolDefinition, skillLoader *SkillLoader) map[string]*Agent {
	if l == nil || len(l.specs) == 0 {
		return nil
	}

	agents := make(map[string]*Agent, len(l.specs))
	for _, name := range l.order {
		spec := l.specs[name]
		model := spec.Model
		if model == "" {
			model = baseModel
		}
		systemPrompt := spec.SystemPrompt
		if systemPrompt == "" {
			systemPrompt = fmt.Sprintf("You are the %s sub-agent at %s.", spec.Name, WORKDIR)
		}
		agents[spec.Name] = NewAgent(
			spec.Name,
			systemPrompt,
			model,
			WithDesc(spec.Description),
			WithToolList(toolList),
			WithSkillLoader(skillLoader),
		)
		agents[spec.Name].InheritModel = spec.Model == ""
	}
	return agents
}
