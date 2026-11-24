package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
)

// Home and catalog lookup of one /snapshot run, T taking the snapshot keys of cli.commands
type SnapshotCommand struct {
	Home string
	T    func(key string, kwargs map[string]any) string
}

// Outcome of a restore that handed the profile off the gateway
type SnapshotRestore struct {
	Restored bool
	Detached bool
	Profile  string
	Ack      *jsonutil.Object
}

// Stop advice per supervisor that would relaunch a gateway right after a drain
var ownerStopHints = map[string]string{
	"systemd":  "stop its systemd unit with systemctl before retrying",
	"launchd":  "stop its LaunchAgent with launchctl before retrying",
	"external": "stop the gateway through its external supervisor before retrying",
}

// Plain map of a decoded snapshot manifest row
func snapshotRow(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if m, ok := jsonutil.Plain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Value of a str.isdecimal token, false for an empty token or any other rune
func decimalCount(token string) (int, bool) {
	if token == "" {
		return 0, false
	}
	n := 0
	for _, r := range token {
		if !unicode.Is(unicode.Nd, r) {
			return 0, false
		}
		start := r
		for unicode.Is(unicode.Nd, start-1) {
			start--
		}
		n = n*10 + int(r-start)%10
	}
	return n, true
}

// Value of a token Python int accepts, underscores allowed between digits only
func pyInt(token string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(token, "_", "")))
	if err != nil || strings.HasPrefix(token, "_") || strings.HasSuffix(token, "_") || strings.Contains(token, "__") {
		return 0, false
	}
	return n, true
}

// Size of a snapshot as bytes, kilobytes or megabytes
func snapshotSize(size int64) (string, error) {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size), nil
	case size < 1024*1024:
		text, err := templateutil.Format("{v:.0f}", map[string]any{"v": float64(size) / 1024})
		return text + " KB", err
	}
	text, err := templateutil.Format("{v:.1f}", map[string]any{"v": float64(size) / 1024 / 1024})
	return text + " MB", err
}

// Lines of the newest twenty quick snapshots as a table, or the hint to create the first one
func (s SnapshotCommand) List() ([]string, error) {
	snaps, err := backup.ListQuickSnapshots(20, s.Home)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return []string{s.T("snapshot.none_yet", nil), s.T("snapshot.create_hint", nil)}, nil
	}
	lines := []string{
		s.T("snapshot.list_header", map[string]any{"dir": home.Display(s.Home)}),
		"",
		fmt.Sprintf("%3s  %-35s %5s %10s %s", s.T("snapshot.col_num", nil), s.T("snapshot.col_id", nil),
			s.T("snapshot.col_files", nil), s.T("snapshot.col_size", nil), s.T("snapshot.col_label", nil)),
		strings.Repeat("─", 3) + "  " + strings.Repeat("─", 35) + " " + strings.Repeat("─", 5) + " " + strings.Repeat("─", 10) + " " + strings.Repeat("─", 20),
	}
	for i, raw := range snaps {
		row := snapshotRow(raw)
		size := int64(0)
		if total, present := row["total_size"]; present {
			if n, ok := anyutil.Int(total); ok {
				size = n
			}
		}
		sizeText, err := snapshotSize(size)
		if err != nil {
			return nil, err
		}
		files := "0"
		if count, present := row["file_count"]; present {
			files = anyutil.String(count)
		}
		label := ""
		if text, isStr := row["label"].(string); isStr {
			label = text
		}
		lines = append(lines, fmt.Sprintf("%3d  %-35s %5s %10s %s", i+1, anyutil.String(row["id"]), files, sizeText, label))
	}
	return lines, nil
}

// Line of a new quick snapshot whose label is every word after the subcommand
func (s SnapshotCommand) Create(parts []string) ([]string, error) {
	var label *string
	if len(parts) > 2 {
		joined := strings.Join(parts[2:], " ")
		label = &joined
	}
	id, err := backup.CreateQuickSnapshot(label, s.Home, nil, nil)
	if err != nil {
		return nil, err
	}
	if id != "" {
		return []string{s.T("snapshot.created", map[string]any{"snapshot_id": id})}, nil
	}
	return []string{s.T("snapshot.nothing_to_snapshot", nil)}, nil
}

