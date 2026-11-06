package cron

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The owner may have accepted this fire, so it is never booked as failed or refired
type CronExecutionUnknown struct{ Msg string }

func (e *CronExecutionUnknown) Error() string { return e.Msg }

// Admission journal of one fire keyed by the sha256 of the JSON pair of job and request ids
func (s *Store) journalPath(jobID, requestID string) string {
	key, err := pyjson.Dumps([]any{jobID, requestID})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.home, "cron", "admissions", hex.EncodeToString(sum[:])+".json")
}

// The exact pause a recovery uncertainty caused, true when the pause returned nothing
func pauseIdentity(paused *Job) any {
	if paused == nil {
		return true
	}
	o := pyjson.NewObject()
	o.Set("paused_at", strOrNull(paused.PausedAt))
	o.Set("reason", strOrNull(paused.PausedReason))
	return o
}

// Completion ids whose admission journal still exists, the only ones journal recovery can replay
func replayableCompletions(s *Store, jobID string, completions []string) []string {
	out := []string{}
	for _, id := range completions {
		if _, err := os.Stat(s.journalPath(jobID, id)); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// Writes a journal record as two space indented JSON in a private atomic file
func writeAdmissionJournal(path string, record any) error {
	text, err := pyjson.Dumps(record, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(record, pyjson.Indent(2), pyjson.EnsureASCII(true)); err != nil {
			return err
		}
	}
	return config.AtomicWrite(path, []byte(text), 0o600)
}

// Pauses the job for an unverified admission and records that exact pause on the fire journal
func (s *Store) pauseForUnknown(jobID, executionID, reason string) error {
	paused, err := s.pauseJob(jobID, &reason)
	if err != nil {
		return err
	}
	journal := s.journalPath(jobID, executionID)
	if paused == nil {
		return nil
	}
	raw, err := os.ReadFile(journal)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	record, ok := decoded.(*pyjson.Object)
	if !ok {
		return &pyTypeError{msg: "'" + pyTypeName(decoded) + "' object does not support item assignment"}
	}
	record.Set("pause_recorded", pauseIdentity(paused))
	return writeAdmissionJournal(journal, record)
}

// Python TypeError raised by an operation on the wrong value type
type pyTypeError struct{ msg string }

func (e *pyTypeError) Error() string         { return e.msg }
func (e *pyTypeError) ExceptionName() string { return "TypeError" }

// Resumes the job only while it is still held by exactly the recorded pause
func (s *Store) liftPause(jobID string, pause any) error {
	identity, ok := pause.(*pyjson.Object)
	if !ok {
		return nil
	}
	job, err := s.getJob(jobID)
	if err != nil || job == nil || job.State != "paused" {
		return err
	}
	at, _ := identity.Get("paused_at")
	reason, _ := identity.Get("reason")
	if !pyrepr.Equal(strOrNull(job.PausedAt), at) || !pyrepr.Equal(strOrNull(job.PausedReason), reason) {
		return nil
	}
	if _, err := s.resumeJob(jobID); err != nil && isOSError(err) {
		return err
	}
	return nil
}
