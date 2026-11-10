package state

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	retainedConnectionsMu sync.Mutex
	retainedConnections   []any
)

// Function that keeps a quarantined connection and its descriptors referenced until process exit
func prepareConnectionRetirement() func(conn any) {
	return func(conn any) {
		retainedConnectionsMu.Lock()
		defer retainedConnectionsMu.Unlock()
		retainedConnections = append(retainedConnections, conn)
	}
}

type headerProbeFD struct {
	file *os.File
	id   FileIdentity
}

var (
	headerProbeMu         sync.Mutex
	headerProbeFDs        = map[string]headerProbeFD{}
	retiredHeaderProbeFDs []*os.File
	ftsTableNames         = []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"}
)

// Platform the deleted sidecar scan and holder count dispatch on
var dbfilePlatform = runtime.GOOS

// Reads a range of a possibly live database through a cached never closed descriptor, false when unreadable
func preadDBRange(dbPath string, offset int64, length int) ([]byte, bool) {
	if isWindows {
		f, err := os.Open(dbPath)
		if err != nil {
			return nil, false
		}
		defer f.Close()
		buf := make([]byte, length)
		n, err := f.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			return nil, false
		}
		return buf[:n], true
	}
	st, ok := statFileIdentity(dbPath)
	if !ok {
		return nil, false
	}
	headerProbeMu.Lock()
	defer headerProbeMu.Unlock()
	cached, have := headerProbeFDs[dbPath]
	if have && cached.id != st {
		retiredHeaderProbeFDs = append(retiredHeaderProbeFDs, cached.file)
		delete(headerProbeFDs, dbPath)
		have = false
	}
	if !have {
		f, err := os.Open(dbPath)
		if err != nil {
			return nil, false
		}
		id, err := openFileIdentity(f)
		if err != nil {
			retiredHeaderProbeFDs = append(retiredHeaderProbeFDs, f)
			return nil, false
		}
		cached = headerProbeFD{f, id}
		headerProbeFDs[dbPath] = cached
	}
	buf := make([]byte, length)
	n, err := cached.file.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, false
	}
	return buf[:n], true
}

// Lock safe raw header read of a possibly live database
func preadDBHeader(dbPath string, length int) ([]byte, bool) {
	return preadDBRange(dbPath, 0, length)
}

// application_id from the SQLite header, false when the header is short or not SQLite
func readSQLiteApplicationID(dbPath string) (uint32, bool) {
	end := stateDBApplicationIDOffset + 4
	header, ok := preadDBHeader(dbPath, end)
	if !ok || len(header) < end || string(header[:16]) != "SQLite format 3\x00" {
		return 0, false
	}
	return binary.BigEndian.Uint32(header[stateDBApplicationIDOffset:end]), true
}

// Device and inode of each existing wal and shm sidecar keyed by suffix
func statSQLiteSidecarIdentity(dbPath string) map[string]FileIdentity {
	idents := map[string]FileIdentity{}
	for _, suffix := range []string{"-wal", "-shm"} {
		if id, ok := StatDBFileIdentity(dbPath + suffix); ok {
			idents[suffix] = id
		}
	}
	return idents
}

// Canonical form of every watched sidecar spelling mapped to its literal still named path
func watchedSQLiteSidecarPaths(dbPath string) (map[string]string, error) {
	literalBase, err := pyAbspath(dbPath)
	if err != nil {
		return nil, err
	}
	watched := map[string]string{}
	for _, path := range []string{literalBase + "-wal", literalBase + "-shm"} {
		canonical, err := CanonicalSQLitePath(path)
		if err != nil {
			return nil, err
		}
		watched[canonical] = path
	}
	resolvedBases := []string{
		filepath.Join(realpathLax(filepath.Dir(literalBase)), filepath.Base(literalBase)),
		realpathLax(literalBase),
	}
	for _, base := range resolvedBases {
		for _, suffix := range []string{"-wal", "-shm"} {
			canonical, err := CanonicalSQLitePath(base + suffix)
			if err != nil {
				return nil, err
			}
			if _, ok := watched[canonical]; !ok {
				watched[canonical] = base + suffix
			}
		}
	}
	return watched, nil
}

