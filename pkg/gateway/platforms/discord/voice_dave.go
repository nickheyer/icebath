package discord

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

// libdave media type, codec and result constants of dave.h
const (
	daveMediaAudio   = 0
	daveCodecOpus    = 1
	daveResultOK     = 0
	daveInitTransID  = 0
	daveTransitionTO = 10 * time.Second
)

// Release page the libdave shared library is installed from
const libdaveReleasesURL = "https://github.com/discord/libdave/releases"

// File name of the libdave shared library on this platform
func libdaveFileName() string {
	switch runtime.GOOS {
	case "windows":
		return "libdave.dll"
	case "darwin", "ios":
		return "libdave.dylib"
	}
	return "libdave.so"
}

// libdave C API entry points
type daveAPI struct {
	maxProtocolVersion   func() uint16
	free                 func(p unsafe.Pointer)
	sessionCreate        func(context, authSessionID, callback, userData unsafe.Pointer) uintptr
	sessionDestroy       func(s uintptr)
	sessionInit          func(s uintptr, version uint16, groupID uint64, selfUserID unsafe.Pointer)
	sessionReset         func(s uintptr)
	sessionSetExtSender  func(s uintptr, data unsafe.Pointer, length uintptr)
	sessionProposals     func(s uintptr, data unsafe.Pointer, length uintptr, ids unsafe.Pointer, idsLen uintptr, out, outLen unsafe.Pointer)
	sessionCommit        func(s uintptr, data unsafe.Pointer, length uintptr) uintptr
	sessionWelcome       func(s uintptr, data unsafe.Pointer, length uintptr, ids unsafe.Pointer, idsLen uintptr) uintptr
	sessionKeyPackage    func(s uintptr, out, outLen unsafe.Pointer)
	sessionKeyRatchet    func(s uintptr, userID unsafe.Pointer) uintptr
	keyRatchetDestroy    func(k uintptr)
	commitIsFailed       func(c uintptr) bool
	commitIsIgnored      func(c uintptr) bool
	commitDestroy        func(c uintptr)
	welcomeDestroy       func(w uintptr)
	encryptorCreate      func() uintptr
	encryptorDestroy     func(e uintptr)
	encryptorSetRatchet  func(e, k uintptr)
	encryptorPassthrough func(e uintptr, on bool)
	encryptorAssignSSRC  func(e uintptr, ssrc uint32, codec int32)
	encryptorMaxSize     func(e uintptr, media int32, frame uintptr) uintptr
	encryptorEncrypt     func(e uintptr, media int32, ssrc uint32, frame unsafe.Pointer, frameLen uintptr, out unsafe.Pointer, capacity uintptr, written unsafe.Pointer) int32
	decryptorCreate      func() uintptr
	decryptorDestroy     func(d uintptr)
	decryptorRatchet     func(d, k uintptr)
	decryptorPassthrough func(d uintptr, on bool)
	decryptorMaxSize     func(d uintptr, media int32, frame uintptr) uintptr
	decryptorDecrypt     func(d uintptr, media int32, frame unsafe.Pointer, frameLen uintptr, out unsafe.Pointer, capacity uintptr, written unsafe.Pointer) int32
}

var (
	daveLoadMu sync.Mutex
	daveLoaded *daveAPI
)

// Error a voice join returns when libdave cannot be loaded, naming the library and where it is installed from
type daveMissingError struct{ detail string }

func (e *daveMissingError) Error() string {
	return fmt.Sprintf("libdave library needed in order to use voice: install %s from %s on the system library path (%s)",
		libdaveFileName(), libdaveReleasesURL, e.detail)
}

// Command installing libdave where this platform's loader finds it, shown by the voice dependency message
func (e *daveMissingError) VoiceDependencyHint() string { return libdaveInstallHint() }

// libdave release whose C API this client binds
const libdaveRelease = "v1.2.2/cpp"