// Line of a prune down to the newest N quick snapshots, twenty when no count is given
func (s SnapshotCommand) Prune(parts []string) ([]string, error) {
	keep := 20
	if len(parts) > 2 {
		count, ok := decimalCount(parts[2])
		if !ok {
			return []string{s.T("snapshot.usage_prune", nil)}, nil
		}
		keep = count
	}
	deleted, err := backup.PruneQuickSnapshots(keep, s.Home)
	if err != nil {
		return nil, err
	}
	return []string{s.T("snapshot.pruned", map[string]any{"deleted": deleted, "keep": keep})}, nil
}

// Snapshot id a restore names by id or list number, with lines instead when none is named or the number is off
func (s SnapshotCommand) RestoreTarget(parts []string) (string, []string, error) {
	if len(parts) < 3 {
		lines := []string{s.T("snapshot.usage_restore", nil)}
		snaps, err := backup.ListQuickSnapshots(1, s.Home)
		if err != nil {
			return "", nil, err
		}
		if len(snaps) > 0 {
			lines = append(lines, s.T("snapshot.most_recent", map[string]any{"snapshot_id": snapshotRow(snaps[0])["id"]}))
		}
		return "", lines, nil
	}
	snapID := parts[2]
	if idx, ok := pyInt(snapID); ok {
		snaps, err := backup.ListQuickSnapshots(20, s.Home)
		if err != nil {
			return "", nil, err
		}
		if idx < 1 || idx > len(snaps) {
			return "", []string{s.T("snapshot.invalid_number", map[string]any{"max": len(snaps)})}, nil
		}
		snapID = anyutil.String(snapshotRow(snaps[idx-1])["id"])
	}
	return snapID, nil, nil
}

// Lines reporting a finished restore, onGateway telling the gateway reloads the state by itself
func (s SnapshotCommand) RestoreReport(snapID string, restored bool, restoreErr error, onGateway bool) ([]string, error) {
	if restoreErr != nil {
		return []string{s.T("snapshot.restore_incomplete", map[string]any{"snapshot_id": snapID}), restoreErr.Error()}, nil
	}
	if restored {
		lines := []string{s.T("snapshot.restored", map[string]any{"snapshot_id": snapID})}
		if !onGateway {
			lines = append(lines, s.T("snapshot.restart_recommended", nil))
		}
		return lines, nil
	}
	all, err := backup.ListQuickSnapshots(1000000, s.Home)
	if err != nil {
		return nil, err
	}
	for _, row := range all {
		if anyutil.String(snapshotRow(row)["id"]) == snapID {
			return []string{s.T("snapshot.restore_incomplete", map[string]any{"snapshot_id": snapID})}, nil
		}
	}
	return []string{s.T("snapshot.not_found", map[string]any{"snapshot_id": snapID})}, nil
}

// Line telling a parked satellite profile is not served again yet, empty when it is or none was parked
func (r SnapshotRestore) ReserveLine() string {
	if r.Profile == "" || AckConfirmed(r.Ack, "served", r.Profile) {
		return ""
	}
	return fmt.Sprintf("Profile '%s' is not served again yet: %s. The host retries on its next rescan (within 30s).", r.Profile, AckFailure(r.Ack))
}

// Identify answer of the manually launched gateway owning the home, nil when none runs, an error for a supervised one
func ManualGatewayIdentity(h string, identify func(string) *jsonutil.Object, fail func(string, ...any) error) (*jsonutil.Object, error) {
	identity := identify(h)
	if identity == nil {
		_, _, running, err := gateway.GetRunningPidIdentityStrict(filepath.Join(h, "gateway.pid"))
		if err != nil {
			return nil, err
		}
		if running {
			return nil, fail("gateway has no usable control socket for %s; stop it through its owner and retry", h)
		}
		return nil, nil
	}
	claimed, _ := home.RecordValue(identity.Get, home.RecordKey)
	if !anyutil.IsTrue(claimed) {
		return nil, fail("gateway control identity does not match %s", h)
	}
	resolved, err := pathutil.Resolve(anyutil.String(claimed))
	if err != nil {
		return nil, err
	}
	if rel, ok := pathutil.RelativeTo(resolved, h); !ok || rel != "." {
		return nil, fail("gateway control identity does not match %s", h)
	}
	owner := gateway.LaunchSupervisor(identity)
	if owner != "manual" {
		hint, known := ownerStopHints[owner]
		if !known {
			hint = "stop it through its supervisor before retrying"
		}
		named := owner
		if named == "" {
			named = "unknown"
		}
		return nil, fail("%s gateway still owns %s; %s", named, h, hint)
	}
	return identity, nil
}