// Reports the identity naming a generation the watched path no longer holds
func identityIsTrulyUnlinked(identity FileIdentity, watchedPath string) bool {
	watched, ok := statFileIdentity(watchedPath)
	return !ok || identity != watched
}

// Reports a deleted fd target that really names an orphaned generation, a closed fd or exited process never counting
func fdIsTrulyUnlinked(fdPath, watchedPath string) bool {
	id, err := statIdentityErr(fdPath)
	if err != nil {
		return !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ESRCH)
	}
	return identityIsTrulyUnlinked(id, watchedPath)
}

// Calls fn with pid, readlink target and fd path for every readable /proc descriptor entry
func iterProcFDTargets(fn func(pid int, target, fdPath string)) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pidStr := entry.Name()
		if pidStr == "" || !pystr.All(pidStr, pystr.IsDigit) {
			continue
		}
		fdDir := "/proc/" + pidStr + "/fd"
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			fdPath := fdDir + "/" + fd.Name()
			n, ok := pyint.Parse(pidStr)
			if !ok || !n.IsInt64() {
				return &pyConvError{"ValueError", "invalid literal for int() with base 10: " + pyrepr.StrRepr(pidStr)}
			}
			target, err := os.Readlink(fdPath)
			if err != nil {
				continue
			}
			fn(int(n.Int64()), target, fdPath)
		}
	}
	return nil
}

const (
	darwinAllPIDsType           = 1
	darwinPIDListFDs            = 1
	darwinPIDFDVnodePathInfo    = 2
	darwinProcFDInfoSize        = 8
	darwinFDRecordSize          = 1200
	darwinFDDevOffset           = 24
	darwinFDInoOffset           = 32
	darwinFDPathOffset          = 176
	defaultDarwinFDScanTimeoutS = 5.0
)

// libproc entry points the macOS descriptor scan calls, each returning bytes used or zero on failure
type darwinLibprocAPI interface {
	ProcListPIDs(typ, typeinfo uint32, buf []byte) int
	ProcPIDInfo(pid, flavor int, arg uint64, buf []byte) int
	ProcPIDFDInfo(pid, fd, flavor int, buf []byte) int
}

var (
	darwinLibprocMu     sync.Mutex
	darwinLibprocHandle darwinLibprocAPI
	darwinFDScanTimeout = defaultDarwinFDScanTimeoutS
)

// libproc of this process, loaded once
var darwinLibproc = func() (darwinLibprocAPI, error) {
	darwinLibprocMu.Lock()
	defer darwinLibprocMu.Unlock()
	if darwinLibprocHandle != nil {
		return darwinLibprocHandle, nil
	}
	lib, err := loadDarwinLibproc()
	if err != nil {
		return nil, err
	}
	darwinLibprocHandle = lib
	return lib, nil
}

// Every positive pid proc_listpids names, the buffer doubled until the kernel leaves room
var darwinAllPIDs = func(lib darwinLibprocAPI) []int {
	size := 4096 * 8
	for {
		buf := make([]byte, size)
		used := lib.ProcListPIDs(darwinAllPIDsType, 0, buf)
		if used <= 0 {
			return []int{}
		}
		if used < size {
			pids := []int{}
			for off := 0; off+4 <= used/4*4; off += 4 {
				if pid := int32(binary.LittleEndian.Uint32(buf[off:])); pid > 0 {
					pids = append(pids, int(pid))
				}
			}
			return pids
		}
		size *= 2
	}
}

// Calls fn with pid, fd, last pathname and identity for every vnode descriptor libproc reports
func iterDarwinFDTargets(fn func(pid, fd int, target string, identity FileIdentity)) error {
	return iterDarwinFDTargetsWith(darwinLibproc, darwinAllPIDs, fn)
}

