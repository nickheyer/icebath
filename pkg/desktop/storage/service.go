package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/desktop/desktoplog"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// What the shell provides the storage group
type Host interface {
	// Sends event with payload to every app window
	Broadcast(event string, payload any)
	// Opens the file manager with path selected as shell.showItemInFolder does
	ShowItemInFolder(path string)
	// Native folder picker that may create folders, false when the user cancels
	PickDirectory(title, defaultPath string) (string, bool, error)
}

// Directories and the desktop log the storage group works in
type Options struct {
	HermesHome string
	UserData   string
	Log        *desktoplog.Log
	Host       Host
}

// Storage group of the preload API over the machine root home and the app user data
type Service struct {
	hermesHome string
	userData   string
	log        *desktoplog.Log
	host       Host
	prefs      *UIPreferences
	dataURLMax *dataURLReadMax
}

// Opens the storage group and starts watching the machine preferences store
func NewService(o Options) *Service {
	s := &Service{hermesHome: o.HermesHome, userData: o.UserData, log: o.Log, host: o.Host, dataURLMax: loadDataURLReadMax(o.UserData)}
	s.prefs = NewUIPreferences(o.HermesHome, func(snap UIPreferencesSnapshot) { o.Host.Broadcast(uiPrefsChangedEvent, snap) })
	return s
}

// Stops the background watchers
func (s *Service) Close() { s.prefs.Close() }

// Pre paint preferences snapshot, nil while the store cannot be read
func (s *Service) UiPreferencesSnapshot() *UIPreferencesSnapshot { return s.prefs.Snapshot() }

// Answers uiPreferences.read
func (s *Service) UiPreferencesRead() (UIPreferencesSnapshot, error) { return s.prefs.Read() }

// Answers uiPreferences.write and pushes the result to every window
func (s *Service) UiPreferencesWrite(payload json.RawMessage) (UIPreferencesSnapshot, error) {
	return s.prefs.Write(payload)
}

// Pre paint skin payload of a window from its routed profile or the primary profile
func (s *Service) LocalSkin(routedProfile *string, primaryProfile string) *Object {
	return ReadLocalSkinPayload(s.hermesHome, routedProfile, primaryProfile)
}

func (s *Service) journal(origin string) *PreparedJournal {
	return NewPreparedJournal(s.userData, origin, func(line string) { fmt.Fprintln(os.Stderr, line) })
}

// Answers preparedSubmissions.read for the calling frame's origin
func (s *Service) PreparedSubmissionsRead(origin string) (string, error) {
	return s.journal(origin).ReadText()
}

// Answers preparedSubmissions.update for the calling frame's origin
func (s *Service) PreparedSubmissionsUpdate(origin string, key, entry json.RawMessage) error {
	return s.journal(origin).UpdateText(key, entry)
}

// Answers preparedSubmissions.compareAndSet for the calling frame's origin
func (s *Service) PreparedSubmissionsCompareAndSet(origin string, key, expected, entry json.RawMessage) (*Object, error) {
	return s.journal(origin).CompareAndSetText(key, expected, entry)
}

// Creates desktop.log when missing and shows it in the file manager
func (s *Service) RevealLogs() *Object {
	path := s.log.Path()
	out := NewObject()
	err := os.MkdirAll(filepath.Dir(path), 0o777)
	if err == nil {
		if st, serr := os.Stat(path); serr != nil || !st.Mode().IsRegular() {
			var f *os.File
			if f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
				err = f.Close()
			}
		}
	}
	if err != nil {
		out.Set("ok", false)
		out.Set("path", path)
		out.Set("error", hardening.NodeErr(err).Error())
		return out
	}
	s.host.ShowItemInFolder(path)
	out.Set("ok", true)
	out.Set("path", path)
	return out
}

// Answers getRecentLogs with the last 200 desktop log lines
func (s *Service) GetRecentLogs() *Object {
	out := NewObject()
	out.Set("path", s.log.Path())
	lines := []any{}
	for _, l := range s.log.Recent(200) {
		lines = append(lines, l)
	}
	out.Set("lines", lines)
	return out
}

// Persists a renderer error boundary report with its component stack and flushes at once
func (s *Service) ReportRendererError(report json.RawMessage) {
	var label, boundary, message, stack any
	if v, err := jsParse(string(report)); err == nil {
		if o, ok := v.(*Object); ok {
			label, boundary, message, stack = o.vals["label"], o.vals["boundary"], o.vals["message"], o.vals["componentStack"]
		}
	}
	s.log.Remember(FormatRendererBoundaryReport(label, boundary, message, stack))
	s.log.FlushSync()
}

// Records a renderer supplied line clamped to 6000 characters and flushes at once
func (s *Service) LogLine(line json.RawMessage) {
	v, _ := jsParse(string(line))
	text, _ := v.(string)
	text = desktoplog.JSSlice(text, 6000)
	if text == "" {
		return
	}
	s.log.Remember(text)
	s.log.FlushSync()
}

// Desktop log text of a renderer error boundary report, every field clamped
func FormatRendererBoundaryReport(label, boundary, message, componentStack any) string {
	clamp := func(v any, max int) string { return desktoplog.JSSlice(jsString(v), max) }
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	head := "[renderer crash:" + or(clamp(label, 64), "unknown") + "] [error-boundary:" + or(clamp(boundary, 64), "unknown") + "] " +
		or(clamp(message, 2000), "(no message)")
	if stack := jsTrim(clamp(componentStack, 4000)); stack != "" {
		return head + "\n" + stack
	}
	return head
}

// String conversion of a JSON value as String(value ?? ”) does
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case *Object:
		return "[object Object]"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return joinComma(parts)
	}
	return fmt.Sprint(v)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
