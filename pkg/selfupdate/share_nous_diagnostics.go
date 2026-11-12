package selfupdate

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Collects the force redacted debug bundle, adds the client files, gzips the Nous envelope and uploads it
func ShareNousDiagnostics(ctx context.Context, logLines int, clientFiles [][2]string) (*pyjson.Object, error) {
	bundle, err := debugCollectShareBundle(ctx, logLines, true)
	if err != nil {
		return nil, err
	}
	for _, file := range clientFiles {
		bundle.set(file[0], file[1])
	}
	data, err := debugBuildNousBundle(bundle, true, time.Now())
	if err != nil {
		return nil, err
	}
	return debugShareToNous(ctx, data)
}