// Descriptor scan over the given libproc loader and pid lister, resolved before a scan goroutine starts
func iterDarwinFDTargetsWith(loadLib func() (darwinLibprocAPI, error), allPIDs func(darwinLibprocAPI) []int, fn func(pid, fd int, target string, identity FileIdentity)) error {
	lib, err := loadLib()
	if err != nil {
		return err
	}
	for _, pid := range allPIDs(lib) {
		size := 4096
		var listing []byte
		used := 0
		for {
			listing = make([]byte, size)
			used = lib.ProcPIDInfo(pid, darwinPIDListFDs, 0, listing)
			if used <= 0 || used < size {
				break
			}
			size *= 2
		}
		for offset := 0; offset < used-darwinProcFDInfoSize+1; offset += darwinProcFDInfoSize {
			fd := int(int32(binary.LittleEndian.Uint32(listing[offset:])))
			record := make([]byte, darwinFDRecordSize)
			if lib.ProcPIDFDInfo(pid, fd, darwinPIDFDVnodePathInfo, record) <= 0 {
				continue
			}
			identity := FileIdentity{
				Dev: uint64(binary.LittleEndian.Uint32(record[darwinFDDevOffset:])),
				Ino: binary.LittleEndian.Uint64(record[darwinFDInoOffset:]),
			}
			raw := record[darwinFDPathOffset:]
			if i := slices.Index(raw, 0); i >= 0 {
				raw = raw[:i]
			}
			fn(pid, fd, pystr.DecodeUTF8Replace(raw), identity)
		}
	}
	return nil
}

type darwinScanResult struct {
	holders []Holder
	err     error
	panic   any
}

// macOS deleted sidecar holders by identity, bounded by the scan deadline and failing open with a warning
func iterDarwinSidecarHolders(dbPath string) ([]Holder, error) {
	abs, err := pyAbspath(dbPath)
	if err != nil {
		return nil, err
	}
	base := realpathLax(abs)
	watched := map[string]string{}
	for _, path := range []string{base + "-wal", base + "-shm"} {
		watched[pystr.Casefold(path)] = path
	}
	done := make(chan darwinScanResult, 1)
	loadLib, allPIDs := darwinLibproc, darwinAllPIDs
	go func() {
		holders := []Holder{}
		defer func() {
			if r := recover(); r != nil {
				done <- darwinScanResult{panic: r}
			}
		}()
		err := iterDarwinFDTargetsWith(loadLib, allPIDs, func(pid, _ int, target string, identity FileIdentity) {
			if literal, ok := watched[pystr.Casefold(target)]; ok && identityIsTrulyUnlinked(identity, literal) {
				holders = append(holders, Holder{PID: pid, Target: target})
			}
		})
		done <- darwinScanResult{holders: holders, err: err}
	}()
	timeout := darwinFDScanTimeout
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case res := <-done:
		if res.panic != nil {
			panic(res.panic)
		}
		if res.err != nil {
			return nil, res.err
		}
		return res.holders, nil
	case <-timer.C:
		slog.Warn(fmt.Sprintf("deleted-WAL holder scan timed out after %.1fs for %s; continuing fail-open", timeout, dbPath))
		return []Holder{}, nil
	}
}

// Processes holding an unlinked wal or shm sidecar of the database, this process included, empty on other hosts
func IterDeletedSQLiteSidecarHolders(dbPath string) []Holder {
	if dbfilePlatform == "windows" {
		return []Holder{}
	}
	holders := []Holder{}
	var err error
	switch dbfilePlatform {
	case "darwin":
		var found []Holder
		if found, err = iterDarwinSidecarHolders(dbPath); err == nil {
			holders = found
		}
	case "linux":
		var watched map[string]string
		if watched, err = watchedSQLiteSidecarPaths(dbPath); err == nil {
			err = iterProcFDTargets(func(pid int, target, fdPath string) {
				if err != nil {
					return
				}
				canonical, cerr := CanonicalSQLitePath(target)
				if cerr != nil {
					err = cerr
					return
				}
				literal, ok := watched[canonical]
				if strings.Contains(target, " (deleted)") && ok && fdIsTrulyUnlinked(fdPath, literal) {
					holders = append(holders, Holder{PID: pid, Target: target})
				}
			})
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("deleted-WAL holder scan failed for %s: %s", dbPath, errText(err)))
	}
	return holders
}

// Refuses an open while any process holds a deleted wal or shm generation of the database
func RefuseDeletedWALGeneration(dbPath string) error {
	if len(IterDeletedSQLiteSidecarHolders(dbPath)) == 0 {
		return nil
	}
	slog.Error(DeletedWalGenerationMsg)
	return &DeletedWalGenerationError{Msg: DeletedWalGenerationMsg}
}

