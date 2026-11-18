package tuiserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

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
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}, nil
}

func init() {
	setupSectionValues["terminal.plugin_backends"] = func(ctx context.Context, h string, cfg config.Tree, _ string) (any, error) {
		env, err := setupToolEnv(h, cfg, "")
		if err != nil {
			return nil, err
		}
		rows := []any{}
		for _, row := range tools.SetupTerminalBackendRows(ctx, env) {
			rows = append(rows, map[string]any{"name": row.Name, "display_name": row.DisplayName, "description": row.Description})
		}
		return rows, nil
	}
	setupSectionValues["terminal.docker"] = func(_ context.Context, h string, cfg config.Tree, _ string) (any, error) {
		env, err := setupToolEnv(h, cfg, "")
		if err != nil {
			return nil, err
		}
		path, runtime := tools.SetupDockerRuntime(env)
		return map[string]any{"path": path, "runtime": runtime}, nil
	}
	setupSectionValues["terminal.modal_managed"] = func(_ context.Context, h string, cfg config.Tree, _ string) (any, error) {
		env, err := setupToolEnv(h, cfg, "")
		if err != nil {
			return nil, err
		}
		return tools.SetupModalManagedAvailable(env, cfg)
	}
	setupSectionValues["terminal.ssh_defaults"] = func(context.Context, string, config.Tree, string) (any, error) {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return map[string]any{"user": os.Getenv("USER"), "key": filepath.Join(homeDir, ".ssh", "id_rsa")}, nil
	}
	setupSectionValues["terminal.vercel_linked"] = func(_ context.Context, _ string, _ config.Tree, cwd string) (any, error) {
		linked, err := tools.NearestVercelProject(cwd)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for key, value := range linked {
			out[key] = value
		}
		return out, nil
	}
	setupSectionValues["terminal.vercel_default_image"] = func(context.Context, string, config.Tree, string) (any, error) {
		return config.DefaultVercelImage, nil
	}
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
