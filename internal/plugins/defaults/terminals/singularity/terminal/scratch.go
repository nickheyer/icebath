package terminal

import (
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	singularityconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/singularity/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// How long a verdict on the scratch folder's size holds
const usageTTL = 5 * time.Minute

// Folder of overlays and the image cache: the setting's, else a writable /scratch of the user, else the sandboxes folder's singularity
func scratchDir(cfg *config.Config) (string, error) {
	scratch := terminal.ExpandPath(strings.TrimSpace(cfg.GetString(singularityconfig.ScratchDir)))
	if scratch == "" {
		sandboxes, err := terminal.SandboxDir(cfg)
		if err != nil {
			return "", err
		}
		scratch = filepath.Join(sandboxes, "singularity")
		if st, err := os.Stat("/scratch"); err == nil && st.IsDir() && terminal.Writable("/scratch") {
			user, ok := os.LookupEnv("USER")
			if !ok {
				user = "icebath"
			}
			scratch = filepath.Join("/scratch", user, "icebath-agent")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				return "", err
			}
			slog.Info(fmt.Sprintf("Using /scratch for sandboxes: %s", scratch))
		}
	}
	return scratch, os.MkdirAll(scratch, 0o755)
}

// Folder the .sif images are kept in: the setting's, else .apptainer in the scratch folder
func cacheDir(cfg *config.Config) (string, error) {
	cache := terminal.ExpandPath(strings.TrimSpace(cfg.GetString(singularityconfig.CacheDir)))
	if cache == "" {
		scratch, err := scratchDir(cfg)
		if err != nil {
			return "", err
		}
		cache = filepath.Join(scratch, ".apptainer")
	}
	return cache, os.MkdirAll(cache, 0o755)
}

// Every icebath entry of a scratch folder, the overlays included
func entries(scratch string) []string {
	found, _ := filepath.Glob(filepath.Join(scratch, "icebath-*"))
	return found
}

// Notes a scratch folder an instance used, swept as icebath stops
func (r *registrant) remember(scratch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.scratches, scratch) {
		r.scratches = append(r.scratches, scratch)
	}
}

// Bytes of every regular file under root, links to files followed
func treeBytes(root string) int64 {
	var total int64
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return 0
	}
	filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
		return nil
	})
	return total
}

// Warns when the scratch folder's sandboxes take more than the disk warning, the verdict held five minutes, never after a failed scan
func (r *registrant) checkUsage(cfg *config.Config) bool {
	r.mu.Lock()
	if r.usage.set && time.Since(r.usage.stamp) < usageTTL {
		over := r.usage.over
		r.mu.Unlock()
		return over
	}
	r.mu.Unlock()
	scratch, err := scratchDir(cfg)
	if err != nil {
		slog.Debug("Disk usage warning check failed", "error", err)
		return false
	}
	var total int64
	for _, p := range entries(scratch) {
		total += treeBytes(p)
	}
	gigabytes := float64(total) / math.Pow(1024, 3)
	threshold := cast.ToFloat64(cfg.Get(singularityconfig.DiskWarning))
	over := gigabytes > threshold
	if over {
		slog.Warn(fmt.Sprintf("Singularity scratch disk usage (%.1fGB) exceeds threshold (%.0fGB).", gigabytes, threshold))
	}
	r.mu.Lock()
	r.usage.stamp, r.usage.set, r.usage.over = time.Now(), true, over
	r.mu.Unlock()
	return over
}