const (
	RetiredGenerationDirSuffix       = ".retired-wal-"
	RetiredGenerationManifest        = "manifest.json"
	RetiredGenerationManifestVersion = 1

	captureChunkBytes = 8 * 1024 * 1024
	sqliteHeaderBytes = 100
)

// Main image size up to which a capture copies the whole file instead of its header
var RetiredGenerationMainImageMaxBytes int64 = 512 * 1024 * 1024

// Fsyncs a file or directory this process owns, a no op on Windows
func fsyncPath(path string) error {
	if isWindows {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	serr := f.Sync()
	cerr := f.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// This process's open descriptor for the identity, never matched by pathname, false on a miss
func ownDescriptorForIdentity(identity FileIdentity) (int, bool) {
	for _, fdDir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || !pystr.All(name, pystr.IsDigit) {
				continue
			}
			fd, err := strconv.Atoi(name)
			if err != nil {
				continue
			}
			if id, err := fdIdentity(fd); err == nil && id == identity {
				return fd, true
			}
		}
		return 0, false
	}
	return 0, false
}

// Streams size bytes through read into dest via a synced temp file, a short read refusing the capture
func copyRange(read func(offset int64, length int) ([]byte, error), dest string, size int64) (map[string]any, error) {
	digest := sha256.New()
	part := dest + ".part"
	var offset int64
	err := func() error {
		out, err := os.Create(part)
		if err != nil {
			return err
		}
		defer out.Close()
		for offset < size {
			chunk, err := read(offset, int(min(int64(captureChunkBytes), size-offset)))
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				break
			}
			if _, err := out.Write(chunk); err != nil {
				return err
			}
			digest.Write(chunk)
			offset += int64(len(chunk))
		}
		if err := out.Sync(); err != nil {
			return err
		}
		return out.Close()
	}()
	if err != nil {
		os.Remove(part)
		return nil, err
	}
	if offset != size {
		os.Remove(part)
		return nil, &RetiredGenerationCaptureError{Msg: fmt.Sprintf("short read copying %s: %d of %d bytes — the retired generation "+
			"is NOT fully captured; the handle stays open for a retry", filepath.Base(dest), offset, size)}
	}
	if err := os.Rename(part, dest); err != nil {
		return nil, err
	}
	return map[string]any{"file": filepath.Base(dest), "bytes": offset, "sha256": hex.EncodeToString(digest.Sum(nil))}, nil
}

// Copies a descriptor SQLite owns through pread, never closing it
func copyDescriptor(fd int, dest string, size int64) (map[string]any, error) {
	return copyRange(func(offset int64, length int) ([]byte, error) { return rawPread(fd, length, offset) }, dest, size)
}

// Copies the live main file through the lock safe cached descriptor
func copyMainImage(dbPath, dest string, size int64) (map[string]any, error) {
	return copyRange(func(offset int64, length int) ([]byte, error) {
		chunk, _ := preadDBRange(dbPath, offset, length)
		return chunk, nil
	}, dest, size)
}

