package gateway

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// File name of the planned restart marker the next boot reads to announce the gateway is back online
const PlannedRestartMarkerFilename = ".restart_pending.json"

// Planned restart marker under the given home, the process home when empty
func PlannedRestartMarkerPath(h string) string {
	if h == "" {
		h = home.Process()
	}
	return filepath.Join(h, PlannedRestartMarkerFilename)
}

// Records that the gateway is going down for a restart, never failing the restart itself
func WritePlannedRestartMarker(viaService, detached bool) {
	payload := pyjson.NewObject()
	payload.Set("requested_at", float64(time.Now().UnixNano())/1e9)
	payload.Set("via_service", viaService)
	payload.Set("detached", detached)
	if err := config.AtomicJSONWriteWith(PlannedRestartMarkerPath(""), payload); err != nil {
		slog.Debug("Could not record the planned restart; home channels get no online notice", "error", err)
	}
}
