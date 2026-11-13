//go:build goolm

package matrix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	_ "modernc.org/sqlite"
)

// Empty in this build, the pure Go olm implementation being compiled in
const e2eeInstallHint = ""

// True in this build, the goolm tag compiling the pure Go olm implementation in
const E2EEBuiltIn = true

// Whether this binary carries an olm implementation, true with the goolm build tag
func checkE2EEDeps() bool { return E2EEBuiltIn }

// Cause named when encrypted events drop while E2EE is off
func encryptedDropOffCause() string {
	return "Set MATRIX_E2EE_MODE=optional (or required)"
}

// Olm machine wired to the client as its crypto helper, sharing group sessions on demand
type olmCrypto struct {
	mach   *crypto.OlmMachine
	client *mautrix.Client
	syncer *mautrix.DefaultSyncer

	shareMu  sync.Mutex
	inFlight map[id.RoomID]chan struct{}
}

// Uploads device keys and one time keys
func (c *olmCrypto) shareKeys(ctx context.Context) error { return c.mach.ShareKeys(ctx, -1) }

// Megolm encryption, sharing the group session with joined and invited members once on a session error
func (c *olmCrypto) Encrypt(ctx context.Context, roomID id.RoomID, evtType event.Type, content any) (*event.EncryptedEventContent, error) {
	encrypted, err := c.mach.EncryptMegolmEvent(ctx, roomID, evtType, content)
	if err == nil {
		return encrypted, nil
	}
	if !errors.Is(err, crypto.ErrSessionExpired) && !errors.Is(err, crypto.ErrNoGroupSession) && !errors.Is(err, crypto.ErrSessionNotShared) {
		return nil, err
	}
	if err := c.shareGroupSession(ctx, roomID); err != nil {
		return nil, err
	}
	return c.mach.EncryptMegolmEvent(ctx, roomID, evtType, content)
}

// Shares the room group session, concurrent callers waiting on the share already in progress
func (c *olmCrypto) shareGroupSession(ctx context.Context, roomID id.RoomID) error {
	c.shareMu.Lock()
	if wait, ok := c.inFlight[roomID]; ok {
		c.shareMu.Unlock()
		select {
		case <-wait:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	c.inFlight[roomID] = done
	c.shareMu.Unlock()
	defer func() {
		c.shareMu.Lock()
		delete(c.inFlight, roomID)
		c.shareMu.Unlock()
		close(done)
	}()
	users, err := c.client.StateStore.GetRoomJoinedOrInvitedMembers(ctx, roomID)
	if err != nil {
		return err
	}
	return c.mach.ShareGroupSession(ctx, roomID, users)
}

// Megolm decryption of a room event
func (c *olmCrypto) Decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	return c.mach.DecryptMegolmEvent(ctx, evt)
}

// Waits for a megolm session to arrive
func (c *olmCrypto) WaitForSession(ctx context.Context, roomID id.RoomID, senderKey id.SenderKey, sessionID id.SessionID, timeout time.Duration) bool {
	return c.mach.WaitForSession(ctx, roomID, senderKey, sessionID, timeout)
}

// Requests a missing megolm session from the sender device and the bot's own devices
func (c *olmCrypto) RequestSession(ctx context.Context, roomID id.RoomID, senderKey id.SenderKey, sessionID id.SessionID, userID id.UserID, deviceID id.DeviceID) {
	if deviceID == "" {
		deviceID = "*"
	}
	if err := c.mach.SendRoomKeyRequest(ctx, roomID, senderKey, sessionID, "", map[id.UserID][]id.DeviceID{
		userID: {deviceID}, c.client.UserID: {"*"}}); err != nil {
		slog.Warn(fmt.Sprintf("Matrix: room key request failed: %v", err))
	}
}

// Loaded at connect, nothing left to initialise
func (c *olmCrypto) Init(context.Context) error { return nil }

// Decrypts an encrypted room event and dispatches the plaintext, logging and dropping it when decryption fails
func (c *olmCrypto) handleEncrypted(ctx context.Context, evt *event.Event) {
	decrypted, err := c.mach.DecryptMegolmEvent(ctx, evt)
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: failed to decrypt %s: %v", evt.ID, err))
		return
	}
	decrypted.Mautrix.EventSource |= event.SourceDecrypted
	c.syncer.Dispatch(ctx, decrypted)
}

