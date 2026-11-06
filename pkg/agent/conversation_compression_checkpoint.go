package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

const checkpointRequiredRemediation = "set compression.checkpoint_required: false, or switch to a memory " +
	"provider that implements checkpoint API v2"

// Comma joined names of the active memory providers or the explicit none sentinel
func activeMemoryProviderLabel(mm *memory.MemoryManager) string {
	if mm == nil {
		return "no active provider"
	}
	var names []string
	for _, p := range mm.Providers() {
		if p == nil {
			continue
		}
		if name := pystr.Strip(p.Name()); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "no active provider"
	}
	return strings.Join(names, ", ")
}

// Capability refusal whose message carries the config level way out of the fail closed gate
func checkpointIncapable(reason string) *provider.CompressionCheckpointUnavailable {
	return provider.CheckpointBlocked(reason + ". Recover by " + checkpointRequiredRemediation)
}

// Warns at startup when fail closed compression will block because no provider implements checkpoint API v2
func warnCheckpointRequiredWithoutCapableProvider(required bool, mm *memory.MemoryManager) {
	if !required {
		return
	}
	if mm != nil {
		supported, probeErr := probeCheckpointSupport(mm)
		if probeErr != nil {
			slog.Debug(fmt.Sprintf("checkpoint-required capability probe failed; init continues: %v", probeErr))
			return
		}
		if supported {
			return
		}
	}
	slog.Warn(fmt.Sprintf("compression.checkpoint_required is enabled but the active memory "+
		"provider (%s) does not implement checkpoint API v%d. Compress will "+
		"fail closed. Recover by %s.", activeMemoryProviderLabel(mm), memory.PreCompressCheckpointAPIVersion,
		checkpointRequiredRemediation))
}

// Capability probe with a provider panic turned into the probe failure
func probeCheckpointSupport(mm *memory.MemoryManager) (supported bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return mm.SupportsPreCompressCheckpoint(memory.PreCompressCheckpointAPIVersion), nil
}
