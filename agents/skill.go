package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Skill struct {
	Meta map[string]string
	Body string
	Path string
}

// SkillLoader scans skills/<name>/SKILL.md and exposes description/content views.
type SkillLoader struct {
	skillsDir string
	skills    map[string]Skill
	order     []string
}

func NewSkillLoader(skillsDir string) *SkillLoader {
	l := &SkillLoader{
		skillsDir: skillsDir,
		skills:    make(map[string]Skill),
	}
	l.loadAll()
	return l
}

func (l *SkillLoader) loadAll() {
	if l == nil || l.skillsDir == "" {
		return
	}
	info, err := os.Stat(l.skillsDir)
	if err != nil || !info.IsDir() {
		return
	}

	var files []string
	_ = filepath.WalkDir(l.skillsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "SKILL.md" {
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
		name := meta["name"]
		if name == "" {
			name = filepath.Base(filepath.Dir(f))
		}

		if _, exists := l.skills[name]; !exists {
			l.order = append(l.order, name)
		}
		l.skills[name] = Skill{
			Meta: meta,
			Body: body,
			Path: f,
		}
	}
}

func parseFrontmatter(text string) (map[string]string, string) {
	frontmatterRe := regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)$`)
	match := frontmatterRe.FindStringSubmatch(text)
	if len(match) != 3 {
		return map[string]string{}, text
	}

	meta := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(match[1]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key != "" {
			meta[key] = val
		}
	}

	return meta, strings.TrimSpace(match[2])
}

func (l *SkillLoader) GetDescriptions() string {
	if l == nil || len(l.skills) == 0 {
		return "(no skills available)"
	}

	lines := make([]string, 0, len(l.skills))
	for _, name := range l.order {
		skill := l.skills[name]
		desc := skill.Meta["description"]
		if desc == "" {
			desc = "No description"
		}
		tags := skill.Meta["tags"]

		line := fmt.Sprintf("  - %s: %s", name, desc)
		if tags != "" {
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (l *SkillLoader) GetContent(name string) string {
	if l == nil {
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: ", name)
	}
	skill, ok := l.skills[name]
	if !ok {
		available := append([]string(nil), l.order...)
		sort.Strings(available)
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, strings.Join(available, ", "))
	}
	return fmt.Sprintf("<skill name=\"%s\">\n%s\n</skill>", name, skill.Body)
}
