package code

type Model struct {
	ID   string
	Name string
}

var AvailableModels = []Model{
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5"},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},

	{ID: "gpt-5.6-sol", Name: "GPT 5.6 Sol"},
	{ID: "gpt-5.6-terra", Name: "GPT 5.6 Terra"},
	{ID: "gpt-5.6-luna", Name: "GPT 5.6 Luna"},

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

	{ID: "glm-5.2", Name: "GLM 5.2"},
	{ID: "glm-5.1", Name: "GLM 5.1"},

	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro"},
	{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash"},
}

func ModelName(id string) string {
	for _, m := range AvailableModels {
		if m.ID == id {
			return m.Name
		}
	}
	return id
}
