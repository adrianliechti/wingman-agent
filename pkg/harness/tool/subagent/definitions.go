package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

// Definition is a user-provided agent type: a markdown file whose frontmatter
// names the agent and whose body becomes its instructions. Access is an
// abstract tool policy ("read-only", "verify", "all"), never a tool list, and
// Model is an optional default role ("default", "complex", "utility").
type Definition struct {
	Name         string
	Description  string
	Instructions string
	Access       string
	Model        string
}

var definitionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Discover loads agent definitions from the project and the user's home
// directory; the first definition of a name wins, so project files override
// personal ones.
func Discover(workDir string) []Definition {
	var defs []Definition
	seen := map[string]string{}

	for _, dir := range layout.Roots(workDir, "agents") {
		defs = appendDefinitions(defs, seen, dir)
	}

	return defs
}

func appendDefinitions(defs []Definition, seen map[string]string, dir string) []Definition {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return defs
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, e.Name())
	}
	slices.Sort(files)

	for _, name := range files {
		path := filepath.Join(dir, name)

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		def, err := ParseDefinition(string(data))
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent: skipped %s: %v\n", path, err)
			continue
		}

		if winner, ok := seen[def.Name]; ok {
			fmt.Fprintf(os.Stderr, "agent: %s in %s is shadowed by %s\n", def.Name, path, winner)
			continue
		}
		seen[def.Name] = path

		defs = append(defs, def)
	}

	return defs
}

func ParseDefinition(data string) (Definition, error) {
	frontmatter, body, ok := splitFrontmatter(data)
	if !ok {
		return Definition{}, fmt.Errorf("missing frontmatter")
	}

	var header struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Access      string `yaml:"access"`
		Model       string `yaml:"model"`
	}
	if err := yaml.Load([]byte(frontmatter), &header); err != nil {
		return Definition{}, fmt.Errorf("failed to parse frontmatter: %w", err)
	}

	def := Definition{
		Name:         strings.ToLower(strings.TrimSpace(header.Name)),
		Description:  strings.Join(strings.Fields(header.Description), " "),
		Instructions: strings.TrimSpace(body),
	}

	if !definitionName.MatchString(def.Name) {
		return Definition{}, fmt.Errorf("missing or invalid name")
	}
	if def.Description == "" {
		return Definition{}, fmt.Errorf("missing description")
	}
	if def.Instructions == "" {
		return Definition{}, fmt.Errorf("missing instructions body")
	}

	switch strings.ToLower(strings.TrimSpace(header.Access)) {
	case "", "all":
		def.Access = "all"
	case "read-only", "readonly":
		def.Access = "read-only"
	case "verify":
		def.Access = "verify"
	default:
		return Definition{}, fmt.Errorf("unknown access %q (use read-only, verify, or all)", header.Access)
	}

	// Like the model tool parameter, an unusable model is a preference to
	// drop, not an error: .claude/agents files name concrete models.
	switch role := strings.ToLower(strings.TrimSpace(header.Model)); role {
	case "plan":
		def.Model = "complex"
	case "default", "complex", "utility":
		def.Model = role
	}

	return def, nil
}

func (d Definition) subagentType() subagentType {
	var modelRole string
	switch role := strings.ToLower(strings.TrimSpace(d.Model)); role {
	case "plan":
		modelRole = "complex"
	case "default", "complex", "utility":
		modelRole = role
	}

	st := subagentType{Instructions: d.Instructions, Model: modelRole}

	switch strings.ToLower(strings.TrimSpace(d.Access)) {
	case "read-only", "readonly":
		st.AllowTool = allowReadOnlyTool
		st.WrapDynamicReadOnly = true
		st.ReadOnly = true
	case "verify":
		st.AllowTool = allowVerificationTool
	default:
		st.AllowTool = allowNonAgentTool
	}

	return st
}

func splitFrontmatter(data string) (frontmatter, body string, ok bool) {
	data = strings.TrimPrefix(data, "\ufeff")

	var firstLine, rest string
	if before, after, found := strings.Cut(data, "\n"); found {
		firstLine = strings.TrimSuffix(before, "\r")
		rest = after
	} else {
		return "", "", false
	}

	if firstLine != "---" {
		return "", "", false
	}

	var front, remaining strings.Builder
	inFrontmatter := true
	for line := range strings.SplitSeq(rest, "\n") {
		trimmed := strings.TrimSuffix(line, "\r")
		if inFrontmatter {
			if trimmed == "---" {
				inFrontmatter = false
				continue
			}
			front.WriteString(trimmed)
			front.WriteString("\n")
		} else {
			remaining.WriteString(trimmed)
			remaining.WriteString("\n")
		}
	}

	if inFrontmatter {
		return "", "", false
	}

	return front.String(), remaining.String(), true
}