// Whether the identified gateway itself accepted the drain or is already stopping
func PauseAcked(response, identity *jsonutil.Object) bool {
	if response == nil || response.Len() == 0 || identity == nil {
		return false
	}
	respPid, _ := response.Get("pid")
	idPid, _ := identity.Get("pid")
	if !anyutil.Equal(respPid, idPid) {
		return false
	}
	pausing, _ := response.Get("pausing")
	stopping, _ := response.Get("already_stopping")
	return anyutil.IsTrue(pausing) || anyutil.IsTrue(stopping)
}

// Hands the profile off to maintenance, true when this restore parked a satellite and must serve it again
func PauseSnapshotOwner(h, controlHome string, identity *jsonutil.Object) (bool, error) {
	satellite := controlHome != h
	ours, accepted := false, false
	name := filepath.Base(h)
	if satellite {
		marker := profiles.ParkedMarkerPath(h)
		if f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666); err == nil {
			f.Close()
			ours = true
		} else if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		ack := gateway.RequestUnserveProfile(controlHome, name)
		accepted = AckConfirmed(ack, "unserved", name) ||
			(ack != nil && anyutil.IsTrue(ack.ValueOr("pending", nil)) && !anyutil.IsTrue(ack.ValueOr("error", nil)))
		if ours && !accepted {
			_ = os.Remove(marker)
		}
	} else {
		accepted = PauseAcked(gateway.PauseGatewayForUpdate(controlHome, 8*time.Second), identity)
	}
	if !accepted {
		retry := "Stop the gateway and retry."
		if satellite {
			retry = fmt.Sprintf("Park the profile with `icebath -p %s gateway stop` and retry.", name)
		}
		return ours, &gateway.GatewayClientError{Msg: "Gateway refused the snapshot maintenance handoff; nothing was restored. " + retry}
	}
	return ours, nil
}

// Restores a quick snapshot with the profile handed off the gateway, detach closing the caller's hold first
func RestoreSnapshotOffGateway(ctx context.Context, h, snapID string, identity func(string) (*jsonutil.Object, error), detach func(home string) error) (res SnapshotRestore, err error) {
	h, err = filepath.Abs(h)
	if err != nil {
		return res, err
	}
	if resolved, rerr := filepath.EvalSymlinks(h); rerr == nil {
		h = resolved
	}
	observed := gateway.DiscoverGatewayEndpoint(ctx, h, 2*time.Second)
	if observed.State != "ready" || observed.Endpoint == nil {
		return res, &gateway.GatewayClientError{Msg: "Local gateway is not ready for snapshot maintenance. Reconnect and retry."}
	}
	controlHome, err := filepath.Abs(gateway.ControlHomeFor(h, observed.Endpoint))
	if err != nil {
		return res, err
	}
	if resolved, rerr := filepath.EvalSymlinks(controlHome); rerr == nil {
		controlHome = resolved
	}
	var owner *jsonutil.Object
	if controlHome == h {
		owner, err = identity(h)
		if err != nil {
			return res, &gateway.GatewayClientError{Msg: fmt.Sprintf("Nothing was restored: %s.", err.Error())}
		}
	}
	if err := detach(h); err != nil {
		return res, err
	}
	res.Detached = true
	unserved := false
	defer func() {
		if unserved {
			_ = os.Remove(profiles.ParkedMarkerPath(h))
			res.Profile = filepath.Base(h)
			res.Ack = gateway.RequestServeProfileHot(controlHome, res.Profile)
		}
	}()
	ours, err := PauseSnapshotOwner(h, controlHome, owner)
	if err != nil {
		return res, err
	}
	unserved = ours
	unowned, err := manage.WaitProfileUnowned(h, nil)
	if err != nil {
		return res, err
	}
	if !unowned {
		return res, &gateway.GatewayClientError{Msg: "Gateway still owns the profile; nothing was restored. Wait for its running work to stop and retry."}
	}
	res.Restored, err = backup.RestoreQuickSnapshot(ctx, snapID, h)
	return res, err
}
