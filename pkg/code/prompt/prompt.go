package prompt

import (
	"bytes"
	_ "embed"
	"strings"
	"text/template"
)

//go:embed mode_agent.txt
var Instructions string

//go:embed mode_plan.txt
var Planning string

//go:embed section_environment.txt
var sectionEnvironment string

//go:embed section_memory.txt
var sectionMemory string

//go:embed section_plan.txt
var sectionPlan string

//go:embed section_skills.txt
var sectionSkills string

//go:embed section_project.txt
var sectionProject string

// BoundaryMarker separates the static cacheable prefix from the dynamic
// per-turn suffix. Plain text (no markdown) so it serves as a deterministic
// prefix terminator for any caching layer.
const BoundaryMarker = "--- session context ---"

// Static sections live in the cacheable prefix. They change only when the
// session's on-disk state changes (AGENTS.md / skill (un)install / MEMORY.md
// edit). The caller is responsible for mtime-tracking these so the rendered
// string is byte-stable across turns when the source files haven't changed.
var staticTemplates = []struct {
	title string
	tmpl  *template.Template
}{
	{"Project Guidelines", template.Must(template.New("project").Parse(sectionProject))},
	{"Skills", template.Must(template.New("skills").Parse(sectionSkills))},
	{"Memory", template.Must(template.New("memory").Parse(sectionMemory))},
}

// Dynamic sections sit after the boundary. They change per turn — Date rolls
// daily; Plan is mode-gated.
var dynamicTemplates = []struct {
	title string
	tmpl  *template.Template
}{
	{"Session Plan", template.Must(template.New("plan").Parse(sectionPlan))},
	{"Environment", template.Must(template.New("environment").Parse(sectionEnvironment))},
}

type SectionData struct {
	PlanMode            bool
	Date                string
	OS                  string
	Arch                string
	WorkingDir          string
	MemoryDir           string
	MemoryContent       string
	Skills              string
	ProjectInstructions string
}

type Section struct {
	Title   string
	Content string
}

func renderSections(templates []struct {
	title string
	tmpl  *template.Template
}, data SectionData) []Section {
	var sections []Section

	for _, st := range templates {
		var buf bytes.Buffer

		if err := st.tmpl.Execute(&buf, data); err != nil {
			continue
		}

		if content := strings.TrimSpace(buf.String()); content != "" {
			sections = append(sections, Section{Title: st.title, Content: content})
		}
	}

	return sections
}

// RenderSections returns every renderable section regardless of static/dynamic
// classification. Retained for tests and callers that need a flat list.
func RenderSections(data SectionData) []Section {
	return append(renderSections(staticTemplates, data), renderSections(dynamicTemplates, data)...)
}

// BuildInstructions assembles the system prompt with a cacheable static prefix
// followed by a boundary marker and a dynamic suffix.
func BuildInstructions(base string, data SectionData) string {
	var staticParts []Section
	staticParts = append(staticParts, Section{Content: base})
	staticParts = append(staticParts, renderSections(staticTemplates, data)...)

	dynamicParts := renderSections(dynamicTemplates, data)

	staticBlock := composeSections(staticParts)
	dynamicBlock := composeSections(dynamicParts)

	if dynamicBlock == "" {
		return staticBlock
	}
	if staticBlock == "" {
		return dynamicBlock
	}
	return staticBlock + "\n\n" + BoundaryMarker + "\n\n" + dynamicBlock
}

func ComposeSections(sections ...Section) string {
	return composeSections(sections)
}

func composeSections(sections []Section) string {
	var parts []string

	for _, section := range sections {
		content := strings.TrimSpace(section.Content)
		if content == "" {
			continue
		}

		if section.Title != "" {
			parts = append(parts, "## "+section.Title+"\n\n"+content)
			continue
		}

		parts = append(parts, content)
	}

	return strings.Join(parts, "\n\n")
}