// Store that reports the m.room.encryption content it holds for a room
type encryptionEventGetter interface {
	GetEncryptionEvent(ctx context.Context, roomID id.RoomID) (*event.EncryptionEventContent, error)
}

// Client state store whose encryption check asks the homeserver about rooms it has no encryption state for
type sendStateStore struct {
	mautrix.StateStore
	client *mautrix.Client
}

// Whether the room is encrypted, a missing m.room.encryption state meaning unencrypted
func (s *sendStateStore) IsEncrypted(ctx context.Context, roomID id.RoomID) (bool, error) {
	getter, _ := s.StateStore.(encryptionEventGetter)
	var evt *event.EncryptionEventContent
	if getter != nil {
		var err error
		if evt, err = getter.GetEncryptionEvent(ctx, roomID); err != nil {
			return false, err
		}
	}
	if evt != nil {
		return evt.Algorithm != "", nil
	}
	var content event.EncryptionEventContent
	if err := s.client.StateEvent(ctx, roomID, event.StateEncryption, "", &content); err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return false, nil
		}
		return false, err
	}
	return content.Algorithm != "", nil
}

// State store handed to the olm machine, falling back to a homeserver state query when the client store knows nothing
type cryptoStateStore struct {
	ss          mautrix.StateStore
	joinedRooms func() []id.RoomID
	client      *mautrix.Client
	mu          sync.Mutex
	encInfo     map[id.RoomID]*event.EncryptionEventContent
}

// Whether the room has encryption info
func (s *cryptoStateStore) IsEncrypted(ctx context.Context, roomID id.RoomID) (bool, error) {
	info, err := s.GetEncryptionEvent(ctx, roomID)
	return info != nil, err
}

// Encryption info from the client store, then the cache, then the homeserver, nil when unknown
func (s *cryptoStateStore) GetEncryptionEvent(ctx context.Context, roomID id.RoomID) (*event.EncryptionEventContent, error) {
	if getter, ok := s.ss.(encryptionEventGetter); ok {
		if info, err := getter.GetEncryptionEvent(ctx, roomID); err == nil && info != nil {
			return info, nil
		}
	}
	s.mu.Lock()
	cached, ok := s.encInfo[roomID]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	if s.client == nil {
		return nil, nil
	}
	var content event.EncryptionEventContent
	if err := s.client.StateEvent(ctx, roomID, event.StateEncryption, "", &content); err != nil {
		slog.Debug(fmt.Sprintf("Matrix: homeserver encryption-info query failed for %s: %v", roomID, err))
		return nil, nil
	}
	if content.Algorithm == "" {
		return nil, nil
	}
	s.mu.Lock()
	s.encInfo[roomID] = &content
	s.mu.Unlock()
	return &content, nil
}

// History visibility from the client store
func (s *cryptoStateStore) GetHistoryVisibility(ctx context.Context, roomID id.RoomID) (*event.HistoryVisibilityEventContent, error) {
	return s.ss.GetHistoryVisibility(ctx, roomID)
}

// Every joined room, correct for a single user bot
func (s *cryptoStateStore) FindSharedRooms(ctx context.Context, userID id.UserID) ([]id.RoomID, error) {
	return s.joinedRooms(), nil
}

// Joined room ids of the adapter
func (a *MatrixAdapter) joinedRoomIDs() []id.RoomID {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]id.RoomID, 0, len(a.joinedRooms))
	for r := range a.joinedRooms {
		out = append(out, id.RoomID(r))
	}
	return out
}

// Ed25519 key a device advertises on the server, empty when it has none
func extractServerEd25519(keys mautrix.DeviceKeys) string {
	for kid, kval := range keys.Keys {
		if strings.HasPrefix(string(kid), "ed25519:") {
			return kval
		}
	}
	return ""
}