// Fields of a SQLite header, valid false when it is short or not SQLite
func parseSQLiteHeader(header []byte) map[string]any {
	if len(header) < sqliteHeaderBytes || string(header[:16]) != "SQLite format 3\x00" {
		return map[string]any{"valid": false}
	}
	pageSize := int64(binary.BigEndian.Uint16(header[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	parsed := map[string]any{"valid": true, "page_size": pageSize}
	for name, off := range map[string]int{"change_counter": 24, "page_count": 28, "user_version": 60,
		"application_id": stateDBApplicationIDOffset, "version_valid_for": 92} {
		parsed[name] = int64(binary.BigEndian.Uint32(header[off : off+4]))
	}
	return parsed
}

// Python tuple text of an identity
func identityRepr(id FileIdentity) string { return fmt.Sprintf("(%d, %d)", id.Dev, id.Ino) }

// Reports an error Python raises as OSError
func isOSError(err error) bool {
	var errno syscall.Errno
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	return errors.As(err, &errno) || errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr)
}

// Durably captures the lost wal generation this process still holds open and returns the artifact directory
func CaptureRetiredWALGeneration(ctx context.Context, dbPath string, sidecarIdentity map[string]FileIdentity, trigger string) (string, error) {
	walIdentity, ok := sidecarIdentity["-wal"]
	if !ok {
		return "", &RetiredGenerationCaptureError{Msg: fmt.Sprintf("no recorded WAL generation identity for %s; refusing to guess it by pathname", dbPath)}
	}
	walFD, ok := ownDescriptorForIdentity(walIdentity)
	if !ok {
		return "", &RetiredGenerationCaptureError{Msg: fmt.Sprintf("this process no longer holds the retired WAL inode %s of %s", identityRepr(walIdentity), dbPath)}
	}
	walSize, err := fdSize(walFD)
	if err != nil {
		return "", &RetiredGenerationCaptureError{Msg: fmt.Sprintf("cannot stat the retired WAL of %s: %s", dbPath, pypath.OSErrorText(err))}
	}
	name := filepath.Base(dbPath)
	stem := name + RetiredGenerationDirSuffix + time.Now().UTC().Format("20060102-150405") + "-" + strconv.Itoa(os.Getpid())
	final := filepath.Join(filepath.Dir(dbPath), stem)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for n := 1; exists(final) || exists(final+".partial"); n++ {
		final = filepath.Join(filepath.Dir(dbPath), fmt.Sprintf("%s-%d", stem, n))
	}
	staging := final + ".partial"
	err = func() error {
		if err := os.MkdirAll(filepath.Dir(staging), 0o777); err != nil {
			return err
		}
		if err := os.Mkdir(staging, 0o777); err != nil {
			return err
		}
		walCopy, err := copyDescriptor(walFD, filepath.Join(staging, name+"-wal"), walSize)
		if err != nil {
			return err
		}
		walCopy["identity"] = []any{walIdentity.Dev, walIdentity.Ino}
		pathGeneration := map[string]any{}
		for suffix, id := range statSQLiteSidecarIdentity(dbPath) {
			pathGeneration[suffix] = []any{id.Dev, id.Ino}
		}
		manifest := map[string]any{
			"version":                    RetiredGenerationManifestVersion,
			"database":                   dbPath,
			"trigger":                    trigger,
			"pid":                        os.Getpid(),
			"captured_at":                time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			"python":                     runtime.Version(),
			"sqlite":                     LinkedSQLiteVersion(),
			"wal":                        walCopy,
			"shm":                        nil,
			"path_generation_at_capture": pathGeneration,
			"note": "Frames in the captured WAL were committed by the retired generation. Whether they " +
				"belong on top of the main file now at the path is an operator decision; inspect " +
				"the copied image with `icebath sessions recover --inspect-only` first.",
		}
		if shmIdentity, ok := sidecarIdentity["-shm"]; ok {
			if shmFD, ok := ownDescriptorForIdentity(shmIdentity); ok {
				shmSize, err := fdSize(shmFD)
				if err == nil {
					shmCopy, err := copyDescriptor(shmFD, filepath.Join(staging, name+"-shm"), shmSize)
					var capture *RetiredGenerationCaptureError
					if errors.As(err, &capture) {
						return err
					}
					if err == nil {
						shmCopy["identity"] = []any{shmIdentity.Dev, shmIdentity.Ino}
						manifest["shm"] = shmCopy
					}
				}
			}
		}
		header, ok := preadDBRange(dbPath, 0, sqliteHeaderBytes)
		if !ok {
			return &RetiredGenerationCaptureError{Msg: fmt.Sprintf("cannot read the main image header of %s", dbPath)}
		}
		info, err := os.Stat(dbPath)
		if err != nil {
			return err
		}
		mainSize := info.Size()
		var mainIdentity any
		if id, ok := StatDBFileIdentity(dbPath); ok {
			mainIdentity = []any{id.Dev, id.Ino}
		}
		main := map[string]any{"identity": mainIdentity, "size": mainSize, "header": parseSQLiteHeader(header)}
		if mainSize <= RetiredGenerationMainImageMaxBytes {
			copied, err := copyMainImage(dbPath, filepath.Join(staging, name), mainSize)
			if err != nil {
				return err
			}
			main["mode"] = "copied"
			for k, v := range copied {
				main[k] = v
			}
		} else {
			headerFile := filepath.Join(staging, name+".header")
			if err := os.WriteFile(headerFile, header, 0o666); err != nil {
				return err
			}
			if err := fsyncPath(headerFile); err != nil {
				return err
			}
			main["mode"], main["file"], main["bytes"] = "header_only", filepath.Base(headerFile), len(header)
		}
		manifest["main"] = main
		if err := config.AtomicJSONWrite(ctx, filepath.Join(staging, RetiredGenerationManifest), manifest); err != nil {
			return err
		}
		if err := fsyncPath(staging); err != nil {
			return err
		}
		if err := os.Rename(staging, final); err != nil {
			return err
		}
		return fsyncPath(filepath.Dir(final))
	}()
	var capture *RetiredGenerationCaptureError
	switch {
	case err == nil:
		return final, nil
	case errors.As(err, &capture):
		os.RemoveAll(staging)
		return "", err
	case isOSError(err):
		os.RemoveAll(staging)
		return "", &RetiredGenerationCaptureError{Msg: fmt.Sprintf("could not write the retired WAL generation of %s under %s: %s", dbPath, staging, pypath.OSErrorText(err))}
	}
	return "", err
}

// sqlite3.connect of the session store module, patchable so tests keep control of every tracked open
var stateSQLiteConnect SQLiteOpener = ConnectSQLite

// Opens a connection registered for the life of its descriptor through the session store connector
var connectTrackedDB = func(path string, opts ...TrackedOption) (*TrackedConn, error) {
	return ConnectTracked(path, append([]TrackedOption{WithConnectFn(stateSQLiteConnect)}, opts...)...)
}

// First probe bytes of a regular file read without a SQLite connection, false when the probe must not run
func preopenHeader(path string, probeBytes int, force bool) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	if !force && HasLiveConnection(path) {
		return nil, false
	}
	return ReadHeaderBytesPreopen(path, max(16, probeBytes), force)
}

// Reports the zeroed state.db signature, an empty file or a header of only NUL bytes
func IsZeroedStateDB(path string, probeBytes int, force bool) bool {
	head, ok := preopenHeader(path, probeBytes, force)
	if !ok || strings.HasPrefix(string(head), "SQLite format 3") {
		return false
	}
	for _, b := range head {
		if b != 0 {
			return false
		}
	}
	return true
}

// Reports a pre existing state.db whose first page is not SQLite
func HasInvalidSQLiteHeaderPreopen(path string, probeBytes int, force bool) bool {
	head, ok := preopenHeader(path, probeBytes, force)
	return ok && !strings.HasPrefix(string(head), "SQLite format 3")
}

// Pause between quarantine lock attempts
var quarantineLockSleep = time.Sleep

// Takes the cross process quarantine lock beside path within timeout, the release unlocking and closing it
func QuarantineCrossProcessLock(path string, timeout float64) (bool, func(), error) {
	lockPath := filepath.Join(filepath.Dir(path), filepath.Base(path)+".quarantine.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return false, nil, err
	}
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return false, nil, err
	}
	acquired := false
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for !acquired {
		if quarantineTryLock(handle) == nil {
			acquired = true
		} else {
			if !time.Now().Before(deadline) {
				break
			}
			quarantineLockSleep(20 * time.Millisecond)
		}
	}
	release := func() {
		if acquired {
			quarantineUnlock(handle)
		}
		handle.Close()
	}
	return acquired, release, nil
}

