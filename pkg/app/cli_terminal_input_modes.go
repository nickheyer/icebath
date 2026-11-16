package app

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
)

var (
	dsrCPREscRe         = regexp.MustCompile(`\x1b\[\d+;\d+R`)
	dsrCPRVisibleRe     = regexp.MustCompile(`\^\[\[\d+;\d+R`)
	sgrMouseEscRe       = regexp.MustCompile(`\x1b\[<\d+;\d+;\d+[Mm]`)
	sgrMouseVisibleRe   = regexp.MustCompile(`\^\[\[<\d+;\d+;\d+[Mm]`)
	sgrMouseBareRe      = regexp.MustCompile(`<\d+;\d+;\d+[Mm]`)
	backslashContinueRe = regexp.MustCompile(`\\[ \t]*$`)
)

const (
	kittyKeyboardPushSeq  = "\x1b[>1u"
	modifyOtherKeysSeq    = "\x1b[>4;2m"
	extendedEnterKeysSeq  = kittyKeyboardPushSeq + modifyOtherKeysSeq
	bracketedPasteTimeout = 2.0
)

// Example media path for the Termux storage layout present on this device
func termuxExampleImagePath(filename string) string {
	if filename == "" {
		filename = "cat.png"
	}
	home, _ := os.UserHomeDir()
	for _, root := range []string{filepath.Join(home, "storage", "shared"), "/sdcard", "/storage/emulated/0", "/storage/self/primary"} {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return filepath.ToSlash(root) + "/Pictures/" + filename
		}
	}
	return "~/storage/shared/Pictures/" + filename
}

// ptScreen is the height of one painted chrome frame
type ptScreen struct{ height int }

// screenDiff paints a frame against the previous one
type screenDiff func(previous *ptScreen, lastStyle string, previousWidth int) (any, error)

var (
	bracketedPastePatchMu sync.Mutex
	bracketedPastePatched bool
)

// Seams the Ctrl Enter newline probe reads the platform and kernel release through
var (
	ctrlEnterGOOS     = runtime.GOOS
	ctrlEnterReadFile = os.ReadFile
)

// ptOutputConfig is the output setup of the prompt application
type ptOutputConfig struct {
	out       *os.File
	enableCPR bool
	size      func() (rows, columns int)
}
