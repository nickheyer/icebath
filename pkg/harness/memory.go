package harness

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/prompt"
)

// Lines and bytes of the memory file shown in the prompt
const (
	memoryLines = 200
	memoryBytes = 25 * 1024
)

// Directory an agent of a kind keeps its memory in, by the scope of the kind, empty for none
func (a *App) memoryDir(kind Kind) (string, error) {
	switch kind.Memory {
	case "user":
		dir, err := configDirOf()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "agent-memory", kind.Name), nil
	case "project":
		return filepath.Join(a.Root, ProjectDir, "agent-memory", kind.Name), nil
	case "local":
		return filepath.Join(a.Root, ProjectDir, "agent-memory-local", kind.Name), nil
	}
	return "", nil
}

// Prompt block telling an agent about its memory directory, with the head of its memory file
func (a *App) memoryBlock(kind Kind) (string, error) {
	dir, err := a.memoryDir(kind)
	if err != nil || dir == "" {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(dir, "MEMORY.md")
	rules := []string{
		"Your memory directory is " + dir + ". It persists across sessions, nothing else you learn does.",
		"MEMORY.md there is read into this prompt at the start of every session, its first " + itoa(memoryLines) + " lines. Keep it a short index, one line per topic, with details in other files beside it.",
		"Write what you learn that later sessions need, patterns of this project, decisions made, mistakes to avoid. Edit the files with Read, Write and Edit as you go, not only at the end.",
	}
	d := prompt.New().Heading(1, "Memory").Bullets(rules...)
	data, err := os.ReadFile(file)
	if err == nil {
		text := string(data)
		if len(text) > memoryBytes {
			text = text[:memoryBytes]
		}
		lines := strings.Split(text, "\n")
		if len(lines) > memoryLines {
			lines = lines[:memoryLines]
		}
		d.Heading(2, "MEMORY.md").Raw(strings.Join(lines, "\n"))
	}
	return d.String(), nil
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(n))
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
