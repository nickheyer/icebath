package app

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Python truth of a config value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Saves a clipboard image under the images directory and attaches it, true when one was attached
func (c *CLI) tryAttachClipboardImage() bool {
	c.imageCounter++
	path := filepath.Join(home.Dir(c.ctx), "images", fmt.Sprintf("clip_%s_%d.png", time.Now().Format("20060102_150405"), c.imageCounter))
	if clipSaveImage(path) {
		c.attachedImages = append(c.attachedImages, path)
		return true
	}
	c.imageCounter--
	return false
}

// Copies text to the terminal clipboard through OSC 52, wrapped for tmux and screen passthrough
func (c *CLI) writeOSC52Clipboard(text string) {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	switch {
	case os.Getenv("TMUX") != "":
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case os.Getenv("STY") != "":
		seq = "\x1bP" + seq + "\x1b\\"
	}
	if _, err := io.WriteString(console().Out, seq); err != nil {
		pkgLog().Debug("OSC 52 clipboard write failed", "error", err)
	}
}
