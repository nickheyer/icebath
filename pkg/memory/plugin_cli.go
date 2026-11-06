package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Plugin host seams for a provider cli.py and config_schema.py, swapped by tests
var (
	describeProviderCLI  = plugins.DescribeMemoryProviderCLI
	runProviderCLI       = plugins.RunMemoryProviderCLI
	providerConfigSchema = plugins.MemoryProviderConfigSchema
)

// Nonzero status a provider CLI handler or its argparse parser exited with
type PluginCLIExit struct{ Code int }

func (e *PluginCLIExit) Error() string { return "exit " + strconv.Itoa(e.Code) }

// Process exit status the CLI maps this error to
func (e *PluginCLIExit) ExitCode() int { return e.Code }

// Whether path names a regular file or a link to one
func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// CLI a non bundled provider cli.py contributes through register_cli, nil without one or under host isolation
func hostedProviderCLI(ctx context.Context, env *tools.Env, r *ProviderRegistration) *ProviderCLI {
	if _, err := os.Stat(filepath.Join(r.Dir, "cli.py")); err != nil {
		return nil
	}
	if plugins.InProcessImportRefusal(ctx, fmt.Sprintf("memory provider %s CLI commands", pyRepr(r.Name)), r.Source, nil) != "" {
		return nil
	}
	name, dir, module, desc := r.Name, r.Dir, providerDirModuleName(r.Dir, r.Name), r.Description()
	registerCLI, handler, err := describeProviderCLI(ctx, name, dir, module)
	if err != nil {
		envLog(env).Debug(fmt.Sprintf("Failed to scan CLI for memory plugin '%s': %s", name, err))
		return nil
	}
	if !registerCLI {
		return nil
	}
	run := func(withHandler bool) func(cmd *cobra.Command, args []string) error {
		return func(cmd *cobra.Command, args []string) error {
			runCtx := cmd.Context()
			if runCtx == nil {
				runCtx = context.Background()
			}
			rc, dispatched, err := runProviderCLI(runCtx, name, dir, module, desc, withHandler, args, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if !dispatched {
				return cmd.Root().Help()
			}
			if rc != 0 {
				return &PluginCLIExit{Code: rc}
			}
			return nil
		}
	}
	cli := &ProviderCLI{Setup: func(cmd *cobra.Command) {
		cmd.DisableFlagParsing = true
		cmd.Args = cobra.ArbitraryArgs
		cmd.RunE = run(false)
	}}
	if handler {
		cli.Handler = run(true)
	}
	return cli
}

// Loader of a provider config_schema.py run in the plugin host, nil when the file is absent or its schema empty
func hostedConfigSchema(path string) func(ctx context.Context) (*ProviderConfigSchema, error) {
	return func(ctx context.Context) (*ProviderConfigSchema, error) {
		if !isRegularFile(path) {
			return nil, nil
		}
		record, err := providerConfigSchema(ctx, path)
		if err != nil || !pyrepr.Truthy(record) {
			return nil, err
		}
		m, ok := record.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(record))
		}
		return ProviderConfigSchemaFromRecord(m)
	}
}
