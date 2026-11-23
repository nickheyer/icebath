package tuiserver

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool environment of a home over its config, rooted at the gateway directory when cwd is empty
func setupToolEnv(h string, cfg config.Tree, cwd string) (*tools.Env, error) {
	if cwd == "" {
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = dir
	}
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: pkgLog()}, nil
}

func init() {
	setupParamActions["terminal.ssh.test"] = func(ctx context.Context, _ string, params map[string]string, out io.Writer) bool {
		return tools.TestSSHConnection(ctx, out, params["host"], params["user"], params["port"], params["key"])
	}
	setupParamActions["terminal.backend.plugin"] = func(ctx context.Context, h string, params map[string]string, out io.Writer) bool {
		cfg, err := config.LoadReadonly(ctx, h, nil)
		if err != nil {
			fmt.Fprintf(out, "Backend plugin setup hook failed: %s\n", err.Error())
			return false
		}
		env, err := setupToolEnv(h, cfg, params["cwd"])
		if err != nil {
			fmt.Fprintf(out, "Backend plugin setup hook failed: %s\n", err.Error())
			return false
		}
		return tools.RunTerminalBackendSetup(ctx, env, out, params["backend"])
	}
}
