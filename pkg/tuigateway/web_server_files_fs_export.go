package tuigateway

import "context"

// Context bound to the profile whose sandbox a container path names, its home and terminal policy
func AgentPathScope(ctx context.Context, profileHome string) context.Context {
	return agentPathScope(ctx, profileHome)
}

// Native path a file URL names decoded once, a ValueError for an encoded separator or a foreign host
func FileURLPath(url string) (string, error) { return fileURLPath(url) }