// Moves a non SQLite state.db aside with its sidecars and returns the quarantine path, empty when nothing moved
func QuarantineInvalidStateDB(path string, alreadyLocked bool) (string, error) {
	if alreadyLocked {
		return doQuarantine(path), nil
	}
	acquired, release, err := QuarantineCrossProcessLock(path, 5.0)
	if err != nil {
		return "", err
	}
	defer release()
	if !acquired {
		slog.Error(fmt.Sprintf("quarantine lock for %s not acquired within 5s — refusing to "+
			"quarantine without the cross-process lock. The invalid file "+
			"is left in place. If sessions fail to load, run `icebath sessions recover "+
			"--source <state.db> --inspect-only`, or restore a snapshot with "+
			"`/snapshot list` / `/snapshot restore <id>` (terminal `icebath` chat only).", path))
		return "", nil
	}
	return doQuarantine(path), nil
}

// Renames the invalid file and its sidecars under the lock, rechecking that it is still there and still invalid
func doQuarantine(path string) string {
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if !exists(path) {
		slog.Info(fmt.Sprintf("quarantine_invalid_state_db: %s already moved by another process", path))
		return ""
	}
	if !HasInvalidSQLiteHeaderPreopen(path, 100, false) {
		slog.Info(fmt.Sprintf("quarantine_invalid_state_db: %s is no longer invalid (another "+
			"process quarantined it and a fresh DB was created)", path))
		return ""
	}
	kind := "notadb"
	if IsZeroedStateDB(path, 100, false) {
		kind = "zeroed"
	}
	stem := fmt.Sprintf("%s.%s-%s-%d", filepath.Base(path), kind, time.Now().Format("20060102-150405"), os.Getpid())
	dir := filepath.Dir(path)
	dest := filepath.Join(dir, stem+".bak")
	for n := 1; exists(dest); n++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s-%d.bak", stem, n))
	}
	if err := os.Rename(path, dest); err != nil {
		slog.Error(fmt.Sprintf("Failed to quarantine invalid %s: %s", path, pypath.OSErrorText(err)))
		return ""
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if side := path + suffix; exists(side) {
			os.Rename(side, dest+suffix)
		}
	}
	return dest
}

