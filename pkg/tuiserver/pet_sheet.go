package tuiserver

import (
	"fmt"
	"os"
)

// Stable revision id of one spritesheet file from its mtime and size, zero pair when unreadable
func petSheetRevision(spritesheet string) string {
	info, err := os.Stat(spritesheet)
	if err != nil {
		return "0:0"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}
