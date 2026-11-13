package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Journal switch outcomes, a path with no database file and every refusal
var (
	ErrNoDatabase    = errors.New("no database to convert")
	ErrSwitchRefused = errors.New("journal mode switch refused")
)

// Operator text of a journal switch outcome and the class it belongs to
type switchError struct {
	msg  string
	kind error
}

// Operator text
func (e *switchError) Error() string { return e.msg }

// Outcome class
func (e *switchError) Unwrap() error { return e.kind }

// Switch refusal with formatted operator text
func refuseSwitch(format string, args ...any) error {
	return &switchError{msg: fmt.Sprintf(format, args...), kind: ErrSwitchRefused}
}

// Header write and read version bytes of each journal mode
var headerVersions = map[[2]byte]string{{1, 1}: string(JournalDelete), {2, 2}: string(JournalWAL)}

// Completed or already settled offline journal mode switch
type JournalSwitch struct {
	Path string
	// Mode the header reported before the switch
	From string
	// Mode the header reports now
	To string
	// Nothing changed because the file already ran in the target mode
	Already bool
	// Mode config.yaml asks for, which the next open applies again when it differs from To
	Configured JournalMode
}

// Journal mode the header of path reports, not-a-database or unknown versions for anything else
func (e *Engine) headerMode(path string) (string, error) {
	head, err := e.readHeader(path, 20, false)
	if err != nil {
		return "", err
	}
	if len(head) < 20 || string(head[:16]) != sqliteMagic {
		return "not-a-database", nil
	}
	if mode, ok := headerVersions[[2]byte{head[18], head[19]}]; ok {
		return mode, nil
	}
	return fmt.Sprintf("unknown(%d/%d)", head[18], head[19]), nil
}

// Switches the journal mode of a store offline, refusing while any other process holds it, then verifying the header
func (e *Engine) SetJournalMode(ctx context.Context, path string, target JournalMode, force bool, d Database) (JournalSwitch, error) {
	db, err := e.storePath(ctx, path)
	if err != nil {
		return JournalSwitch{}, err
	}
	out := JournalSwitch{Path: db, Configured: d.journalMode()}
	if !exists(db) {
		return out, &switchError{msg: fmt.Sprintf("No database at %s (nothing to convert).", db), kind: ErrNoDatabase}
	}
	current, err := e.headerMode(db)
	if err != nil {
		return out, refuseSwitch("Cannot read %s: %v", db, err)
	}
	out.From = current
	if current == string(target) {
		out.To, out.Already = current, true
		return out, nil
	}
	if current == "not-a-database" || strings.HasPrefix(current, "unknown(") {
		return out, refuseSwitch("Refusing to change the journal mode of %s: its file header reads %s, so it is not an "+
			"icebath SQLite store this command can convert (pass --db PATH to point at one).", db, current)
	}
	if target == JournalWAL && e.OnCrossVMFilesystem(db) {
		return out, refuseSwitch("Refusing to change the journal mode of %s: WAL shared memory silently corrupts on "+
			"cross-VM filesystems (virtiofs/9p — Docker Desktop, OrbStack, Lima bind mounts). Keep this store on "+
			"journal_mode=delete or move it to a native filesystem.", db)
	}
	if target == JournalWAL && WALResetVulnerable() {
		return out, refuseSwitch("Refusing to enable WAL: the linked SQLite %s has the WAL-reset bug "+
			"(https://sqlite.org/wal.html#walresetbug). Upgrade to 3.51.3+ first.", SQLiteVersion())
	}
	holders := ForeignHolders(db)
	if force {
		kept := holders[:0]
		for _, h := range holders {
			if h.PID > 0 {
				kept = append(kept, h)
			}
		}
		holders = kept
	}
	if len(holders) > 0 {
		lines := []string{fmt.Sprintf("Refusing to change the journal mode of %s: other processes hold it open "+
			"(a live switch would destroy their uncheckpointed commits). Stop them and re-run:", db)}
		for _, h := range holders {
			pid, who := -1, "scan"
			if h.PID > 0 {
				pid, who = h.PID, DescribeHolderPID(h.PID)
			}
			lines = append(lines, fmt.Sprintf("    pid %d — %s: %s", pid, who, h.Target))
		}
		return out, refuseSwitch("%s", strings.Join(lines, "\n"))
	}
	reported, err := e.switchMode(ctx, db, strings.ToUpper(string(target)))
	if err != nil {
		return out, err
	}
	after, err := e.headerMode(db)
	if err != nil {
		return out, refuseSwitch("Cannot re-read %s after the switch: %v", db, err)
	}
	if after != string(target) {
		if reported == "" {
			reported = "?"
		}
		return out, refuseSwitch("SQLite reported journal_mode=%s but the file header reads %s; not converted.", reported, after)
	}
	out.To = after
	return out, nil
}

// Opens a zero timeout connection and flips the mode, any opener that appeared since the scan making the pragma refuse
func (e *Engine) switchMode(ctx context.Context, db, mode string) (string, error) {
	c, err := e.connect(ctx, fileURI(db), db)
	if err != nil {
		return "", refuseSwitch("Cannot open %s: %s", db, errText(err))
	}
	defer closeLogged(c, "journal switch", db)
	if _, err := c.q().ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return "", refuseSwitch("Cannot open %s: %s", db, errText(err))
	}
	reported, err := setModeNoWait(ctx, c.q(), mode)
	if err != nil {
		return "", refuseSwitch("journal_mode=%s refused: %s (another opener appeared; nothing was changed)", mode, errText(err))
	}
	return reported, nil
}

// First n bytes of path read raw, an error naming why when a live connection or the filesystem refuses
func (e *Engine) readHeader(path string, n int, force bool) ([]byte, error) {
	key := resolvePath(path)
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	if _, live := e.live.mainKey(key); live && !force {
		return nil, fmt.Errorf("refusing a raw read of %s while this process holds a connection to it: %w", path, ErrLiveConnection)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := readFull(f, buf)
	if err != nil {
		return nil, err
	}
	return buf[:got], nil
}