// Python int of a SQLite value, an error when int raises
func sqliteValueInt(v any) (any, error) {
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	n, err := pyint.Of(v)
	if err != nil {
		return nil, err
	}
	return pyint.Value(n), nil
}

// Strictly read only stats snapshot of a state.db file, every field collected independently and nil when it fails
func CollectStateDBStats(dbPath string) (*pyjson.Object, error) {
	stats := pyjson.NewObject()
	for _, key := range []string{"page_count", "page_size", "freelist_count", "logical_size_bytes", "wal_size_bytes", "journal_mode",
		"messages", "sessions", "fts_tables", "fts_storage_version", "fts_storage_upgrade_needed",
		"fts_teardown_pending", "fts_rebuild_pending",
		"fts_rebuild_high_water", "fts_rebuild_progress", "fts_rebuild_deferral"} {
		stats.Set(key, nil)
	}
	walPath := dbPath + "-wal"
	if _, err := os.Stat(walPath); err == nil {
		if info, err := os.Stat(walPath); err == nil {
			stats.Set("wal_size_bytes", info.Size())
		}
	} else {
		stats.Set("wal_size_bytes", int64(0))
	}
	uri, err := ReadOnlyDBURI(dbPath)
	var conn *TrackedConn
	if err == nil {
		conn, err = connectTrackedDB(uri, WithTrackingPath(dbPath), WithURI(), WithTimeout(2.0))
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("collect_state_db_stats: cannot open %s read-only: %s", dbPath, errText(err)))
		return stats, nil
	}
	defer conn.Close()
	ctx := context.Background()
	scalar := func(query string, args ...any) any {
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		if !rows.Next() {
			return nil
		}
		var v any
		if rows.Scan(&v) != nil {
			return nil
		}
		return v
	}
	intOf := func(query string, args ...any) (any, error) {
		v := scalar(query, args...)
		if v == nil {
			return nil, nil
		}
		return sqliteValueInt(v)
	}
	metaInt := func(key string) any {
		v, err := intOf("SELECT value FROM state_meta WHERE key = ?", key)
		if err != nil {
			return nil
		}
		return v
	}
	setInt := func(key, query string) error {
		v, err := intOf(query)
		if err == nil {
			stats.Set(key, v)
		}
		return err
	}
	if err := setInt("page_count", "PRAGMA page_count"); err != nil {
		return nil, err
	}
	if err := setInt("page_size", "PRAGMA page_size"); err != nil {
		return nil, err
	}
	if pc, ok := stats.Vals["page_count"].(int64); ok {
		if ps, ok := stats.Vals["page_size"].(int64); ok {
			stats.Set("logical_size_bytes", pc*ps)
		}
	}
	if err := setInt("freelist_count", "PRAGMA freelist_count"); err != nil {
		return nil, err
	}
	if jm := scalar("PRAGMA journal_mode"); jm != nil {
		stats.Set("journal_mode", pyrepr.Str(jm))
	}
	if err := setInt("messages", "SELECT COUNT(*) FROM messages"); err != nil {
		return nil, err
	}
	if err := setInt("sessions", "SELECT COUNT(*) FROM sessions"); err != nil {
		return nil, err
	}
	if rows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name IN (?, ?, ?)",
		ftsTableNames[0], ftsTableNames[1], ftsTableNames[2]); err == nil {
		names := map[string]bool{}
		ok := true
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil {
				ok = false
				break
			}
			names[name] = true
		}
		ok = ok && rows.Err() == nil
		rows.Close()
		if ok {
			tables := pyjson.NewObject()
			for _, t := range ftsTableNames {
				tables.Set(t, names[t])
			}
			stats.Set("fts_tables", tables)
		}
	}
	stats.Set("fts_storage_version", metaInt("fts_storage_version"))
	if drift, err := FTSLayoutDrift(ctx, conn); err == nil {
		stats.Set("fts_storage_upgrade_needed", len(drift) > 0)
	}
	if row, err := fetchOne(ctx, conn, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name LIKE 'fts\_v22\_trash\_%' ESCAPE '\' LIMIT 1`); err == nil {
		stats.Set("fts_teardown_pending", row != nil)
	}
	highWater := metaInt("fts_rebuild_high_water")
	stats.Set("fts_rebuild_high_water", highWater)
	progress := metaInt("fts_rebuild_progress")
	stats.Set("fts_rebuild_progress", progress)
	pending := false
	if highWater != nil {
		hw, herr := pyint.Of(highWater)
		pr := big.NewInt(0)
		if progress != nil {
			pr, _ = pyint.Of(progress)
		}
		pending = herr == nil && pr.Cmp(hw) < 0
	}
	stats.Set("fts_rebuild_pending", pending)
	if row, err := fetchOne(ctx, conn, "SELECT value FROM state_meta WHERE key = ? LIMIT 1", FTSRebuildDeferralKey); err == nil && row != nil {
		var parsed any
		var perr error
		switch v := row[0].(type) {
		case string:
			parsed, perr = pyjson.Loads(v)
		case []byte:
			parsed, perr = pyjson.LoadsBytes(v)
		default:
			perr = &pyConvError{"TypeError", "the JSON object must be str, bytes or bytearray"}
		}
		if obj, ok := parsed.(*pyjson.Object); ok && perr == nil {
			stats.Set("fts_rebuild_deferral", obj)
		}
	}
	return stats, nil
}

