package cron

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestUsageAuditAndFDHelpers(t *testing.T) {
	s, _ := newTestStore(t)
	rec := pyjson.NewObject()
	rec.Set("job_id", "j1")
	rec.Set("note", "café")
	s.writeUsageAudit(rec)
	s.writeUsageAudit(rec)
	b, err := os.ReadFile(s.usageAuditPath())
	if err != nil || strings.Count(string(b), "\n") != 2 || !strings.Contains(string(b), `{"job_id": "j1", "note": "café"}`) {
		t.Fatal(string(b), err)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(utcnowISOms()) {
		t.Fatal(utcnowISOms())
	}
	dir, file := s.getLockPaths()
	if !strings.HasSuffix(file, "/cron/.tick.lock") || !strings.HasSuffix(dir, "/cron") {
		t.Fatal(dir, file)
	}
	if !isLockContentionErrno(&os.PathError{Op: "flock", Err: syscall.EWOULDBLOCK}) || isLockContentionErrno(errors.New("x")) || isLockContentionErrno(syscall.ENOENT) {
		t.Fatal("lock contention errno")
	}
	if !isFDExhaustion(fmt.Errorf("load: %w", syscall.EMFILE)) || !isFDExhaustion(errors.New("RuntimeError: [Errno 24] Too many open files")) || isFDExhaustion(errors.New("disk full")) || isFDExhaustion(nil) {
		t.Fatal("fd exhaustion")
	}
	raised := 0
	prev := applyNofileSoftLimit
	applyNofileSoftLimit = func(cfg map[string]any) bool { raised++; return cfg == nil }
	t.Cleanup(func() { applyNofileSoftLimit = prev })
	reclaimFDs()
	if raised != 1 {
		t.Fatal("nofile limit not raised")
	}
}