// Shell command downloading this platform's libdave release asset onto the library search path
func libdaveInstallHint() string {
	base := "https://github.com/discord/libdave/releases/download/" + libdaveRelease + "/libdave-"
	arch := map[string]string{"amd64": "X64", "arm64": "ARM64"}[runtime.GOARCH]
	switch {
	case runtime.GOOS == "linux" && arch != "":
		return "curl -fsSLo /tmp/libdave.zip " + base + "Linux-" + arch + "-boringssl.zip && sudo unzip -oj /tmp/libdave.zip lib/libdave.so -d /usr/lib && sudo ldconfig"
	case runtime.GOOS == "darwin" && arch != "":
		return "mkdir -p ~/lib && curl -fsSLo /tmp/libdave.zip " + base + "macOS-" + arch + "-boringssl.zip && unzip -oj /tmp/libdave.zip lib/libdave.dylib -d ~/lib"
	case runtime.GOOS == "windows" && arch == "X64":
		return "Invoke-WebRequest " + base + "Windows-X64-boringssl.zip -OutFile $env:TEMP\\libdave.zip; Expand-Archive -Force $env:TEMP\\libdave.zip $env:TEMP\\libdave; " +
			"Copy-Item $env:TEMP\\libdave\\bin\\libdave.dll (Split-Path (Get-Command icebath).Source)"
	}
	return "git clone --recursive -b " + libdaveRelease + " https://github.com/discord/libdave && cd libdave/cpp && ./vcpkg/bootstrap-vcpkg.sh && " +
		"make shared && make install BUILD_SHARED_LIBS=ON && sudo cp build/install/lib/libdave.* /usr/local/lib && sudo ldconfig"
}

// Loads libdave once from the platform library search, binding every function the voice client calls
func loadDave() (*daveAPI, error) {
	daveLoadMu.Lock()
	defer daveLoadMu.Unlock()
	if daveLoaded != nil {
		return daveLoaded, nil
	}
	if !pyctypes.Supported {
		return nil, &daveMissingError{detail: "shared library loading is unavailable on this platform"}
	}
	path := pyctypes.FindLibrary("dave")
	if path == "" && runtime.GOOS == "windows" {
		path = pyctypes.FindLibrary("libdave")
	}
	if path == "" {
		return nil, &daveMissingError{detail: "not found"}
	}
	lib, err := pyctypes.LoadLibrary(path, false)
	if err != nil {
		return nil, &daveMissingError{detail: err.Error()}
	}
	d := &daveAPI{}
	for name, fn := range map[string]any{
		"daveMaxSupportedProtocolVersion": &d.maxProtocolVersion, "daveFree": &d.free,
		"daveSessionCreate": &d.sessionCreate, "daveSessionDestroy": &d.sessionDestroy, "daveSessionInit": &d.sessionInit,
		"daveSessionReset":             &d.sessionReset,
		"daveSessionSetExternalSender": &d.sessionSetExtSender, "daveSessionProcessProposals": &d.sessionProposals,
		"daveSessionProcessCommit": &d.sessionCommit, "daveSessionProcessWelcome": &d.sessionWelcome,
		"daveSessionGetMarshalledKeyPackage": &d.sessionKeyPackage, "daveSessionGetKeyRatchet": &d.sessionKeyRatchet,
		"daveKeyRatchetDestroy": &d.keyRatchetDestroy, "daveCommitResultIsFailed": &d.commitIsFailed,
		"daveCommitResultIsIgnored": &d.commitIsIgnored, "daveCommitResultDestroy": &d.commitDestroy,
		"daveWelcomeResultDestroy": &d.welcomeDestroy, "daveEncryptorCreate": &d.encryptorCreate,
		"daveEncryptorDestroy": &d.encryptorDestroy, "daveEncryptorSetKeyRatchet": &d.encryptorSetRatchet,
		"daveEncryptorSetPassthroughMode": &d.encryptorPassthrough, "daveEncryptorAssignSsrcToCodec": &d.encryptorAssignSSRC,
		"daveEncryptorGetMaxCiphertextByteSize": &d.encryptorMaxSize, "daveEncryptorEncrypt": &d.encryptorEncrypt,
		"daveDecryptorCreate": &d.decryptorCreate, "daveDecryptorDestroy": &d.decryptorDestroy,
		"daveDecryptorTransitionToKeyRatchet": &d.decryptorRatchet, "daveDecryptorTransitionToPassthroughMode": &d.decryptorPassthrough,
		"daveDecryptorGetMaxPlaintextByteSize": &d.decryptorMaxSize, "daveDecryptorDecrypt": &d.decryptorDecrypt,
	} {
		addr, err := lib.Symbol(name)
		if err != nil {
			return nil, &daveMissingError{detail: err.Error()}
		}
		pyctypes.RegisterFunc(fn, addr)
	}
	daveLoaded = d
	return d, nil
}

