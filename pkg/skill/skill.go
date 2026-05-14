package skill

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when-to-use"`

	Arguments []string `yaml:"arguments"`

	Location string `yaml:"-"`

	Content string `yaml:"-"`

	Raw string `yaml:"-"`

	Bundled bool `yaml:"-"`
}

func (s *Skill) GetContent(workingDir string) (string, error) {
	if s.Content != "" {
		return s.Content, nil
	}

	if s.Location == "" {
		return "", fmt.Errorf("skill %q has no location or content", s.Name)
	}

	var path string
	if filepath.IsAbs(s.Location) {
		path = filepath.Join(s.Location, "SKILL.md")
	} else {
		path = filepath.Join(workingDir, s.Location, "SKILL.md")
	}
	return readSkillContent(path)
}

func (s *Skill) ApplyArguments(content, args, skillDir string) string {
	fields := strings.Fields(args)

	lookup := map[string]string{
		"ARGUMENTS":        args,
		"SKILL_DIR":        skillDir,
		"CLAUDE_SKILL_DIR": skillDir,
	}
	if len(s.Arguments) > 0 {
		remaining := args
		for i, name := range s.Arguments {
			if remaining == "" {
				lookup[name] = ""
				continue
			}
			if i == len(s.Arguments)-1 {
				lookup[name] = remaining
			} else {
				word, rest, _ := strings.Cut(strings.TrimSpace(remaining), " ")
				lookup[name] = word
				remaining = rest
			}
		}
	}

	matched := false
	resolve := func(name string) (string, bool) {
		if v, ok := lookup[name]; ok {
			return v, true
		}
		return "", false
	}
	resolveIdx := func(idx int) string {
		if idx >= 0 && idx < len(fields) {
			return fields[idx]
		}
		return ""
	}

	content = indexedPattern.ReplaceAllStringFunc(content, func(m string) string {
		sub := indexedPattern.FindStringSubmatch(m)
		if sub[1] != "ARGUMENTS" {
			return m
		}
		idx := atoi(sub[2])
		matched = true
		return resolveIdx(idx)
	})

	content = bracedPattern.ReplaceAllStringFunc(content, func(m string) string {
		name := bracedPattern.FindStringSubmatch(m)[1]
		if i := atoi(name); i > 0 {
			matched = true
			return resolveIdx(i - 1)
		}
		if v, ok := resolve(name); ok {
			matched = true
			return v
		}
		return m
	})

	// $KEY is word-bounded; the trailing boundary char (or end-of-input)
	// is captured and re-emitted so we don't chew it.
	content = barePattern.ReplaceAllStringFunc(content, func(m string) string {
		sub := barePattern.FindStringSubmatch(m)
		name, boundary := sub[1], sub[2]
		if i := atoi(name); i > 0 {
			matched = true
			return resolveIdx(i-1) + boundary
		}
		if v, ok := resolve(name); ok {
			matched = true
			return v + boundary
		}
		return m
	})

	if !matched && args != "" {
		content = content + "\n\nARGUMENTS: " + args
	}

	return content
}

