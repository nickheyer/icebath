package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Planned stop marker inside a profile home, the request a pausing update leaves for its gateway
func plannedStopMarkerPath(profilePath string) string {
	return filepath.Join(profilePath, ".gateway-planned-stop.json")
}

// Writes the planned stop request naming pid into a profile home, false when the home refuses the write
func writeUpdatePlannedStopMarker(ctx context.Context, profilePath string, pid int) (bool, error) {
	body := jsonutil.NewObject()
	body.Set("target_pid", pid)
	body.Set("target_start_time", gateway.GetProcessStartTime(pid))
	body.Set("stopper_pid", os.Getpid())
	body.Set("written_at", time.Now().UTC().Format(timeutil.RFC3339Micro))
	err := config.AtomicJSONWriteWith(ctx, plannedStopMarkerPath(profilePath), body)
	if err == nil {
		return true, nil
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) {
		return false, nil
	}
	return false, err
}
