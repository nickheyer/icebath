package storage

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

const defaultProjectDirFile = "project-dir.json"

func (s *Service) defaultProjectDirPath() string {
	return filepath.Join(s.userData, defaultProjectDirFile)
}

// Stored default project directory when it still exists, false for none or anything unreadable
func (s *Service) readDefaultProjectDir() (string, bool) {
	raw, err := os.ReadFile(s.defaultProjectDirPath())
	if err != nil {
		return "", false
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	doc, err := jsParse(text)
	if err != nil {
		return "", false
	}
	obj, ok := doc.(*Object)
	if !ok {
		return "", false
	}
	dir, ok := obj.vals["dir"].(string)
	if !ok || jsTrim(dir) == "" {
		return "", false
	}
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return "", false
	}
	return resolved, true
}

// Persists the default project directory, an empty dir clearing it, logging a failed write
func (s *Service) writeDefaultProjectDir(dir string) {
	doc := NewObject()
	if dir != "" {
		resolved, _ := filepath.Abs(dir)
		doc.Set("dir", resolved)
	}
	target := s.defaultProjectDirPath()
	err := os.MkdirAll(filepath.Dir(target), 0o777)
	if err == nil {
		err = os.WriteFile(target, []byte(jsStringify(doc, "  ")), 0o666)
	}
	if err != nil {
		s.log.Remember("[settings] write default project dir failed: " + hardening.NodeErr(err).Error())
	}
}

// Answers settings.getDefaultProjectDir with the saved folder or null, the home folder label and the session cwd
func (s *Service) SettingsGetDefaultProjectDir() *Object {
	out := NewObject()
	if dir, ok := s.readDefaultProjectDir(); ok {
		out.Set("dir", dir)
	} else {
		out.Set("dir", nil)
	}
	home, _ := os.UserHomeDir()
	out.Set("defaultLabel", home)
	out.Set("resolvedCwd", s.workspace.ResolveHermesCwd())
	return out
}

// Answers settings.setDefaultProjectDir, creating the directory and storing it, a blank value clearing it
func (s *Service) SettingsSetDefaultProjectDir(dir json.RawMessage) (*Object, error) {
	v, _ := jsParse(string(dir))
	text, _ := v.(string)
	next := jsTrim(text)
	if next != "" {
		if err := os.MkdirAll(next, 0o777); err != nil {
			return nil, &plainError{"Could not create directory: " + hardening.NodeErr(err).Error()}
		}
	}
	s.writeDefaultProjectDir(next)
	out := NewObject()
	if next == "" {
		out.Set("dir", nil)
	} else {
		out.Set("dir", next)
	}
	return out, nil
}

// Answers settings.pickDefaultProjectDir with the folder the user chose in the native picker
func (s *Service) SettingsPickDefaultProjectDir() (*Object, error) {
	start, ok := s.readDefaultProjectDir()
	if !ok {
		start, _ = os.UserHomeDir()
	}
	dir, chosen, err := s.host.PickDirectory("Choose default project directory", start)
	if err != nil {
		return nil, err
	}
	out := NewObject()
	if !chosen || dir == "" {
		out.Set("canceled", true)
		out.Set("dir", nil)
		return out, nil
	}
	out.Set("canceled", false)
	out.Set("dir", dir)
	return out, nil
}

// Error carrying exactly the message the handler throws
type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }
