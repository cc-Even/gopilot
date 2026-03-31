package agents

type AgentOptions struct {
	Desc        string
	ToolList    []ToolDefinition
	BaseUrl     string
	ApiKey      string
	SubAgents   map[string]*Agent
	SkillLoader *SkillLoader
}

type AgentOption func(*AgentOptions)

func WithDesc(desc string) AgentOption {
	return func(o *AgentOptions) {
		o.Desc = desc
	}
}

func WithToolList(toolList []ToolDefinition) AgentOption {
	return func(o *AgentOptions) {
		o.ToolList = toolList
	}
}

func WithBaseUrl(baseUrl string) AgentOption {
	return func(o *AgentOptions) {
		o.BaseUrl = baseUrl
	}
}

func WithApiKey(apiKey string) AgentOption {
	return func(o *AgentOptions) {
		o.ApiKey = apiKey
	}
}

func WithSubAgents(SubAgents map[string]*Agent) AgentOption {
	return func(o *AgentOptions) {
		o.SubAgents = SubAgents
	}
}

func WithSkillLoader(skillLoader *SkillLoader) AgentOption {
	return func(o *AgentOptions) {
		o.SkillLoader = skillLoader
	}
}
