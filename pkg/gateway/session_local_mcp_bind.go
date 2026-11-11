package gateway

import "context"

// Policy with its editor MCP selection discovered from the servers and borrowed by the authority, unchanged without servers
func bindEditorMCP(ctx context.Context, a *SessionAuthority, sid string, policy *LocalSessionPolicy, servers any) (*LocalSessionPolicy, error) {
	if !pyTruthy(servers) {
		return policy, nil
	}
	rebound, _, err := rebindEditorMCP(ctx, a, sid, policy, servers)
	if err != nil {
		return nil, err
	}
	if err := attachEditorMCP(ctx, a, rebound, servers); err != nil {
		return nil, err
	}
	return rebound, nil
}