// Server device keys entry of the bot's own device, nil when absent
func queryOwnDeviceKeys(ctx context.Context, client *mautrix.Client) (*mautrix.DeviceKeys, error) {
	resp, err := client.QueryKeys(ctx, &mautrix.ReqQueryKeys{DeviceKeys: mautrix.DeviceKeysRequest{client.UserID: mautrix.DeviceIDList{client.DeviceID}}})
	if err != nil {
		return nil, err
	}
	dev, ok := resp.DeviceKeys[client.UserID][client.DeviceID]
	if !ok {
		return nil, nil
	}
	return &dev, nil
}

// Re-queries the server after a key upload and checks the ed25519 key matches the local one
func (a *MatrixAdapter) reverifyKeysAfterUpload(ctx context.Context, client *mautrix.Client, localEd25519 string) bool {
	a.mu.Lock()
	unverified := a.deviceIDUnverified
	a.mu.Unlock()
	if client.DeviceID == "" || unverified {
		slog.Warn("Matrix: skipping post-upload key verification — device_id not yet established")
		return true
	}
	dev, err := queryOwnDeviceKeys(ctx, client)
	if err != nil {
		slog.Error(fmt.Sprintf("Matrix: post-upload key verification failed: %v", err))
		return false
	}
	if dev != nil && extractServerEd25519(*dev) != localEd25519 {
		slog.Error(fmt.Sprintf("Matrix: device %s has immutable identity keys that don't match this "+
			"installation. Generate a new access token with a fresh device.", client.DeviceID))
		return false
	}
	return true
}

// Clears the stored olm account when the token's device changed, true when it reset
func (a *MatrixAdapter) resetCryptoStoreIfDeviceChanged(ctx context.Context, store *crypto.SQLCryptoStore, deviceID string) (bool, error) {
	if deviceID == "" {
		return false, nil
	}
	stored, err := store.FindDeviceID(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: could not read stored device ID: %v", err))
		return false, nil
	}
	if stored == "" || string(stored) == deviceID {
		return false, nil
	}
	slog.Warn(fmt.Sprintf("Matrix: access token belongs to a new device (%s -> %s) — resetting local Olm account "+
		"so fresh identity keys are generated for this device", stored, deviceID))
	for _, table := range []string{"crypto_account", "crypto_olm_session", "crypto_megolm_outbound_session"} {
		if _, err := store.DB.Exec(ctx, "DELETE FROM "+table+" WHERE account_id=$1", store.AccountID); err != nil {
			return false, err
		}
	}
	store.Account = nil
	return true, nil
}

// Records the resolved device id on the stored account row
func putDeviceID(ctx context.Context, store *crypto.SQLCryptoStore, deviceID string) error {
	_, err := store.DB.Exec(ctx, "UPDATE crypto_account SET device_id=$1 WHERE account_id=$2", deviceID, store.AccountID)
	store.DeviceID = id.DeviceID(deviceID)
	return err
}

// Re-pickles the olm account under the current pickle key, false only when an account exists that no key opens
func (a *MatrixAdapter) migrateLegacyCryptoPickle(ctx context.Context, store *crypto.SQLCryptoStore, db *dbutil.Database, acctID, pickleKey string) (bool, error) {
	if _, err := store.GetAccount(ctx); err == nil {
		return true, nil
	}
	for _, legacyKey := range []string{acctID + ":default", acctID} {
		if legacyKey == pickleKey {
			continue
		}
		legacy := crypto.NewSQLCryptoStore(db, dbutil.NoopLogger, acctID, store.DeviceID, []byte(legacyKey))
		account, err := legacy.GetAccount(ctx)
		if err != nil || account == nil {
			continue
		}
		if err := a.repickleCryptoSessions(ctx, store, acctID, legacyKey, pickleKey); err != nil {
			slog.Error(fmt.Sprintf("Matrix: pickle key migration failed while re-pickling sessions (%v) — leaving "+
				"the account under the legacy key so the migration is retried on the next start.", err))
			return false, nil
		}
		if err := store.PutAccount(ctx, account); err != nil {
			return false, err
		}
		slog.Info("Matrix: re-pickled crypto store account and sessions under the current pickle key " +
			"(device ID was configured after the account was created)")
		return true, nil
	}
	slog.Error("Matrix: crypto store account exists but cannot be unpickled with the current or any " +
		"legacy pickle key. If MATRIX_DEVICE_ID was changed manually, restore its previous value.")
	return false, nil
}

