package harness

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/files"
)

// Files remembered when a tool names none
const DefaultPreImages = 256

// Contents of a file as it stood before a call changed it
type Image struct {
	Path    string
	Content string
	// File did not exist before the call
	Missing bool
}

// Hook that reads the target of every Write and Edit before it runs so a diff exists even when the tool fails
type PreImage struct {
	agent.Hooks
	// Tools whose file_path argument is captured, Write and Edit when empty
	Tools []string
	// Largest file captured, eight MiB when zero
	MaxBytes int64
	// Images kept before the oldest is forgotten, DefaultPreImages when zero
	Max int
	// Resolves file_path the way the files tools do, against their directory and root
	Files  files.Options
	mu     sync.Mutex
	images map[string]Image
	order  []string
}

func (p *PreImage) tools() []string {
	if len(p.Tools) > 0 {
		return p.Tools
	}
	return []string{files.WriteName, files.EditName}
}

func (p *PreImage) maxBytes() int64 {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return 8 << 20
}

func (p *PreImage) limit() int {
	if p.Max > 0 {
		return p.Max
	}
	return DefaultPreImages
}

// Captures the file named by the call before the tool touches it
func (p *PreImage) BeforeTool(ctx context.Context, call protocol.ToolCall) (agent.Advice, error) {
	adv := agent.Advice{Call: call}
	wanted := false
	for _, name := range p.tools() {
		if name == call.Name {
			wanted = true
			break
		}
	}
	if !wanted {
		return adv, nil
	}
	var in struct {
		Path string `json:"file_path"`
	}
	if err := json.Unmarshal(call.Args, &in); err != nil || in.Path == "" {
		return adv, nil
	}
	path, err := p.Files.Resolve(in.Path)
	if err != nil {
		return adv, nil
	}
	img := Image{Path: path}
	st, err := os.Stat(path)
	switch {
	case err != nil:
		img.Missing = true
	case st.Size() <= p.maxBytes() && st.Mode().IsRegular():
		data, err := os.ReadFile(path)
		if err == nil {
			img.Content = string(data)
		}
	}
	p.mu.Lock()
	if p.images == nil {
		p.images = map[string]Image{}
	}
	if _, dup := p.images[call.ID]; !dup {
		p.order = append(p.order, call.ID)
	}
	p.images[call.ID] = img
	for len(p.order) > p.limit() {
		delete(p.images, p.order[0])
		p.order = p.order[1:]
	}
	p.mu.Unlock()
	return adv, nil
}

// Image captured for a call id
func (p *PreImage) Get(callID string) (Image, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	img, ok := p.images[callID]
	return img, ok
}

var _ agent.Hook = (*PreImage)(nil)
