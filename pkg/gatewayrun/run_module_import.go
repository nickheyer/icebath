package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Guards the one time import of the runner module, latched only once it ran whole
var runModuleImport struct {
	sync.Mutex
	done bool
}

// Runs once what importing the runner module does: the gateway marker, dotenv, the network preference and startup warnings
func ImportRunModule(ctx context.Context) error {
	runModuleImport.Lock()
	defer runModuleImport.Unlock()
	if runModuleImport.done {
		return nil
	}
	os.Setenv("_ICEBATH_GATEWAY", "1")
	processHome := home.Process()
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: processHome, ProjectEnv: filepath.Join(pm.RepoRoot(), ".env")}); err != nil {
		return err
	}
	warnOnFailure := func(label string, step func()) {
		defer func() {
			if failure := recover(); failure != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %s failed: %v\n", label, failure)
			}
		}()
		step()
	}
	warnOnFailure("IPv4 preference application", func() {
		if settings.NetworkForceIpv4.Get(home.WithOverride(ctx, processHome)) {
			netpref.ApplyIPv4Preference(true)
		}
	})
	warnOnFailure("config validation", func() {
		config.PrintConfigWarnings(ctx, home.Dir(ctx), nil, tools.RemovedBackendNote)
	})
	warnOnFailure("deprecation check", func() { config.WarnDeprecatedCwdEnvVars(os.Stderr, home.Dir(ctx)) })
	runModuleImport.done = true
	return nil
}
