package code

type Model struct {
	ID   string
	Name string
}

var AvailableModels = []Model{
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},

	{ID: "gpt-5.5", Name: "GPT 5.5"},
	{ID: "gpt-5.4", Name: "GPT 5.4"},

	{ID: "gpt-5.3-codex", Name: "GPT 5.3 Codex"},
	{ID: "gpt-5.2-codex", Name: "GPT 5.2 Codex"},

	{ID: "claude-opus-4-8", Name: "Claude Opus 4.8"},
	{ID: "claude-opus-4-7", Name: "Claude Opus 4.7"},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
	{ID: "claude-opus-4-5", Name: "Claude Opus 4.5"},

	{ID: "claude-fable-5", Name: "Claude Fable 5"},
	{ID: "claude-mythos-5", Name: "Claude Mythos 5"},
}

func ModelName(id string) string {
	for _, m := range AvailableModels {
		if m.ID == id {
			return m.Name
		}
	}
	return id
}
