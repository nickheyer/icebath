package backend

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	xunicode "golang.org/x/text/encoding/unicode"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	procinfo "github.com/nickheyer/icebath/pkg/internal/proc"
)

const (
	DesktopInstallationFile = "desktop-installation.json"
	emptyLockReclaimPolls   = 20
	repairLockAttempts      = 40
	repairLockPoll          = 25 * time.Millisecond
	OwnCTEpsilonSeconds     = 0.005
	CreateTimeToleranceSecs = 2.0
	V1MaxAgeSeconds         = 1200
)

var (
	InstallationIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	repairLockPidRe  = regexp.MustCompile(`^\d+$`)
	repairLockCtRe   = regexp.MustCompile(`^ct:(\d+(?:\.\d+)?)$`)
)

// Draws the installation id of a fresh install, a random version 4 UUID in production
var newInstallationUUID = uuid.NewString

// IdentityState is how one recorded process identity reads against the live process table
type IdentityState string

const (
	IdentityDead    IdentityState = "dead"
	IdentityOurs    IdentityState = "ours"
	IdentityMatch   IdentityState = "match"
	IdentityUnknown IdentityState = "unknown"
)

// SyncJudgeEnv is the process facts an identity judgement reads synchronously
type SyncJudgeEnv struct {
	OurPID     int
	OurCT      func() *float64
	IsAlive    func(pid int) bool
	CreateTime func(pid int) *float64
	NowS       float64
}

// Judges a recorded pid and creation time, an identity without a comparable time live only within the v1 ceiling
func IdentityStateSync(pid int, recordedCT *float64, startedAt float64, env SyncJudgeEnv) IdentityState {
	if pid == 0 {
		return IdentityDead
	}
	if pid == env.OurPID {
		ourCT := env.OurCT()
		if recordedCT != nil && ourCT != nil && math.Abs(*recordedCT-*ourCT) <= OwnCTEpsilonSeconds {
			return IdentityOurs
		}
		return IdentityDead
	}
	if !env.IsAlive(pid) {
		return IdentityDead
	}
	var actual *float64
	if recordedCT != nil {
		actual = env.CreateTime(pid)
	}
	if recordedCT == nil || actual == nil {
		if env.NowS-startedAt <= V1MaxAgeSeconds {
			return IdentityUnknown
		}
		return IdentityDead
	}
	if math.Abs(*recordedCT-*actual) <= CreateTimeToleranceSecs {
		return IdentityMatch
	}
	return IdentityDead
}

// Whether a judged identity still holds what it claimed
func IsLiveIdentity(state IdentityState) bool {
	return state == IdentityOurs || state == IdentityMatch || state == IdentityUnknown
}

// Alive by signal zero and not a Linux or macOS zombie
func pidAliveNotZombie(pid int) bool {
	if !IsPidAlive(pid) {
		return false
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return !procinfo.Zombie(pid)
	}
	return true
}

// Creation time of a live pid in unix seconds on Linux, macOS and Windows, nil elsewhere or when unreadable
func ProcessCreateTimeSync(pid int) *float64 {
	if !IsPidAlive(pid) {
		return nil
	}
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		return nil
	}
	birth, err := procinfo.Birth(pid)
	if err != nil {
		return nil
	}
	return &birth
}

var ownCreateTime struct {
	once sync.Once
	ct   *float64
}

// Creation time of this process, read once
func OwnCreateTimeSync() *float64 {
	ownCreateTime.once.Do(func() { ownCreateTime.ct = ProcessCreateTimeSync(os.Getpid()) })
	return ownCreateTime.ct
}

// Whether a lock holder still runs, this pid only at this incarnation and an unprovable one within the v1 ceiling of the lock mtime
func LockHolderIsLiveSync(pid int, recordedCT *float64, writtenAtS float64, createTime func(int) *float64) bool {
	return IsLiveIdentity(IdentityStateSync(pid, recordedCT, writtenAtS, SyncJudgeEnv{
		OurPID: os.Getpid(), OurCT: OwnCreateTimeSync, IsAlive: pidAliveNotZombie, CreateTime: createTime,
		NowS: float64(time.Now().UnixMilli()) / 1000,
	}))
}

// Creation time as the lock text writes it, three decimals
func FormatCreateTime(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}

// Lowercase installation id of a desktop-installation.json body, empty when absent or invalid
func ParseInstallationID(raw string) string {
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	obj, _ := doc.(map[string]any)
	value, ok := obj["installationId"].(string)
	if !ok || !InstallationIDRe.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

// Installation id of a regular file this user owns, tightened to 0600 on POSIX, empty otherwise
func ReadInstallationID(filePath string) string {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	if ownedByOtherUser(info) {
		return ""
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		if err := os.Chmod(filePath, 0o600); err != nil {
			return ""
		}
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return ""
	}
	return ParseInstallationID(utf8Lossy(raw))
}

// Decodes bytes as UTF-8 the way Node reads a utf8 file, invalid sequences replaced
func utf8Lossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out, _ := xunicode.UTF8.NewDecoder().Bytes(b)
	return string(out)
}

// Liveness judge of repair lock holders for one wait, each pid creation time probed once
func repairLockHolderJudge() func(pid int, recordedCT *float64, writtenAtS float64) bool {
	createTimes := map[int]*float64{}
	createTime := func(pid int) *float64 {
		ct, ok := createTimes[pid]
		if !ok {
			ct = ProcessCreateTimeSync(pid)
			createTimes[pid] = ct
		}
		return ct
	}
	return func(pid int, recordedCT *float64, writtenAtS float64) bool {
		return LockHolderIsLiveSync(pid, recordedCT, writtenAtS, createTime)
	}
}

// Repair lock text naming this process and its creation time
func repairLockBody() string {
	body := strconv.Itoa(os.Getpid()) + "\n"
	if ct := OwnCreateTimeSync(); ct != nil {
		body += "ct:" + FormatCreateTime(*ct) + "\n"
	}
	return body
}

// Reclaims a repair lock whose holder is dead or that stayed empty too long, by rename then verify
func reclaimDeadRepairLock(repairPath string, emptyPolls int, holderIsLive func(int, *float64, float64) bool) bool {
	raw, err := os.ReadFile(repairPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(repairPath)
	if err != nil {
		return false
	}
	writtenAtS := float64(info.ModTime().UnixNano()) / 1e9
	lines := strings.Split(utf8Lossy(raw), "\n")
	second := ""
	if len(lines) > 1 {
		second = lines[1]
	}
	var dead bool
	if !repairLockPidRe.MatchString(lines[0]) {
		dead = emptyPolls >= emptyLockReclaimPolls
	} else if pid, err := strconv.Atoi(lines[0]); err != nil || pid > math.MaxInt32 {
		dead = true
	} else {
		var recorded *float64
		if m := repairLockCtRe.FindStringSubmatch(second); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				recorded = &v
			}
		}
		dead = !holderIsLive(pid, recorded, writtenAtS)
	}
	if !dead {
		return false
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	claim := fmt.Sprintf("%s.%d.%s.reclaim", repairPath, os.Getpid(), hex.EncodeToString(nonce))
	if err := os.Rename(repairPath, claim); err != nil {
		return false
	}
	if moved, err := os.ReadFile(claim); err == nil && bytes.Equal(moved, raw) {
		_ = os.Remove(claim)
		return true
	}
	if err := os.Link(claim, repairPath); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = os.Rename(claim, repairPath)
	}
	_ = os.Remove(claim)
	return false
}

// Counts consecutive empty lock polls, then reclaims a dead lock or backs off one poll
func waitOnContendedRepairLock(repairPath string, emptyPolls int, holderIsLive func(int, *float64, float64) bool) int {
	polls := 0
	if info, err := os.Stat(repairPath); err == nil && info.Size() == 0 {
		polls = emptyPolls + 1
	}
	if !reclaimDeadRepairLock(repairPath, polls, holderIsLive) {
		time.Sleep(repairLockPoll)
	}
	return polls
}

// Replaces a corrupt id file with a fresh id, never following or removing another user's file
func replaceInstallationIDFile(filePath, installationID string) error {
	info, err := os.Lstat(filePath)
	if err == nil {
		symlink := info.Mode()&fs.ModeSymlink != 0
		switch {
		case !info.Mode().IsRegular() && !symlink:
			return errors.New("Desktop installation ID path is not a regular file.")
		case !symlink && ownedByOtherUser(info):
			return errors.New("Desktop installation ID is owned by another user.")
		}
		err = os.Remove(filePath)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return hardening.NodeErr(err)
	}
	body, _ := json.Marshal(map[string]string{"installationId": installationID})
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return hardening.NodeErr(err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return hardening.NodeErr(err)
	}
	return hardening.NodeErr(f.Close())
}

// Closes the lock and deletes it only while it still holds the body this process wrote
func releaseRepairLock(lock *os.File, repairPath string, ownedBody []byte) {
	lock.Close()
	if ownedBody == nil {
		return
	}
	if current, err := os.ReadFile(repairPath); err == nil && bytes.Equal(current, ownedBody) {
		_ = os.Remove(repairPath)
	}
}

// Installation id of this desktop, created once under a repair lock that reclaims a crashed holder
func LoadOrCreateInstallationID(filePath string) (string, error) {
	if existing := ReadInstallationID(filePath); existing != "" {
		return existing, nil
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o777); err != nil {
		return "", hardening.NodeErr(err)
	}
	installationID := strings.ToLower(newInstallationUUID())
	if !InstallationIDRe.MatchString(installationID) {
		return "", errors.New("Could not generate a valid desktop installation ID.")
	}
	repairPath := filePath + ".repair.lock"
	holderIsLive := repairLockHolderJudge()
	emptyPolls := 0
	for range repairLockAttempts {
		lock, err := os.OpenFile(repairPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return "", hardening.NodeErr(err)
			}
			if winner := ReadInstallationID(filePath); winner != "" {
				return winner, nil
			}
			emptyPolls = waitOnContendedRepairLock(repairPath, emptyPolls, holderIsLive)
			continue
		}
		id, err := claimInstallationID(lock, repairPath, filePath, installationID)
		return id, err
	}
	return "", errors.New("Could not repair the desktop installation ID.")
}

// Writes the lock body, then adopts a winner that appeared meanwhile or writes the fresh id
func claimInstallationID(lock *os.File, repairPath, filePath, installationID string) (string, error) {
	ownedBody := []byte(repairLockBody())
	defer func() { releaseRepairLock(lock, repairPath, ownedBody) }()
	if _, err := lock.Write(ownedBody); err != nil {
		return "", hardening.NodeErr(err)
	}
	if winner := ReadInstallationID(filePath); winner != "" {
		return winner, nil
	}
	if err := replaceInstallationIDFile(filePath, installationID); err != nil {
		return "", err
	}
	return installationID, nil
}

// Stable 32 hex owner id of an SSH scope on this installation
func SSHOwnershipID(installationID, scope string) (string, error) {
	if !InstallationIDRe.MatchString(installationID) {
		return "", errors.New("Desktop installation ID is invalid.")
	}
	sum := sha256.Sum256([]byte(installationID + "\x00" + scope))
	return hex.EncodeToString(sum[:])[:32], nil
}
