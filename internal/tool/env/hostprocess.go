package env

import "context"

func (h *Host) Shell(ctx context.Context) (string, error) {
	return h.FindShell(ctx)
}

func (h *Host) SafeCwd(cwd string) string {
	return SafeCwd(cwd)
}

func (h *Host) ChildEnv(ctx context.Context, extra map[string]string) (map[string]string, error) {
	sanitized, err := h.Sanitize(ctx, h.client.ToolchainEnviron(ctx, Environ()), extra)
	if err != nil {
		return nil, err
	}
	return h.TerminalEnv(ctx, sanitized)
}

func (h *Host) RestoreLoginPath(command, parentPath string) string {
	return DroppedPathPrelude(command, parentPath)
}