// Distinct pids holding the database open, a lower bound, false on any error or on hosts without a scan
func CountDBHolders(dbPath string) (int, bool) {
	target := realpathLax(dbPath)
	pids := map[int]bool{}
	switch {
	case dbfilePlatform == "darwin":
		identity, err := statIdentityErr(target)
		if err != nil {
			return 0, false
		}
		if err := iterDarwinFDTargets(func(pid, _ int, _ string, ident FileIdentity) {
			if ident == identity {
				pids[pid] = true
			}
		}); err != nil {
			return 0, false
		}
	case dbfilePlatform == "linux":
		if err := iterProcFDTargets(func(pid int, link, _ string) {
			if link == target {
				pids[pid] = true
			}
		}); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return len(pids), true
}

// Safety predicate for the narrow Desktop holder reap
func isInactiveOrphanDesktopHolder(ppid int, ageSeconds, minAgeSeconds float64, ephemeralBackend bool, connectionStatuses []string) bool {
	return (ppid == 0 || ppid == 1) && ageSeconds >= minAgeSeconds && ephemeralBackend &&
		!slices.Contains(connectionStatuses, "ESTABLISHED")
}

// Unique pids proven to hold the database or one of its sidecars, in first seen order
func concreteStateDBHolderPIDs(dbPath string, holders []Holder) ([]int, error) {
	abs, err := pyAbspath(dbPath)
	if err != nil {
		return nil, err
	}
	abs = normcase(abs)
	watched := map[string]bool{abs: true, abs + "-wal": true, abs + "-shm": true}
	pids := []int{}
	for _, h := range holders {
		if h.PID <= 0 {
			continue
		}
		canonical, err := CanonicalSQLitePath(h.Target)
		if err != nil {
			return nil, err
		}
		if watched[canonical] && !slices.Contains(pids, h.PID) {
			pids = append(pids, h.PID)
		}
	}
	return pids, nil
}
