package selfupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Hidden entry the install binary answers the import health probe on
const ImportHealthEntry = pm.ImportHealthEntry

// Runner of the import health probe child and the builder of its install bound command
var (
	importHealthRun     = pysubprocess.RunWith
	importHealthCommand = pm.InstallRuntimeCommand
)

// Answers the import health probe with the marker and the empty failure list of a binary that started whole
func ImportHealthMain(argv []string, stdout io.Writer) int {
	if len(argv) != 2 || (argv[1] != "True" && argv[1] != "False") {
		return 2
	}
	if _, err := io.WriteString(stdout, "\n"+argv[0]+"[]"); err != nil {
		return 1
	}
	return 0
}

// Starts the install's own binary on the probe entry and returns its import failures in probe order
func criticalModuleImportFailures(ctx context.Context, root string, reportRuntimeErrors bool) ([]string, map[string][2]string, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, nil, err
	}
	marker := "__ICEBATH_IMPORT_HEALTH_" + hex.EncodeToString(token) + "__"
	command := importHealthCommand(root, []string{marker, pyrepr.Repr(reportRuntimeErrors)}, pm.LaunchCommandOptions{Subcommand: []string{ImportHealthEntry}})
	result, err := importHealthRun(ctx, command, pysubprocess.Options{Dir: root, Timeout: 120 * time.Second})
	if err != nil {
		var expired *pysubprocess.TimeoutExpired
		if errors.As(err, &expired) {
			order, failures := probeFailure("TimeoutExpired", "timed out before reporting import health")
			return order, failures, nil
		}
		if isOSOrSubprocessError(err) {
			return []string{}, map[string][2]string{}, nil
		}
		return nil, nil, err
	}
	output := pystr.DecodeUTF8Replace(result.Stdout)
	cut := strings.LastIndex(output, marker)
	if cut < 0 {
		order, failures := probeFailure("ProbeTerminated", fmt.Sprintf("terminated before reporting import health (exit code %d)", result.Returncode))
		return order, failures, nil
	}
	order, failures, ok := importHealthPayload(output[cut+len(marker):])
	if !ok {
		order, failures = probeFailure("MalformedPayload", "reported malformed import health data")
	}
	return order, failures, nil
}

// Decodes the probe's JSON list of module, class and text triples, false when it is any other shape
func importHealthPayload(text string) ([]string, map[string][2]string, bool) {
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, false
	}
	rows, ok := decoded.([]any)
	if !ok {
		return nil, nil, false
	}
	order := []string{}
	failures := map[string][2]string{}
	for _, row := range rows {
		item, ok := row.([]any)
		if !ok || len(item) != 3 {
			return nil, nil, false
		}
		var fields [3]string
		for i, value := range item {
			if fields[i], ok = value.(string); !ok {
				return nil, nil, false
			}
		}
		if _, seen := failures[fields[0]]; !seen {
			order = append(order, fields[0])
		}
		failures[fields[0]] = [2]string{fields[1], fields[2]}
	}
	return order, failures, true
}

// Failure row for the probe itself rather than a module it loaded
func probeFailure(kind, detail string) ([]string, map[string][2]string) {
	return []string{"critical-module probe"}, map[string][2]string{"critical-module probe": {kind, detail}}
}

// The first critical module import failure, ok when every module loads
func validateCriticalModulesImport(ctx context.Context, root string, reportRuntimeErrors bool) (bool, string, string, error) {
	order, failures, err := criticalModuleImportFailures(ctx, root, reportRuntimeErrors)
	if err != nil {
		return false, "", "", err
	}
	if len(order) > 0 {
		return false, order[0], failures[order[0]][1], nil
	}
	return true, "", "", nil
}