var (
	indexedPattern = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\[(\d+)\]\}?`)
	bracedPattern  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*|\d+)\}`)
	// Trailing boundary capture prevents chewing the next char and avoids
	// re-matching the indexed form already handled by indexedPattern.
	barePattern = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*|\d+)([^A-Za-z0-9_\[]|$)`)
)

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

var skillDirs = []string{
	".agents/skills",
	".wingman/skills",
	".claude/skills",
	".opencode/skills",
}

var personalSkillRoots = []string{
	".agents/skills",
	".wingman/skills",
	".claude/skills",
	".config/opencode/skills",
}

func Discover(root string) ([]Skill, error) {
	return discover(root, skillDirs, true), nil
}

func DiscoverPersonal() ([]Skill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return discover(home, personalSkillRoots, false), nil
}

func MustDiscoverPersonal() []Skill {
	skills, _ := DiscoverPersonal()
	return skills
}

func discover(root string, dirs []string, relativeLocation bool) []Skill {
	var skills []Skill
	seen := make(map[string]bool)

	for _, dir := range dirs {
		skillDir := filepath.Join(root, dir)
		matches, err := doublestar.Glob(os.DirFS(skillDir), "*/SKILL.md")
		if err != nil {
			continue
		}

		// doublestar.Glob doesn't guarantee ordering; sort for determinism.
		sort.Strings(matches)

		for _, match := range matches {
			skillFile := filepath.Join(skillDir, match)
			sk, err := parseSkillFile(skillFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "skill: skipped %s: %v\n", skillFile, err)
				continue
			}

			if seen[strings.ToLower(sk.Name)] {
				continue
			}
			seen[strings.ToLower(sk.Name)] = true

			location := filepath.Dir(skillFile)
			if relativeLocation {
				if rel, err := filepath.Rel(root, location); err == nil {
					location = rel
				}
			}
			sk.Location = location

			skills = append(skills, sk)
		}
	}

	return skills
}

func LoadBundled(fsys fs.FS, root string) ([]Skill, error) {
	var skills []Skill

	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := root + "/" + entry.Name() + "/SKILL.md"

		data, err := fs.ReadFile(fsys, skillPath)
		if err != nil {
			continue
		}

		skill, content, err := parseSkillData(string(data))
		if err != nil {
			continue
		}

		skill.Content = content
		skill.Raw = string(data)
		skill.Bundled = true
		skills = append(skills, skill)
	}

	return skills, nil
}

func MaterializeBundled(s *Skill) (string, error) {
	if !s.Bundled || s.Raw == "" {
		return "", nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(home, ".wingman", "skills", s.Name)
	file := filepath.Join(dir, "SKILL.md")

	// Don't overwrite user customizations.
	if _, err := os.Stat(file); err == nil {
		s.Location = dir
		return dir, nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(s.Raw), 0644); err != nil {
		return "", err
	}

	s.Location = dir
	return dir, nil
}

func (s *Skill) AbsoluteDir(workDir string) string {
	if s.Location == "" {
		return ""
	}
	if filepath.IsAbs(s.Location) {
		return s.Location
	}
	return filepath.Join(workDir, s.Location)
}

func FindSkill(name string, skills []Skill) *Skill {
	lower := strings.ToLower(name)
	for i := range skills {
		if strings.ToLower(skills[i].Name) == lower {
			return &skills[i]
		}
	}
	return nil
}

func MustDiscover(root string) []Skill {
	skills, _ := Discover(root)
	return skills
}

func Merge(bundled, discovered []Skill) []Skill {
	overrides := make(map[string]bool)
	for _, s := range discovered {
		overrides[strings.ToLower(s.Name)] = true
	}

	var result []Skill
	for _, s := range bundled {
		if !overrides[strings.ToLower(s.Name)] {
			result = append(result, s)
		}
	}

	result = append(result, discovered...)
	return result
}

// FormatForPrompt only includes skills with an on-disk Location; bundled
// skills appear only after the user has invoked one (which materializes it).
// The slash-command picker UI lists everything regardless.
func FormatForPrompt(skills []Skill) string {
	var sb strings.Builder
	count := 0

	for _, s := range skills {
		if s.Location == "" {
			continue
		}
		if count == 0 {
			fmt.Fprint(&sb, "<available_skills>\n")
		}
		count++

		fmt.Fprint(&sb, "  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", s.Name)
		fmt.Fprintf(&sb, "    <description>%s</description>\n", s.Description)

		if s.WhenToUse != "" {
			fmt.Fprintf(&sb, "    <when-to-use>%s</when-to-use>\n", s.WhenToUse)
		}

		fmt.Fprintf(&sb, "    <location>%s/SKILL.md</location>\n", displayLocation(s.Location))

		fmt.Fprint(&sb, "  </skill>\n")
	}

	if count == 0 {
		return ""
	}
	fmt.Fprint(&sb, "</available_skills>")
	return sb.String()
}

// displayLocation abbreviates home-relative paths with `~` (avoids leaking
// the username) and always emits forward slashes so the model gets one
// canonical form to round-trip back to the read tool.
func displayLocation(loc string) string {
	if filepath.IsAbs(loc) {
		if home, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(home, loc); err == nil && !strings.HasPrefix(rel, "..") {
				return "~/" + filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(loc)
}

func parseSkillFile(path string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}

	skill, _, err := parseSkillData(string(data))
	return skill, err
}

func parseSkillData(data string) (Skill, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))

	var inFrontmatter bool
	var frontmatter strings.Builder
	var content strings.Builder
	var pastFrontmatter bool

	for scanner.Scan() {
		line := scanner.Text()

		if line == "---" {
			if !inFrontmatter && !pastFrontmatter {
				inFrontmatter = true
				continue
			}
			if inFrontmatter {
				inFrontmatter = false
				pastFrontmatter = true
				continue
			}
		}

		if inFrontmatter {
			frontmatter.WriteString(line)
			frontmatter.WriteString("\n")
		} else if pastFrontmatter {
			content.WriteString(line)
			content.WriteString("\n")
		}
	}

	if err := scanner.Err(); err != nil {
		return Skill{}, "", err
	}

	var skill Skill

	if err := yaml.Unmarshal([]byte(frontmatter.String()), &skill); err != nil {
		return Skill{}, "", fmt.Errorf("failed to parse frontmatter: %w", err)
	}

	if skill.Name == "" || skill.Description == "" {
		return Skill{}, "", fmt.Errorf("skill missing required fields")
	}

	return skill, strings.TrimSpace(content.String()), nil
}

func readSkillContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	_, content, err := parseSkillData(string(data))
	return content, err
}
