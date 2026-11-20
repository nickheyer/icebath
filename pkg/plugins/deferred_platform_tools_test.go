package plugins

import (
	"strings"
)

// Whether a WARN log line names every given text
func warnedWith(logs string, texts ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "level=WARN") {
			continue
		}
		hit := true
		for _, text := range texts {
			hit = hit && strings.Contains(line, text)
		}
		if hit {
			return true
		}
	}
	return false
}