// Highest DAVE protocol version the loaded libdave speaks, zero without libdave
func daveMaxProtocolVersion() int {
	d, err := loadDave()
	if err != nil {
		return 0
	}
	return int(d.maxProtocolVersion())
}

// Copies and frees a buffer libdave allocated
func (d *daveAPI) takeBuffer(p unsafe.Pointer, n uintptr) []byte {
	if p == nil {
		return nil
	}
	out := append([]byte(nil), unsafe.Slice((*byte)(p), n)...)
	d.free(p)
	return out
}

// Frame decryption of one remote user, the DAVE session surface the receiver reads
type voiceDaveSession interface {
	decrypt(userID int64, mediaType int, frame []byte) ([]byte, error)
}

// DAVE session of one voice connection holding the MLS group, the encryptor and one decryptor per user
type daveSession struct {
	api                   *daveAPI
	mu                    sync.Mutex
	handle                uintptr
	selfUserID            string
	groupID               uint64
	ready                 bool
	recognized            map[string]bool
	encryptor             uintptr
	encryptorSSRCs        map[uint32]bool
	decryptors            map[string]uintptr
	selfTransitions       map[int]int
	latestPreparedVersion int
	passthroughGeneration int
}

// Session initialised for a protocol version, the bot user and the voice channel as MLS group
func newDaveSession(api *daveAPI, protocolVersion int, selfUserID, channelID int64) *daveSession {
	s := &daveSession{api: api, handle: api.sessionCreate(nil, nil, nil, nil), recognized: map[string]bool{},
		encryptor: api.encryptorCreate(), encryptorSSRCs: map[uint32]bool{}, decryptors: map[string]uintptr{}, selfTransitions: map[int]int{}}
	runtime.SetFinalizer(s, func(s *daveSession) { s.destroy() })
	s.reinit(protocolVersion, selfUserID, channelID)
	return s
}

// Releases the native session, encryptor and decryptors
func (s *daveSession) destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.decryptors {
		s.api.decryptorDestroy(d)
		delete(s.decryptors, id)
	}
	if s.encryptor != 0 {
		s.api.encryptorDestroy(s.encryptor)
		s.encryptor = 0
	}
	if s.handle != 0 {
		s.api.sessionDestroy(s.handle)
		s.handle = 0
	}
}

// Restarts the MLS group for a protocol version, discarding the established group
func (s *daveSession) reinit(protocolVersion int, selfUserID, channelID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfUserID, s.groupID = strconv.FormatInt(selfUserID, 10), uint64(channelID)
	self := pyctypes.CString(s.selfUserID)
	s.api.sessionInit(s.handle, uint16(protocolVersion), s.groupID, unsafe.Pointer(&self[0]))
	runtime.KeepAlive(self)
	s.ready = false
}

// Drops the MLS group after a downgrade to protocol zero
func (s *daveSession) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.api.sessionReset(s.handle)
	s.ready = false
}

// Whether an MLS group is established so frames can be encrypted
func (s *daveSession) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Allows plaintext frames from every user, for a bounded window of seconds after which encrypted frames are required again
func (s *daveSession) setPassthroughMode(on bool, seconds float64) {
	s.mu.Lock()
	s.passthroughGeneration++
	gen := s.passthroughGeneration
	for _, d := range s.decryptors {
		s.api.decryptorPassthrough(d, on)
	}
	s.mu.Unlock()
	if !on {
		return
	}
	wait := time.Duration(seconds*float64(time.Second)) - daveTransitionTO
	closeWindow := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.passthroughGeneration != gen {
			return
		}
		for _, d := range s.decryptors {
			s.api.decryptorPassthrough(d, false)
		}
	}
	if wait <= 0 {
		closeWindow()
		return
	}
	time.AfterFunc(wait, closeWindow)
}

// Recognises users the voice server announced as connected
func (s *daveSession) addRecognizedUsers(userIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range userIDs {
		s.recognized[id] = true
		s.setupUserRatchetLocked(id, s.latestPreparedVersion)
	}
}

// Forgets a user the voice server announced as disconnected
func (s *daveSession) removeRecognizedUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recognized, userID)
	if d, ok := s.decryptors[userID]; ok {
		s.api.decryptorDestroy(d)
		delete(s.decryptors, userID)
	}
}

