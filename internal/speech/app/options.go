package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
)

// Sections whose settings the area of Voice lists among its own, each the page of a part of Voice
var parts = []string{speechconfig.STT, speechconfig.TTS, speechconfig.Wake}

// Settings a page lists as options: those of the section a person changes there alone, the keys left out that the page shows elsewhere;
// the advanced ones in place, last, while they are too few for the row of their own that OwnAdvanced gives, else behind it
func (p *page) section(key string, leave ...string) (shown, behind []*configv1.Setting) {
	everyday, advanced := p.options.Own(key, leave...)
	if _, _, ok := p.options.OwnAdvanced(key, leave...); ok {
		return everyday, advanced
	}
	return append(everyday, advanced...), nil
}

// Opens the advanced settings of a page of Voice, as Settings opens those of a section, the keys left out as the page leaves them
func (p *page) advanced(key string, leave ...string) tea.Cmd {
	if _, open, ok := p.options.OwnAdvanced(key, leave...); ok {
		return open()
	}
	return nil
}

// Cells of the row that opens the advanced settings of a page of Voice
func (p *page) advancedRow(key string, leave ...string) []string {
	cells, _, _ := p.options.OwnAdvanced(key, leave...)
	return cells
}

// Settings of the keys as last read, in the order given, those not read left out
func (p *page) settingsOf(keys ...string) []*configv1.Setting {
	var out []*configv1.Setting
	for _, key := range keys {
		if setting := p.options.Setting(key); setting != nil {
			out = append(out, setting)
		}
	}
	return out
}

// Value as one line of YAML
func encode(value any) (string, error) {
	text, err := yaml.Marshal(value)
	return strings.TrimSpace(string(text)), err
}

// Value of text written in YAML
func decodeYAML(text string) (any, error) {
	var value any
	err := yaml.Unmarshal([]byte(text), &value)
	return value, err
}

// Text items of a list read from YAML
func asList(value any) []string {
	items, _ := value.([]any)
	var out []string
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}
