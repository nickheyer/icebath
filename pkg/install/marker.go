package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Release pins of one bootstrap, each nil when unset
type Pin struct {
	Commit *string
	Branch *string
}

// Pinned commit, else the installed binary's own revision, else the commit a prior receipt recorded
func resolveMarkerCommit(installRoot string, pin Pin, markerPath string) *string {
	if pin.Commit != nil && strings.TrimSpace(*pin.Commit) != "" {
		return pin.Commit
	}
	if commit := InstalledRelease(context.Background(), InstallBinaryPath(installRoot)).Commit; commit != "" {
		return &commit
	}
	if commit := readExistingMarkerCommit(markerPath); commit != "" {
		return &commit
	}
	return nil
}

// Full commit a prior receipt records, a UTF-8 BOM tolerated
func readExistingMarkerCommit(markerPath string) string {
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return ""
	}
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))
	var marker map[string]any
	if json.Unmarshal(raw, &marker) != nil {
		return ""
	}
	commit, _ := marker["pinnedCommit"].(string)
	if !IsFullSHA(commit) {
		return ""
	}
	return commit
}

// UTC ISO 8601 with milliseconds, the completedAt format every receipt writer shares
func ISO8601UTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Publishes the bootstrap receipt of an install, the commit resolved from pin, binary and prior receipt
func WriteBootstrapMarker(installRoot string, pin Pin) (json.RawMessage, error) {
	return writeMarker(installRoot, resolveMarkerCommit(installRoot, pin, BootstrapMarkerPath(installRoot)), pin.Branch, time.Now())
}

// Writes the receipt through a synced temp sibling renamed over any prior one
func writeMarker(installRoot string, commit, branch *string, completed time.Time) (json.RawMessage, error) {
	markerPath := BootstrapMarkerPath(installRoot)
	if err := os.MkdirAll(installRoot, 0o777); err != nil {
		return nil, fmt.Errorf("could not create bootstrap marker directory %s: %w", installRoot, err)
	}
	body := fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"pinnedCommit\": %s,\n  \"pinnedBranch\": %s,\n  \"completedAt\": %s\n}\n",
		jsonOrNull(commit), jsonOrNull(branch), strconv.Quote(ISO8601UTC(completed)))
	tmp := filepath.Join(installRoot, BootstrapMarkerName+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("could not create temp bootstrap marker %s: %w", tmp, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("could not write temp bootstrap marker %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("could not flush temp bootstrap marker %s: %w", tmp, err)
	}
	f.Close()
	if _, err := os.Stat(markerPath); err == nil {
		if err := os.Remove(markerPath); err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("could not replace existing bootstrap marker %s: %w", markerPath, err)
		}
	}
	if err := os.Rename(tmp, markerPath); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("could not publish bootstrap marker %s -> %s: %w", tmp, markerPath, err)
	}
	return json.RawMessage(body), nil
}

func jsonOrNull(value *string) string {
	if value == nil {
		return "null"
	}
	data, _ := json.Marshal(*value)
	return string(data)
}

// Update claim the installer holds over the shared marker for one hand off
type UpdateGuard struct {
	lock *updaterecord.UpdateLock
}

// Claims the host wide update marker, returning the refusal text when a live update or unwritable path stops it
func AcquireUpdateGuard(ctx context.Context, markerPath, installRoot string) (*UpdateGuard, string) {
	lock := updaterecord.NewUpdateLock(markerPath, installRoot, false)
	ok, err := lock.Acquire(ctx)
	if err != nil {
		return nil, fmt.Sprintf("Cannot lock this install for the update: %s is not writable (%v). Run the update as the user that owns the install.", markerPath, err)
	}
	if !ok {
		if lock.Holder != nil && lock.Holder.Reason != "" {
			return nil, "Cannot lock this install for the update: " + lock.Holder.Reason + ". Run the update as the user that owns the install."
		}
		return nil, BusyUpdateMessage(lock.Holder)
	}
	return &UpdateGuard{lock: lock}, ""
}

// Releases the claim, a live delegate inheriting it, safe to call more than once
func (g *UpdateGuard) Complete(ctx context.Context) {
	if g != nil && g.lock != nil {
		g.lock.Release(ctx)
	}
}

// Refusal shown when another update holds the marker
func BusyUpdateMessage(holder *updaterecord.UpdateHolder) string {
	const wait = "Wait for it to finish, or close the window or dashboard tab that started it, then try again."
	var h updaterecord.UpdateHolder
	if holder != nil {
		h = *holder
	}
	age := int64(h.AgeSeconds)
	elapsed := fmt.Sprintf("%ds", age%60)
	if age/60 > 0 {
		elapsed = fmt.Sprintf("%dm %ds", age/60, age%60)
	}
	if h.Held {
		return fmt.Sprintf("Another Icebath update is already running (started %s ago; its owner exited but a process it started still holds the checkout). %s", elapsed, wait)
	}
	if h.PID == 0 {
		return "Another Icebath update is starting right now. " + wait
	}
	return fmt.Sprintf("Another Icebath update is already running (PID %d, started %s ago). %s", h.PID, elapsed, wait)
}

// Reports a marker whose owner identity is this very process incarnation
func markerOwnedBySelf(markerPath string) bool {
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}
	verdict, owner, _ := updaterecord.JudgeMarker(raw, nil)
	if verdict != "ours" || owner == nil || *owner != os.Getpid() {
		return false
	}
	pid, _, _ := strings.Cut(strings.TrimPrefix(string(raw), "\ufeff"), "\n")
	return strings.TrimSpace(pid) == strconv.Itoa(os.Getpid())
}

// Heals exit 2 only when the child refused our own claim and no checkout lock is held
func ShouldHealSelfMarkerRefusal(ctx context.Context, exitCode int, markerPath, installRoot string) bool {
	if exitCode != updaterecord.UpdateExitConcurrent || !markerOwnedBySelf(markerPath) {
		return false
	}
	held, err := updaterecord.CheckoutLockHeld(ctx, installRoot)
	return err == nil && !held
}
