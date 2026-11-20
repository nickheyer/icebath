package provider

// AgentNotice is an out of band notice each driver renders its own way
type AgentNotice struct {
	Text  string
	Level string
	Kind  string
	TTLMs int
	Key   string
	ID    string
}
