package tuiserver

import (
	"fmt"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/pet"
)

// Stable revision id of one spritesheet file from its mtime and size, zero pair when unreadable
func petSheetRevision(spritesheet string) string {
	info, err := os.Stat(spritesheet)
	if err != nil {
		return "0:0"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// Copy of a cached pet payload whose frame maps and row list are fresh so callers never mutate the cache
func clonePetPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	for _, key := range []string{"framesByState", "framesByRow"} {
		if m, isMap := payload[key].(map[string]int); isMap {
			copied := make(map[string]int, len(m))
			for k, v := range m {
				copied[k] = v
			}
			out[key] = copied
		}
	}
	if rows, isList := payload["stateRows"].([]string); isList {
		out["stateRows"] = slices.Clone(rows)
	}
	return out
}

// Real frame count of every concrete row of a spritesheet, empty when it does not decode
func petRowFrameCounts(spritesheet string) map[string]int {
	counts, err := pet.RowFrameCounts(spritesheet)
	if err != nil {
		return map[string]int{}
	}
	return counts
}

// Row taxonomy of a concrete sheet, legacy eight rows or the current nine, the default rows when unreadable
func petStateRows(spritesheet string) []string {
	rows, err := pet.SheetRowCount(spritesheet)
	if err != nil {
		return slices.Clone(pet.StateRows)
	}
	return slices.Clone(pet.StateRowsForGrid(rows))
}
