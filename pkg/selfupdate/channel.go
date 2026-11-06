package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	ChannelMain   = "main"
	ChannelStable = "stable"
	ChannelCanary = "canary"
)

const OfficialRepository = "nickheyer/icebath"

var canaryTagRE = regexp.MustCompile(`^v(?:0|[1-9]\p{Nd}{0,2})\.(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)\+canary\.20\p{Nd}{6}T\p{Nd}{6}Z$`)

var canaryStampRE = regexp.MustCompile(`^20\p{Nd}{6}T\p{Nd}{6}Z$`)

var githubOriginRE = regexp.MustCompile(`(?i)^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

var channelNameRE = regexp.MustCompile(`^(?:[a-z0-9]+(?:-[a-z0-9]+)*)$`)

var reservedChannelNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for i := 1; i <= 9; i++ {
		reservedChannelNames[fmt.Sprintf("com%d", i)] = true
		reservedChannelNames[fmt.Sprintf("lpt%d", i)] = true
	}
}

type ChannelError struct{ Msg string }

func (e *ChannelError) Error() string { return e.Msg }

// Returns a lowercase hyphenated channel name of at most 32 characters that names no reserved device
func ValidateChannelName(value any) (string, error) {
	name, ok := value.(string)
	if !ok || !channelNameRE.MatchString(name) {
		return "", &ChannelError{Msg: "Invalid channel name"}
	}
	if len(name) > 32 || reservedChannelNames[name] {
		return "", &ChannelError{Msg: "Invalid channel name"}
	}
	return name, nil
}

// Reports whether tag is a stable version carrying a full UTC canary build timestamp
func IsCanaryTag(tag any) bool {
	text, ok := tag.(string)
	return ok && canaryTagRE.MatchString(pystr.Strip(text))
}

// Returns the UTC build stamp of a canonical canary tag, nil for any other value
func CanaryTimestamp(tag any) *string {
	if !IsCanaryTag(tag) {
		return nil
	}
	_, stamp, _ := strings.Cut(pystr.Strip(tag.(string)), "+canary.")
	return &stamp
}

// Builds the canary identity of a stable version and a full UTC timestamp
func CanaryTagForDate(version, dateUTC string) (string, error) {
	core := strings.TrimPrefix(version, "v")
	if !updaterecord.StableTagRE.MatchString("v" + core) {
		return "", fmt.Errorf("invalid stable version for canary: %s", pyrepr.Repr(version))
	}
	if !canaryStampRE.MatchString(dateUTC) {
		return "", fmt.Errorf("invalid canary UTC timestamp: %s", pyrepr.Repr(dateUTC))
	}
	return "v" + core + "+canary." + dateUTC, nil
}

// Sixteen hex digit key of the install at projectRoot, the running install when empty
func InstallID(projectRoot string) string {
	if projectRoot == "" {
		projectRoot = pm.InstallRoot()
	}
	return pm.InstallKey(projectRoot)
}

func installRecords(cfg any) map[string]any {
	root, ok := cfg.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	update, ok := root["update"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	installs, ok := update["installs"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return installs
}

// Returns the path and channel record config holds for the install, empty when absent or malformed
func ChannelRecord(cfg any, projectRoot string) map[string]any {
	if record, ok := installRecords(cfg)[InstallID(projectRoot)].(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

func stampGet(stamp *pyjson.Object, key string) any {
	value, _ := stamp.Get(key)
	return value
}

// Reports whether a packaged payload or OS updater owns the install's channel identity
func packageChannel(stamp *pyjson.Object) bool {
	switch stampGet(stamp, "payload") {
	case "bundled", "light", "runtime":
		return true
	}
	switch stampGet(stamp, "updateMechanism") {
	case "electron-updater", "app-installer", "microsoft-store":
		return true
	}
	return false
}

func rootOrInstall(projectRoot string) string {
	if projectRoot == "" {
		return pm.InstallRoot()
	}
	return projectRoot
}

// Channel an unconfigured install tracks, stable for official releases and the baked identity of packages
func DefaultChannel(projectRoot string) (string, error) {
	root := rootOrInstall(projectRoot)
	stamp := pm.ReadInstallStamp(root)
	if !packageChannel(stamp) {
		official, err := tracksOfficialOrigin(root)
		if err != nil {
			return "", err
		}
		if official {
			return ChannelStable, nil
		}
		return ChannelMain, nil
	}
	if stampGet(stamp, "source") == "channel-build" {
		request, ok := stampGet(stamp, "channelBuild").(*pyjson.Object)
		if !ok {
			return "", errors.New("Channel bundle has no baked subscription")
		}
		return ValidateChannelName(stampGet(request, "channel"))
	}
	if IsCanaryTag(stampGet(stamp, "tag")) {
		return ChannelCanary, nil
	}
	return ChannelStable, nil
}

var releaseBuildRE = regexp.MustCompile(`^v(?:0|[1-9]\p{Nd}{0,2})\.(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)(?:-[a-z0-9]+(?:-[a-z0-9]+)*\.\p{Nd}+)?$`)

// Reports whether root is a checkout of the official repository or the running official release binary
func tracksOfficialOrigin(root string) (bool, error) {
	if official, err := checkoutTracksOfficialOrigin(root); official || err != nil {
		return official, err
	}
	version := updaterecord.BuildVersion()
	if !releaseBuildRE.MatchString(version) && !IsCanaryTag(version) {
		return false, nil
	}
	live, errLive := os.Stat(pm.RepoRoot())
	here, errHere := os.Stat(root)
	return errLive == nil && errHere == nil && os.SameFile(live, here), nil
}

// Reads remote.origin.url from the checkout's git config file and compares it with the official repository
func checkoutTracksOfficialOrigin(root string) (bool, error) {
	common, err := updaterecord.GitCommonDir(root)
	if common == "" || err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		return false, nil
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return false, err
	}
	inOrigin := false
	for _, line := range pystr.Splitlines(text, false) {
		line = pystr.Strip(line)
		key, value, _ := strings.Cut(line, "=")
		switch {
		case strings.HasPrefix(line, "["):
			inOrigin = line == `[remote "origin"]`
		case inOrigin && pystr.Strip(key) == "url":
			match := githubOriginRE.FindStringSubmatch(pystr.Strip(value))
			return match != nil && strings.ToLower(match[1]) == strings.ToLower(OfficialRepository), nil
		}
	}
	return false, nil
}

// Reports whether no channel was ever persisted and channel is the install's default
func RidesDefaultChannel(record map[string]any, channel, projectRoot string) (bool, error) {
	if value, ok := record["channel"]; ok && value != nil {
		return false, nil
	}
	def, err := DefaultChannel(projectRoot)
	if err != nil {
		return false, err
	}
	return channel == def, nil
}

// Channel this install updates from, the package identity first, then its record, then the default
func ResolveUpdateChannel(cfg any, projectRoot string) (string, error) {
	root := rootOrInstall(projectRoot)
	if packageChannel(pm.ReadInstallStamp(root)) {
		return DefaultChannel(root)
	}
	if configured, ok := ChannelRecord(cfg, root)["channel"]; ok && configured != nil {
		return ValidateChannelName(configured)
	}
	return DefaultChannel(root)
}

// Persists channel for this install in config.yaml and returns its id, refusing commit builds and owned installs
func SetInstallChannel(ctx context.Context, channel, projectRoot string) (string, error) {
	root := rootOrInstall(projectRoot)
	if IsCommitBuild(root) {
		return "", errors.New(CommitBuildUpdateMessage)
	}
	channel, err := ValidateChannelName(channel)
	if err != nil {
		return "", err
	}
	stamp := pm.ReadInstallStamp(root)
	if packageChannel(stamp) || stampGet(stamp, "updateMechanism") == "external" {
		distribution := stampGet(stamp, "distribution")
		if !pyrepr.Truthy(distribution) {
			distribution = "an external steward"
		}
		return "", fmt.Errorf("channels don't apply here; updates are owned by %s", pyrepr.Str(distribution))
	}
	sha16 := InstallID(root)
	if _, err := writeChannelRecord(ctx, sha16, root, channel, nil, ""); err != nil {
		return "", err
	}
	return sha16, nil
}

// Prints the install id or persists a channel before any update side effect, true when handled
func HandleMetadataArgs(ctx context.Context, stdout io.Writer, installIDFlag bool, setChannel *string, projectRoot string) (bool, error) {
	if installIDFlag {
		fmt.Fprintln(stdout, InstallID(projectRoot))
		return true, nil
	}
	if setChannel == nil {
		return false, nil
	}
	channel := *setChannel
	key, err := SetInstallChannel(ctx, channel, projectRoot)
	if err != nil {
		fmt.Fprintln(stdout, err.Error())
		return true, &pm.SystemExit{Code: 2}
	}
	fmt.Fprintf(stdout, "Update channel for %s: %s\n", key, channel)
	switch channel {
	case ChannelCanary:
		fmt.Fprintln(stdout, "Canary builds can write forward-incompatible state. Back up your data before switching.")
	case ChannelStable:
		fmt.Fprintln(stdout, "Switching to an older stable release may not read state written by canary. Back up your data first.")
	}
	return true, nil
}

// Holds the process config lock and the cross process channels lock beside configPath
func channelWriteLock(ctx context.Context, configPath string) (context.Context, func(), error) {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o777); err != nil {
		return ctx, nil, err
	}
	locked, unlockConfig := config.LockConfig(ctx)
	lockPath := strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".channels.lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o666)
	if err != nil {
		unlockConfig()
		return ctx, nil, err
	}
	if runtime.GOOS == "windows" {
		if _, err := file.Write([]byte{0}); err != nil {
			file.Close()
			unlockConfig()
			return ctx, nil, err
		}
	}
	if err := pyfs.LockFile(file); err != nil {
		file.Close()
		unlockConfig()
		return ctx, nil, err
	}
	return locked, func() {
		_ = pyfs.UnlockFile(file)
		file.Close()
		unlockConfig()
	}, nil
}

// Adopts a retired channel's destination after verified completion when the subscription is unchanged
func AdoptRetiredChannel(ctx context.Context, request map[string]any) (bool, error) {
	retirement, ok := request["channel_retirement"]
	if !ok || retirement == nil {
		return false, nil
	}
	fields, _ := retirement.(map[string]any)
	source, _ := request["source"].(string)
	homeDir, _ := request["home"].(string)
	destination, err := ValidateChannelName(fields["destination"])
	if err != nil {
		return false, err
	}
	original, _ := fields["original"].(map[string]any)
	if original == nil {
		original = map[string]any{}
	}
	return writeChannelRecord(ctx, InstallID(source), source, destination, original, filepath.Join(homeDir, "config.yaml"))
}

func writeChannelRecord(ctx context.Context, sha16, path, channel string, expected map[string]any, configPath string) (bool, error) {
	if configPath == "" {
		configPath = home.ConfigPath(home.Dir(ctx))
	}
	locked, release, err := channelWriteLock(ctx, configPath)
	if err != nil {
		return false, err
	}
	defer release()
	return writeChannelRecordLocked(locked, sha16, path, channel, expected, configPath)
}

// Writes update.installs.<sha16> keeping every other key, refusing malformed update sections
func writeChannelRecordLocked(ctx context.Context, sha16, path, channel string, expected map[string]any, configPath string) (bool, error) {
	existing, err := config.RequireReadableConfigBeforeWrite(ctx, configPath)
	if err != nil {
		return false, err
	}
	update, present := existing["update"]
	updateMap, isMap := update.(map[string]any)
	if present && update != nil && !isMap {
		return false, errors.New("config key 'update' is not a mapping")
	}
	var installs map[string]any
	if isMap {
		value, ok := updateMap["installs"]
		installs, isMap = value.(map[string]any)
		if ok && value != nil && !isMap {
			return false, errors.New("config key 'update.installs' is not a mapping")
		}
	}
	record, _ := installs[sha16].(map[string]any)
	if expected != nil {
		current := record
		if len(current) == 0 {
			current = map[string]any{}
		}
		if !reflect.DeepEqual(current, expected) {
			return false, nil
		}
	}
	prefix := "update.installs." + sha16
	updates := []config.KeyValue{{Key: prefix + ".path", Value: path}, {Key: prefix + ".channel", Value: channel}}
	if _, ok := installs[sha16]; ok && record == nil {
		replacement := []config.KeyValue{{Key: prefix, Value: map[string]any{"path": path}}}
		updates = append(replacement, updates[1:]...)
	}
	if err := config.WriteConfigKeys(ctx, configPath, updates); err != nil {
		return false, err
	}
	return true, nil
}

type StaleChannelRecord struct {
	SHA16  string
	Record map[string]any
	Reason string
}

// Doctor's missing, replaced and unclaimed verdicts over update.installs records, in key order
func StaleChannelRecords(ctx context.Context, cfg any) ([]StaleChannelRecord, error) {
	records := installRecords(cfg)
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var stale []StaleChannelRecord
	for _, sha16 := range keys {
		record, ok := records[sha16].(map[string]any)
		if !ok {
			continue
		}
		if recorded, ok := record["path"].(string); ok && recorded != "" {
			exists, err := pathExists(recorded)
			if err != nil {
				return nil, err
			}
			if !exists {
				stale = append(stale, StaleChannelRecord{sha16, record, "missing"})
				continue
			}
			if pm.InstallKey(recorded) != sha16 {
				stale = append(stale, StaleChannelRecord{sha16, record, "replaced"})
				continue
			}
		}
		info, err := os.Stat(filepath.Join(pm.InstallsRoot(ctx), sha16, "install.json"))
		switch {
		case err == nil && info.Mode().IsRegular():
		case err == nil || ignorableStatError(err):
			stale = append(stale, StaleChannelRecord{sha16, record, "unclaimed"})
		default:
			slog.Debug("installs root unavailable", "error", err)
		}
	}
	return stale, nil
}

// Reports whether a stat error is one pathlib's exists and is_file treat as absence
func ignorableStatError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP)
}

// Path.exists, absent on the ignorable stat errors and raising any other
func pathExists(path string) (bool, error) {
	if strings.ContainsRune(path, 0) {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if ignorableStatError(err) {
		return false, nil
	}
	return false, err
}
