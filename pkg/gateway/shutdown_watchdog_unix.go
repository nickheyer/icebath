//go:build !windows

package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// AF_UNIX loop tick listener at the pid suffixed state path, not handled when the path overflows sun_path
func listenTickUnix(ctx context.Context, h string) (net.Listener, string, bool, error) {
	path := GetLoopTickSocketPath(ctx, h, nil)
	fits, err := fitsSunPath(path)
	if err != nil || !fits {
		return nil, "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, path, true, err
	}
	sweepStaleTickSockets(path)
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		err = os.Remove(path)
		if err != nil {
			slog.Error(fmt.Sprintf("Unable to check or remove stale UNIX socket %s: %v", pyrepr.StrRepr(path), err))
		}
	} else if err != nil && !os.IsNotExist(err) {
		slog.Error(fmt.Sprintf("Unable to check or remove stale UNIX socket %s: %v", pyrepr.StrRepr(path), err))
	}
	l, err := net.Listen("unix", path)
	return l, path, true, err
}

// Unlinks loop tick socket nodes beside ownPath whose pid is dead or unparsable, never failing
func sweepStaleTickSockets(ownPath string) {
	dir := filepath.Dir(ownPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if ok, _ := filepath.Match("gateway.loop-tick.*.sock", e.Name()); ok && filepath.Join(dir, e.Name()) != ownPath {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		parts := strings.Split(name, ".")
		n, ok := pyint.Parse(parts[len(parts)-2])
		if ok && (!n.IsInt64() || n.Int64() > math.MaxInt32 || n.Int64() < math.MinInt32) {
			slog.Debug(fmt.Sprintf("stale loop-tick socket sweep failed: signed integer is greater than maximum (%s)", name))
			return
		}
		if ok && syscall.Kill(int(n.Int64()), 0) == nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			slog.Debug(fmt.Sprintf("stale loop-tick socket sweep failed: %v", err))
			return
		}
	}
}
