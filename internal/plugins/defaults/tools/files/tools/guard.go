package tools

import (
	"path/filepath"

	"github.com/nickheyer/icebath/internal/config"
)

// Refusal of a write to a path of this machine by another plugin's tool, such as text_to_speech's output file: the protected paths, and the
// SSH client config, which the file tools only write once approved
func (r *registrant) Unwritable(cfg *config.Config, path string) string {
	o := &ops{r: r, cfg: cfg, host: true}
	if refusal := o.denied(path, "Write", false); refusal != "" {
		return refusal
	}
	p := o.canonical(path)
	for _, home := range o.homes(path) {
		if p == realpath(filepath.Join(home, ".ssh", "config")) {
			return "Write denied: '" + path + "' is the SSH client config, which is written only once the user approves it."
		}
	}
	return ""
}

// Refusal of a read of a path of this machine by another plugin's tool, such as a recording a transcription uploads
func (r *registrant) Unreadable(cfg *config.Config, path string) string {
	o := &ops{r: r, cfg: cfg, host: true}
	return o.readBlock(path, realpath(o.hostTilde(path)))
}
