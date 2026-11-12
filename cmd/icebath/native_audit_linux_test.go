//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// Site recording each process start and each sqlite connection of a re-executed binary, the native live sitecustomize
const nativeAuditSite = "native-audit"

// File the native audit site appends its JSON records to
const nativeAuditEnv = "ICEBATH_TEST_AUDIT"

func init() { siteHooks[nativeAuditSite] = installNativeAudit }

var nativeAuditMu sync.Mutex

// Appends one record of this process to the audit file
func nativeAuditRecord(kind string, fields map[string]any) {
	fields["kind"], fields["pid"] = kind, os.Getpid()
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	nativeAuditMu.Lock()
	defer nativeAuditMu.Unlock()
	sink, err := os.OpenFile(os.Getenv(nativeAuditEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer sink.Close()
	if _, err := sink.Write(append(raw, '\n')); err != nil {
		panic(err)
	}
}

// Parent pid and command name of a live process from its proc entries
func procParent(pid int) (int, string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", err
	}
	text := string(raw)
	rparen := strings.LastIndexByte(text, ')')
	lparen := strings.IndexByte(text, '(')
	if lparen < 0 || rparen < lparen {
		return 0, "", fmt.Errorf("unparsable stat of %d", pid)
	}
	fields := strings.Fields(text[rparen+1:])
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("unparsable stat of %d", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, text[lparen+1 : rparen], err
}

// Up to four ancestors as pid and name pairs, nearest first, the repr of the failure when one cannot be read
func procAncestors() []any {
	var out []any
	pid := os.Getppid()
	for len(out) < 4 && pid > 0 {
		ppid, name, err := procParent(pid)
		if err != nil {
			return []any{fmt.Sprintf("%v", err)}
		}
		out = append(out, []any{pid, name})
		pid = ppid
	}
	return out
}

// Records this start and every later sqlite connection with its target
func installNativeAudit() {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	nativeAuditRecord("start", map[string]any{"ppid": os.Getppid(), "argv": os.Args, "executable": executable, "ancestors": procAncestors()})
	sqlite.RegisterConnectionHook(func(_ sqlite.ExecQuerierContext, dsn string) error {
		nativeAuditRecord("sqlite", map[string]any{"target": dsn})
		return nil
	})
}

// One record of the audit file
type nativeAuditEntry struct {
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
	PPID      int    `json:"ppid"`
	Argv      []string
	Ancestors []any
	Target    string
}

// Records of the audit file in write order, none when it was never written
func nativeAuditRecords(path string) ([]nativeAuditEntry, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []nativeAuditEntry
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry nativeAuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}