// Recognised user ids plus the bot itself as NUL terminated strings and their pointer array
func (s *daveSession) recognizedCStrings() ([][]byte, []uintptr) {
	ids := [][]byte{pyctypes.CString(s.selfUserID)}
	for id := range s.recognized {
		if id != s.selfUserID {
			ids = append(ids, pyctypes.CString(id))
		}
	}
	ptrs := make([]uintptr, len(ids))
	for i := range ids {
		ptrs[i] = uintptr(unsafe.Pointer(&ids[i][0]))
	}
	return ids, ptrs
}

// Sets the MLS external sender package of the voice server
func (s *daveSession) setExternalSender(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.api.sessionSetExtSender(s.handle, unsafe.Pointer(&data[0]), uintptr(len(data)))
	runtime.KeepAlive(data)
}

// Commit and optional welcome answering appended or revoked proposals, nil when nothing is to be committed
func (s *daveSession) processProposals(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, ptrs := s.recognizedCStrings()
	var out unsafe.Pointer
	var outLen uintptr
	s.api.sessionProposals(s.handle, unsafe.Pointer(&payload[0]), uintptr(len(payload)), unsafe.Pointer(&ptrs[0]), uintptr(len(ptrs)),
		unsafe.Pointer(&out), unsafe.Pointer(&outLen))
	runtime.KeepAlive(ids)
	runtime.KeepAlive(payload)
	return s.api.takeBuffer(out, outLen)
}

