package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Closes every holographic memory store connection under dir in each running plugin host, returning the summed count
func memoryStoreReleaseAllUnder(dir string) (int, error) {
	ctx := context.Background()
	total := 0
	for _, host := range plugins.RunningPluginHosts() {
		mod, err := memory.ImportProviderModuleIn(ctx, host, "holographic", "store")
		if err != nil {
			return total, err
		}
		n, err := mod.Call(ctx, "MemoryStore.release_all_under", []any{dir}, nil)
		if err != nil {
			return total, err
		}
		c, _ := n.(int64)
		total += int(c)
	}
	return total, nil
}
