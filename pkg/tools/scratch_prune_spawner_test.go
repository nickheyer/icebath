package tools

import (
	"context"
	"os"
	"strconv"
	"time"
)

// Env naming the pid a re-executed test binary registers as its child before sleeping
const scratchSpawnerEnv = "ICEBATH_TOOLS_SCRATCH_SPAWNER"

// Registers the named pid as an mcp-helper child of this process and sleeps, the spawner the prune case kills
func scratchSpawnerEntry() {
	raw := os.Getenv(scratchSpawnerEnv)
	if raw == "" {
		return
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || !RegisterChild(context.Background(), pid, "mcp-helper", "") {
		os.Exit(1)
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}