// Applies an announced commit, failing when libdave rejects it
func (s *daveSession) processCommit(commit []byte) error {
	if len(commit) == 0 {
		return errors.New("empty MLS commit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.api.sessionCommit(s.handle, unsafe.Pointer(&commit[0]), uintptr(len(commit)))
	runtime.KeepAlive(commit)
	if res == 0 {
		return errors.New("MLS commit could not be processed")
	}
	defer s.api.commitDestroy(res)
	if s.api.commitIsFailed(res) {
		return errors.New("MLS commit failed")
	}
	if s.api.commitIsIgnored(res) {
		return nil
	}
	s.ready = true
	return nil
}

// Joins the group from a welcome, failing when libdave rejects it
func (s *daveSession) processWelcome(welcome []byte) error {
	if len(welcome) == 0 {
		return errors.New("empty MLS welcome")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, ptrs := s.recognizedCStrings()
	res := s.api.sessionWelcome(s.handle, unsafe.Pointer(&welcome[0]), uintptr(len(welcome)), unsafe.Pointer(&ptrs[0]), uintptr(len(ptrs)))
	runtime.KeepAlive(ids)
	runtime.KeepAlive(welcome)
	if res == 0 {
		return errors.New("MLS welcome could not be processed")
	}
	s.api.welcomeDestroy(res)
	s.ready = true
	return nil
}

// Serialized MLS key package announcing the bot to the group
func (s *daveSession) keyPackage() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out unsafe.Pointer
	var outLen uintptr
	s.api.sessionKeyPackage(s.handle, unsafe.Pointer(&out), unsafe.Pointer(&outLen))
	return s.api.takeBuffer(out, outLen)
}

// Moves one user's decryptor to the ratchet of a protocol version, or to passthrough for version zero
func (s *daveSession) setupUserRatchetLocked(userID string, protocolVersion int) {
	d, ok := s.decryptors[userID]
	if !ok {
		d = s.api.decryptorCreate()
		s.decryptors[userID] = d
	}
	if protocolVersion == 0 {
		s.api.decryptorPassthrough(d, true)
		return
	}
	uid := pyctypes.CString(userID)
	k := s.api.sessionKeyRatchet(s.handle, unsafe.Pointer(&uid[0]))
	runtime.KeepAlive(uid)
	if k == 0 {
		return
	}
	s.api.decryptorRatchet(d, k)
	s.api.keyRatchetDestroy(k)
	s.api.decryptorPassthrough(d, false)
}

// Sets the bot's encryptor to the ratchet of a protocol version, passthrough for version zero
func (s *daveSession) setupSelfRatchetLocked(protocolVersion int) {
	if protocolVersion == 0 {
		s.api.encryptorPassthrough(s.encryptor, true)
		return
	}
	self := pyctypes.CString(s.selfUserID)
	k := s.api.sessionKeyRatchet(s.handle, unsafe.Pointer(&self[0]))
	runtime.KeepAlive(self)
	if k == 0 {
		return
	}
	s.api.encryptorSetRatchet(s.encryptor, k)
	s.api.keyRatchetDestroy(k)
	s.api.encryptorPassthrough(s.encryptor, false)
}

// Prepares every recognised user's decryptor for a transition and stages or applies the bot's own ratchet
func (s *daveSession) prepareTransition(transitionID, protocolVersion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.recognized {
		if id != s.selfUserID {
			s.setupUserRatchetLocked(id, protocolVersion)
		}
	}
	if transitionID == daveInitTransID {
		s.setupSelfRatchetLocked(protocolVersion)
	} else {
		s.selfTransitions[transitionID] = protocolVersion
	}
	s.latestPreparedVersion = protocolVersion
}

// Switches the bot's encryptor to the ratchet staged for an executed transition
func (s *daveSession) executeTransition(transitionID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.selfTransitions[transitionID]
	if !ok {
		return
	}
	delete(s.selfTransitions, transitionID)
	if version == 0 {
		s.api.sessionReset(s.handle)
		s.ready = false
	}
	s.setupSelfRatchetLocked(version)
}

// End to end encrypted form of one outgoing Opus frame
func (s *daveSession) encryptOpus(ssrc uint32, frame []byte) ([]byte, error) {
	if len(frame) == 0 {
		return frame, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.encryptorSSRCs[ssrc] {
		s.api.encryptorAssignSSRC(s.encryptor, ssrc, daveCodecOpus)
		s.encryptorSSRCs[ssrc] = true
	}
	out := make([]byte, s.api.encryptorMaxSize(s.encryptor, daveMediaAudio, uintptr(len(frame))))
	var written uintptr
	code := s.api.encryptorEncrypt(s.encryptor, daveMediaAudio, ssrc, unsafe.Pointer(&frame[0]), uintptr(len(frame)),
		unsafe.Pointer(&out[0]), uintptr(len(out)), unsafe.Pointer(&written))
	runtime.KeepAlive(frame)
	if code != daveResultOK {
		return nil, fmt.Errorf("DAVE encryption failed: %s", daveEncryptorResult(code))
	}
	return out[:written], nil
}

// Name of a libdave encryptor result code
func daveEncryptorResult(code int32) string {
	switch code {
	case 1:
		return "EncryptionFailure"
	case 2:
		return "MissingKeyRatchet"
	case 3:
		return "MissingCryptor"
	case 4:
		return "TooManyAttempts"
	}
	return strconv.Itoa(int(code))
}

// Whether a frame carries the DAVE supplemental marker
func daveFrameEncrypted(frame []byte) bool {
	n := len(frame)
	return n >= 2 && frame[n-1] == 0xfa && frame[n-2] == 0xfa
}

// Plaintext of one user's frame, an Unencrypted error when a plain frame arrives outside passthrough
func (s *daveSession) decrypt(userID int64, mediaType int, frame []byte) ([]byte, error) {
	if len(frame) == 0 {
		return frame, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.decryptors[strconv.FormatInt(userID, 10)]
	if !ok {
		return nil, fmt.Errorf("DecryptionFailed(NoDecryptorForUser): no decryptor for user %d", userID)
	}
	out := make([]byte, max(s.api.decryptorMaxSize(d, int32(mediaType), uintptr(len(frame))), uintptr(len(frame))))
	var written uintptr
	code := s.api.decryptorDecrypt(d, int32(mediaType), unsafe.Pointer(&frame[0]), uintptr(len(frame)), unsafe.Pointer(&out[0]),
		uintptr(len(out)), unsafe.Pointer(&written))
	runtime.KeepAlive(frame)
	switch {
	case code == daveResultOK:
		return out[:written], nil
	case !daveFrameEncrypted(frame):
		return nil, errors.New("DecryptionFailed(UnencryptedWhenPassthroughDisabled)")
	}
	return nil, fmt.Errorf("DecryptionFailed(%s)", daveDecryptorResult(code))
}

// Name of a libdave decryptor result code
func daveDecryptorResult(code int32) string {
	switch code {
	case 1:
		return "DecryptionFailure"
	case 2:
		return "MissingKeyRatchet"
	case 3:
		return "InvalidNonce"
	case 4:
		return "MissingCryptor"
	}
	return strconv.Itoa(int(code))
}