// Pickle round trip of one session table row kind
type sessionCodec struct {
	table    string
	unpickle func(pickled, key []byte) (interface{ Pickle([]byte) ([]byte, error) }, error)
}

var sessionCodecs = []sessionCodec{
	{"crypto_olm_session", func(p, k []byte) (interface{ Pickle([]byte) ([]byte, error) }, error) {
		return olm.SessionFromPickled(p, k)
	}},
	{"crypto_megolm_inbound_session", func(p, k []byte) (interface{ Pickle([]byte) ([]byte, error) }, error) {
		return olm.InboundGroupSessionFromPickled(p, k)
	}},
	{"crypto_megolm_outbound_session", func(p, k []byte) (interface{ Pickle([]byte) ([]byte, error) }, error) {
		return olm.OutboundGroupSessionFromPickled(p, k)
	}},
}

// Re-pickles olm and megolm sessions sharing the legacy key, leaving rows neither key opens in place
func (a *MatrixAdapter) repickleCryptoSessions(ctx context.Context, store *crypto.SQLCryptoStore, acctID, legacyKey, pickleKey string) error {
	for _, codec := range sessionCodecs {
		type row struct {
			sessionID string
			session   []byte
		}
		rows, err := store.DB.Query(ctx, "SELECT session_id, session FROM "+codec.table+" WHERE account_id=$1", acctID)
		if err != nil {
			return err
		}
		var all []row
		for rows.Next() {
			var r row
			var sessionID sql.NullString
			if err := rows.Scan(&sessionID, &r.session); err != nil {
				rows.Close()
				return err
			}
			r.sessionID = sessionID.String
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, r := range all {
			if r.session == nil {
				continue
			}
			if _, err := codec.unpickle(append([]byte(nil), r.session...), []byte(pickleKey)); err == nil {
				continue
			}
			session, err := codec.unpickle(append([]byte(nil), r.session...), []byte(legacyKey))
			if err != nil {
				slog.Warn(fmt.Sprintf("Matrix: %s row %s cannot be unpickled with the current or legacy key; leaving "+
					"it in place, its sessions are unrecoverable: %v", codec.table, r.sessionID, err))
				continue
			}
			pickled, err := session.Pickle([]byte(pickleKey))
			if err != nil {
				return err
			}
			if _, err := store.DB.Exec(ctx, "UPDATE "+codec.table+" SET session=$1 WHERE account_id=$2 AND session_id=$3",
				pickled, acctID, r.sessionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Whether the device keys are on the server or were re-uploaded, false refusing E2EE
func (a *MatrixAdapter) verifyDeviceKeysOnServer(ctx context.Context, client *mautrix.Client, mach *crypto.OlmMachine) bool {
	a.mu.Lock()
	unverified := a.deviceIDUnverified
	a.mu.Unlock()
	if client.DeviceID == "" || unverified {
		slog.Warn("Matrix: skipping device key verification — device_id not yet established")
		return true
	}
	ourKeys, err := queryOwnDeviceKeys(ctx, client)
	if err != nil {
		slog.Error(fmt.Sprintf("Matrix: cannot verify device keys on server: %v — refusing E2EE", err))
		return false
	}
	localEd25519 := string(mach.OwnIdentity().SigningKey)
	reupload := func(errorMsg func(err error) string) bool {
		if err := mach.ShareKeys(ctx, -1); err != nil {
			slog.Error(errorMsg(err))
			return false
		}
		return a.reverifyKeysAfterUpload(ctx, client, localEd25519)
	}
	if ourKeys == nil {
		slog.Warn("Matrix: device keys missing from server — re-uploading")
		mach.GetAccount().Shared = false
		return reupload(func(err error) string { return fmt.Sprintf("Matrix: failed to re-upload device keys: %v", err) })
	}
	if extractServerEd25519(*ourKeys) == localEd25519 {
		return true
	}
	if mach.GetAccount().Shared {
		slog.Error(fmt.Sprintf("Matrix: server has different identity keys for device %s — local crypto state is "+
			"stale. Delete %s and restart.", client.DeviceID, a.cryptoDBPath()))
		return false
	}
	slog.Warn(fmt.Sprintf("Matrix: server has stale keys for device %s — attempting re-upload", client.DeviceID))
	if _, err := client.MakeRequest(ctx, "DELETE", client.BuildClientURL("v3", "devices", client.DeviceID), nil, nil); err == nil {
		slog.Info(fmt.Sprintf("Matrix: deleted stale device %s from server", client.DeviceID))
	}
	return reupload(func(err error) string {
		return fmt.Sprintf("Matrix: cannot upload device keys for %s: %v. Try generating a new access token to get a fresh device.", client.DeviceID, err)
	})
}

// Verifies cross-signing with the recovery key, or bootstraps a new recovery key when allowed, never fatal
func (a *MatrixAdapter) verifyOrBootstrapCrossSigning(ctx context.Context, mach *crypto.OlmMachine, client *mautrix.Client) {
	if recoveryKey := scopedRecoveryKey(ctx); recoveryKey != "" {
		if err := mach.VerifyWithRecoveryKey(ctx, recoveryKey); err != nil {
			slog.Warn(fmt.Sprintf("Matrix: recovery key verification failed: %v", err))
		} else {
			slog.Info("Matrix: cross-signing verified via recovery key")
		}
		return
	}
	ownXsign, err := mach.GetOwnCrossSigningPublicKeys(ctx)
	if err != nil {
		ownXsign = nil
		slog.Warn(fmt.Sprintf("Matrix: cross-signing key lookup failed: %v", err))
	}
	if ownXsign != nil {
		return
	}
	_, outputError := getMatrixRecoveryKeyOutputTarget(ctx)
	if outputError != "" {
		var reason string
		switch outputError {
		case "not_configured":
			reason = "is not configured. Configure MATRIX_RECOVERY_KEY from your Matrix client " +
				"or set MATRIX_RECOVERY_KEY_OUTPUT_FILE to write a new recovery key once with mode 0600."
		case "exists":
			reason = "already exists and will not be overwritten."
		default:
			reason = fmt.Sprintf("is not usable: %s", outputError)
		}
		slog.Warn("Matrix: cross-signing keys are missing, but automatic bootstrap is skipped because " +
			"MATRIX_RECOVERY_KEY_OUTPUT_FILE " + reason)
		return
	}
	newRecoveryKey, err := mach.GenerateAndVerifyWithRecoveryKey(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: cross-signing bootstrap failed (non-fatal — Element will show "+
			"'not verified by its owner'): %v", err))
		return
	}
	handleGeneratedMatrixRecoveryKey(ctx, string(client.UserID), newRecoveryKey)
}

// Optional mode logs, disables E2EE and continues while required mode closes and refuses to connect
func (a *MatrixAdapter) e2eeSetupFailed(what string, err error, client *mautrix.Client) bool {
	if a.e2eeMode == "optional" {
		slog.Warn(fmt.Sprintf("Matrix: failed to %s optional E2EE client; continuing without encrypted-room "+
			"support: %v.", what, err))
		a.mu.Lock()
		a.encryption = false
		a.mu.Unlock()
		return true
	}
	slog.Error(fmt.Sprintf("Matrix: failed to %s E2EE client: %v.", what, err))
	return abortConnect(client, nil)
}

// Sets up the olm machine over the per profile SQLite crypto store, false when connect must abort
func (a *MatrixAdapter) connectSetupE2EE(ctx context.Context, client *mautrix.Client, stateStore mautrix.StateStore) bool {
	if !a.encryptionOn() {
		return true
	}
	phase := "import"
	ok, err := a.setupOlm(ctx, client, stateStore, &phase)
	if err != nil {
		return a.e2eeSetupFailed(phase, err, client)
	}
	return ok
}

// Body of the E2EE setup whose errors the caller maps by phase
func (a *MatrixAdapter) setupOlm(ctx context.Context, client *mautrix.Client, stateStore mautrix.StateStore, phase *string) (bool, error) {
	a.mu.Lock()
	storeDir := a.storeDir
	a.mu.Unlock()
	if err := os.MkdirAll(storeDir, 0o777); err != nil {
		return false, err
	}
	*phase = "create"
	legacyPickle := filepath.Join(storeDir, "crypto_store.pickle")
	if _, err := os.Stat(legacyPickle); err == nil {
		slog.Info("Matrix: removing legacy crypto_store.pickle (migrated to SQLite)")
		if err := os.Remove(legacyPickle); err != nil {
			return false, err
		}
	}
	rawDB, err := sql.Open("sqlite", "file:"+a.cryptoDBPath()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return false, err
	}
	db, err := dbutil.NewWithDB(rawDB, "sqlite")
	if err != nil {
		rawDB.Close()
		return false, err
	}
	a.mu.Lock()
	a.cryptoDB = db
	acctID := a.userID
	a.mu.Unlock()
	if acctID == "" {
		acctID = "icebath"
	}
	device := string(client.DeviceID)
	if device == "" {
		device = a.deviceID
	}
	if device == "" {
		device = "default"
	}
	pickleKey := acctID + ":" + device
	store := crypto.NewSQLCryptoStore(db, dbutil.NoopLogger, acctID, client.DeviceID, []byte(pickleKey))
	if err := store.DB.Upgrade(ctx); err != nil {
		return false, err
	}
	storeWasReset := false
	if client.DeviceID != "" {
		if storeWasReset, err = a.resetCryptoStoreIfDeviceChanged(ctx, store, string(client.DeviceID)); err != nil {
			return false, err
		}
		if err := putDeviceID(ctx, store, string(client.DeviceID)); err != nil {
			return false, err
		}
	}
	if !storeWasReset {
		migrated, err := a.migrateLegacyCryptoPickle(ctx, store, db, acctID, pickleKey)
		if err != nil {
			return false, err
		}
		if !migrated {
			slog.Warn("Matrix: crypto pickle migration failed — E2EE may not work correctly")
		}
	}
	cryptoState := &cryptoStateStore{ss: stateStore, joinedRooms: a.joinedRoomIDs, client: client, encInfo: map[id.RoomID]*event.EncryptionEventContent{}}
	log := zerolog.Nop()
	mach := crypto.NewOlmMachine(client, &log, store, cryptoState)
	mach.ShareKeysMinTrust = id.TrustStateUnset
	mach.SendKeysMinTrust = id.TrustStateUnset
	if err := mach.Load(ctx); err != nil {
		return false, err
	}
	if !a.verifyDeviceKeysOnServer(ctx, client, mach) {
		return abortConnect(client, db), nil
	}
	if err := mach.ShareKeys(ctx, -1); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			slog.Error(fmt.Sprintf("Matrix: device %s has stale one-time keys on the server signed with a "+
				"previous identity key. Delete the device from the homeserver and restart, "+
				"or generate a new access token to get a fresh device ID.", client.DeviceID))
			return abortConnect(client, db), nil
		}
		slog.Warn(fmt.Sprintf("Matrix: share_keys() warning during startup: %v", err))
	}
	a.verifyOrBootstrapCrossSigning(ctx, mach, client)
	syncer, _ := client.Syncer.(*mautrix.DefaultSyncer)
	helper := &olmCrypto{mach: mach, client: client, syncer: syncer, inFlight: map[id.RoomID]chan struct{}{}}
	syncer.OnSync(mach.ProcessSyncResponse)
	syncer.OnEventType(event.StateMember, mach.HandleMemberEvent)
	syncer.OnEventType(event.EventEncrypted, a.batched(func(ctx context.Context, evt *event.Event) error {
		helper.handleEncrypted(ctx, evt)
		return nil
	}))
	client.StateStore = &sendStateStore{StateStore: stateStore, client: client}
	client.Crypto = helper
	a.mu.Lock()
	a.crypto = helper
	a.mu.Unlock()
	suffix := ""
	if client.DeviceID != "" {
		suffix = fmt.Sprintf(", device_id=%s", client.DeviceID)
	}
	slog.Info(fmt.Sprintf("Matrix: E2EE enabled (store: %s%s)", a.cryptoDBPath(), suffix))
	return true, nil
}
