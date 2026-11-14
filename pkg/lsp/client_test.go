package lsp

import (
	"testing"
)

func mockClient(t *testing.T, l *loop, rt *Runtime, dir, script string, extra map[string]string) *Client {
	t.Helper()
	return newClient(l, rt, ClientOptions{ServerID: "mock-" + script, WorkspaceRoot: dir, Command: mockCommand(t),
		Env: mockEnv(script, extra), Cwd: dir})
}
