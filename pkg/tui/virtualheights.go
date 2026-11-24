package tui

import (
	"os"
	"strconv"
	"strings"
)

const (
	termuxPrefix      = "/data/data/com.termux/files/usr"
	maxEstimateLines  = 800
	blankScanUnits    = 16_000
	maxBlankBlockRows = 6
)

// Section of a slash command panel row
type panelSection struct {
	Items []string
	Rows  [][2]string
	Text  *string
	Title *string
}

// Panel a slash command prints into the transcript
type panelData struct {
	Sections []panelSection
	Title    string
}

// Whether the process runs inside Termux, by its version variable or install prefix
func isTermuxEnv(env func(string) string) bool {
	return env("TERMUX_VERSION") != "" || strings.Contains(env("PREFIX"), termuxPrefix)
}

// Whether Termux layout defaults apply, inside Termux while the setting leaves them on
func isTermuxTUIMode(env func(string) string, layout bool) bool {
	return layout && isTermuxEnv(env)
}

// Whether Termux layout defaults apply to this window
func termuxTUIMode() bool { return isTermuxTUIMode(os.Getenv, prefs.TermuxLayout) }

// Base 36 djb2 xor hash over UTF-16 units with JavaScript 32 bit wrapping
func hashText(text string) string {
	h := int64(5381)
	for _, c := range units(text) {
		shifted := int64(int32(uint32(h) << 5))
		h = int64(int32(uint32(shifted+h))) ^ int64(c)
		h = int64(int32(uint32(h)))
	}
	return strconv.FormatUint(uint64(uint32(h)), 36)
}

// Cache key of a message's rendered height, stable across equal message values
func messageHeightKey(m msg) string {
	var todoSig []string
	for _, t := range m.Todos {
		todoSig = append(todoSig, t.Status+":"+t.Content)
	}
	var panelSig []string
	if m.PanelData != nil {
		for _, s := range m.PanelData.Sections {
			title, textLen := "", 0
			if s.Title != nil {
				title = *s.Title
			}
			if s.Text != nil {
				textLen = unitLen(*s.Text)
			}
			panelSig = append(panelSig, title+":"+strconv.Itoa(textLen)+":"+strconv.Itoa(len(s.Items))+":"+strconv.Itoa(len(s.Rows)))
		}
	}
	introSig := ""
	if m.Kind == "intro" {
		introSig = val(infoOf(m.Info).Version)
	}
	body := strings.Join([]string{m.Text, m.Thinking, toolRowsSignature(m.Tools), strings.Join(todoSig, "\u0001"), strings.Join(panelSig, "\u0001"), introSig}, "\x00")
	return m.Role + ":" + m.Kind + ":" + hashText(body)
}
