package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Olm machine attached to the client once E2EE setup succeeds
type e2eeMachine interface {
	shareKeys(ctx context.Context) error
}

// Transport of a client whose session was closed, failing every request like a closed aiohttp session
type closedTransport struct{}

func (closedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("Session is closed")
}

// Closes what connect opened so far and reports false so callers return it directly
func abortConnect(client *mautrix.Client, cryptoDB io.Closer) bool {
	if cryptoDB != nil {
		_ = cryptoDB.Close()
	}
	if client.Client != nil {
		client.Client.CloseIdleConnections()
	}
	client.Client = &http.Client{Transport: closedTransport{}}
	return false
}

// Authenticates by access token through whoami or by password login, resolving user and device ids
func (a *MatrixAdapter) connectAuthenticate(ctx context.Context, client *mautrix.Client) bool {
	switch {
	case a.accessToken != "":
		client.AccessToken = a.accessToken
		resp, err := client.Whoami(ctx)
		if err != nil {
			slog.Error(fmt.Sprintf("Matrix: whoami failed — check MATRIX_ACCESS_TOKEN and MATRIX_HOMESERVER: %v", err))
			return abortConnect(client, nil)
		}
		resolvedUserID := string(resp.UserID)
		if resolvedUserID == "" {
			resolvedUserID = a.userID
		}
		resolvedDeviceID := string(resp.DeviceID)
		if resolvedUserID != "" {
			a.mu.Lock()
			a.userID = resolvedUserID
			a.mu.Unlock()
			client.UserID = id.UserID(resolvedUserID)
		}
		var effectiveDeviceID string
		if resolvedDeviceID != "" && a.deviceID != "" && resolvedDeviceID != a.deviceID {
			slog.Error(fmt.Sprintf("Matrix: MATRIX_DEVICE_ID=%s does not match the device this access token "+
				"belongs to (%s). A token can only upload keys for its own device, so the "+
				"configured value is being ignored. Unset MATRIX_DEVICE_ID, or use a token "+
				"issued for %s.", a.deviceID, resolvedDeviceID, a.deviceID))
			effectiveDeviceID = resolvedDeviceID
		} else if a.deviceID != "" {
			effectiveDeviceID = a.deviceID
		} else {
			effectiveDeviceID = resolvedDeviceID
		}
		if effectiveDeviceID != "" {
			client.DeviceID = id.DeviceID(effectiveDeviceID)
		}
		if client.DeviceID == "" {
			devResp, err := client.QueryKeys(ctx, &mautrix.ReqQueryKeys{DeviceKeys: mautrix.DeviceKeysRequest{client.UserID: mautrix.DeviceIDList{}}})
			if err != nil {
				slog.Warn(fmt.Sprintf("Matrix: device list query failed: %v", err))
			} else {
				allDevices := devResp.DeviceKeys[client.UserID]
				if len(allDevices) == 1 {
					for deviceID := range allDevices {
						client.DeviceID = deviceID
					}
				} else if len(allDevices) == 0 {
					slog.Warn(fmt.Sprintf("Matrix: no devices found for %s — key verification will be skipped", client.UserID))
				}
			}
		}
		if client.DeviceID == "" {
			slog.Warn(fmt.Sprintf("Matrix: device_id could not be resolved for %s. Set MATRIX_DEVICE_ID for full "+
				"key verification. E2EE will proceed without server-side device key confirmation.", client.UserID))
			a.mu.Lock()
			a.deviceIDUnverified = true
			a.mu.Unlock()
		}
		who := a.userID
		if who == "" {
			who = "(unknown user)"
		}
		suffix := ""
		if effectiveDeviceID != "" {
			suffix = fmt.Sprintf(" (device %s)", effectiveDeviceID)
		}
		slog.Info(fmt.Sprintf("Matrix: using access token for %s%s", who, suffix))
	case a.password != "" && a.userID != "":
		resp, err := client.Login(ctx, &mautrix.ReqLogin{
			Type:       mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: a.userID},
			Password:   a.password, DeviceID: id.DeviceID(a.deviceID), InitialDeviceDisplayName: "Icebath Agent",
			StoreCredentials: true})
		if err != nil {
			slog.Error(fmt.Sprintf("Matrix: login failed — %v", err))
			return abortConnect(client, nil)
		}
		if resp != nil {
			client.DeviceID = resp.DeviceID
		}
		slog.Info(fmt.Sprintf("Matrix: logged in as %s", a.userID))
	default:
		slog.Error("Matrix: need MATRIX_ACCESS_TOKEN or MATRIX_USER_ID + MATRIX_PASSWORD")
		return abortConnect(client, nil)
	}
	return true
}

// Full initial sync seeding joined rooms and the DM cache and dispatching queued to-device events
func (a *MatrixAdapter) connectInitialSync(ctx context.Context, client *mautrix.Client) {
	resp, err := client.SyncRequest(ctx, 10000, "", "", true, "")
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: initial sync error: %v", err))
		return
	}
	a.mu.Lock()
	clear(a.joinedRooms)
	a.mu.Unlock()
	a.absorbSync(ctx, client, resp, "", true)
}

// Logs in, sets up E2EE, registers handlers, runs the initial sync and starts the sync loop
func (a *MatrixAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.mu.Lock()
	a.deviceIDUnverified = false
	hasClient := a.client != nil
	a.mu.Unlock()
	if hasClient {
		if err := a.Disconnect(ctx); err != nil {
			slog.Warn(fmt.Sprintf("Matrix: error disconnecting before reconnect: %v", err))
		}
	}
	if a.homeserver == "" {
		slog.Error("Matrix: homeserver URL not configured")
		return false, nil
	}
	if err := os.MkdirAll(a.resolveStoreDir(ctx), 0o777); err != nil {
		return false, err
	}
	httpClient, err := newMatrixHTTPClient(ctx, a.proxyURL)
	if err != nil {
		return false, err
	}
	client, err := mautrix.NewClient(a.homeserver, id.UserID(a.userID), a.accessToken)
	if err != nil {
		return false, err
	}
	client.Client = httpClient
	client.DeviceID = id.DeviceID(a.deviceID)
	stateStore := mautrix.NewMemoryStateStore()
	client.StateStore = stateStore
	client.Store = mautrix.NewMemorySyncStore()
	syncer := mautrix.NewDefaultSyncer()
	client.Syncer = syncer
	a.mu.Lock()
	a.client = client
	a.stateStore = stateStore
	a.mu.Unlock()
	if !a.connectAuthenticate(ctx, client) {
		return false, nil
	}
	syncer.OnEvent(client.StateStoreSyncHandler)
	if a.encryptionOn() && !a.connectSetupE2EE(ctx, client, stateStore) {
		return false, nil
	}
	syncer.OnEventType(event.EventMessage, a.batched(a.onRoomMessage))
	syncer.OnEventType(event.EventReaction, a.batched(a.onReaction))
	syncer.OnEventType(event.StateMember, a.batched(a.onMemberEvent))
	a.mu.Lock()
	a.startupTS = nowSeconds()
	a.mu.Unlock()
	a.resetClockSkewDetector()
	a.mu.Lock()
	a.closing = false
	a.mu.Unlock()
	a.connectInitialSync(ctx, client)
	if crypto := a.cryptoMachine(); a.encryptionOn() && crypto != nil {
		if err := crypto.shareKeys(ctx); err != nil {
			slog.Warn(fmt.Sprintf("Matrix: initial key share failed: %v", err))
		}
	}
	task := startTask(context.WithoutCancel(ctx), a.syncLoop)
	a.mu.Lock()
	a.syncTask = task
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	a.WirePluginHandlers(ctx, client)
	return true, nil
}

// Attached olm machine, nil without E2EE
func (a *MatrixAdapter) cryptoMachine() e2eeMachine {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.crypto == nil {
		return nil
	}
	return a.crypto
}

// Stops the sync loop and background joins and redactions, closes the crypto store and drops the client
func (a *MatrixAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.closing = true
	syncTask := a.syncTask
	a.mu.Unlock()
	if syncTask != nil && !syncTask.isDone() {
		syncTask.cancel()
		<-syncTask.done
	}
	a.mu.Lock()
	groups := [][]*bgTask{nil, nil}
	for _, t := range a.inviteJoinTasks {
		groups[0] = append(groups[0], t)
	}
	for t := range a.reactionRedactionTasks {
		groups[1] = append(groups[1], t)
	}
	a.mu.Unlock()
	for _, pending := range groups {
		for _, t := range pending {
			if !t.isDone() {
				t.cancel()
			}
		}
		for _, t := range pending {
			<-t.done
		}
	}
	a.mu.Lock()
	clear(a.inviteJoinTasks)
	clear(a.reactionRedactionTasks)
	cryptoDB := a.cryptoDB
	a.cryptoDB = nil
	a.crypto = nil
	client := a.client
	a.client = nil
	a.mu.Unlock()
	if cryptoDB != nil {
		if err := cryptoDB.Close(); err != nil {
			slog.Debug(fmt.Sprintf("Matrix: could not close crypto DB on disconnect: %v", err))
		}
	}
	if client != nil && client.Client != nil {
		client.Client.CloseIdleConnections()
	}
	slog.Info("Matrix: disconnected")
	return nil
}

// Whether the adapter is shutting down
func (a *MatrixAdapter) isClosing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closing
}

// Long polls sync from the stored batch, retrying transient errors every five seconds and stopping on auth failures
func (a *MatrixAdapter) syncLoop(ctx context.Context) {
	client := a.currentClient()
	if client == nil {
		return
	}
	nextBatch, _ := client.Store.LoadNextBatch(ctx, client.UserID)
	for !a.isClosing() {
		sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp, err := client.SyncRequest(sctx, 30000, nextBatch, "", false, "")
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if nb := a.absorbSync(ctx, client, resp, nextBatch, false); nb != "" {
				nextBatch = nb
			}
			runtime.Gosched()
			continue
		}
		if a.isClosing() {
			return
		}
		if isPermanentMatrixAuthError(err) {
			slog.Error(fmt.Sprintf("Matrix: permanent auth error, stopping sync: %v", err))
			return
		}
		slog.Warn(fmt.Sprintf("Matrix: sync error: %v — retrying in 5s", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Applies one sync response to joined rooms, next batch, event dispatch and pending invites, returning next batch
func (a *MatrixAdapter) absorbSync(ctx context.Context, client *mautrix.Client, resp *mautrix.RespSync, since string, initial bool) string {
	a.mu.Lock()
	a.lastSyncTS = nowSeconds()
	roomsJoin := resp.Rooms.Join
	if len(roomsJoin) > 0 || initial {
		for roomID := range roomsJoin {
			a.joinedRooms[string(roomID)] = true
		}
		a.invalidateRoomIdentitiesLocked("")
	}
	joinedCount := len(a.joinedRooms)
	a.mu.Unlock()
	a.warnEncryptedDrops(roomsJoin)
	nb := resp.NextBatch
	if nb != "" {
		_ = client.Store.SaveNextBatch(ctx, client.UserID, nb)
	}
	if initial {
		slog.Info(fmt.Sprintf("Matrix: initial sync complete, joined %d rooms", joinedCount))
		a.refreshDMCache(ctx)
	}
	if err := a.dispatchSync(ctx, resp, since); err != nil {
		label := "sync event dispatch error"
		if initial {
			label = "initial sync event dispatch error"
		}
		slog.Warn(fmt.Sprintf("Matrix: %s: %v", label, err))
	}
	a.schedulePendingInviteJoins(ctx, resp)
	return nb
}

// Fails loud once per room when encrypted events arrive but no decryptor is attached
func (a *MatrixAdapter) warnEncryptedDrops(roomsJoin map[id.RoomID]*mautrix.SyncJoinedRoom) {
	if a.cryptoMachine() != nil {
		return
	}
	roomIDs := make([]string, 0, len(roomsJoin))
	for roomID := range roomsJoin {
		roomIDs = append(roomIDs, string(roomID))
	}
	slices.Sort(roomIDs)
	for _, roomID := range roomIDs {
		a.mu.Lock()
		warned := a.warnedEncryptedDropRooms[roomID]
		a.mu.Unlock()
		if warned {
			continue
		}
		room := roomsJoin[id.RoomID(roomID)]
		if room == nil || !slices.ContainsFunc(room.Timeline.Events, func(ev *event.Event) bool {
			return ev != nil && ev.Type.Type == "m.room.encrypted"
		}) {
			continue
		}
		a.mu.Lock()
		a.warnedEncryptedDropRooms[roomID] = true
		a.mu.Unlock()
		var cause string
		if a.e2eeMode == "off" {
			cause = encryptedDropOffCause()
		} else {
			cause = fmt.Sprintf("E2EE mode is %s but the decryptor was not set up at connect; "+
				"see the earlier Matrix E2EE warning for the cause", a.e2eeMode)
		}
		slog.Warn(fmt.Sprintf("Matrix: dropping encrypted messages in %s — this process has no E2EE decryptor. %s. "+
			"Without it, messages in encrypted rooms never reach the agent.", roomID, cause))
	}
}

// Handlers spawned by one sync dispatch and the errors they returned before the dispatch finished
type syncBatch struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	errs   []error
	closed bool
}

// Records a handler error, false once the dispatch finished and nobody collects it
func (b *syncBatch) record(err error) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.errs = append(b.errs, err)
	return true
}

// Joins a handler to the batch, false once the dispatch finished
func (b *syncBatch) join() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.wg.Add(1)
	return true
}

type syncBatchKey struct{}

// Event handler run as its own goroutine of the current sync batch, errors and panics collected
func (a *MatrixAdapter) batched(fn func(ctx context.Context, evt *event.Event) error) mautrix.EventHandler {
	return func(ctx context.Context, evt *event.Event) {
		batch, _ := ctx.Value(syncBatchKey{}).(*syncBatch)
		run := func() {
			err := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("%v", r)
					}
				}()
				return fn(ctx, evt)
			}()
			if err == nil || (batch != nil && batch.record(err)) {
				return
			}
			slog.Warn(fmt.Sprintf("Matrix: event handler failed during sync dispatch: %v", err))
		}
		if batch == nil || !batch.join() {
			go run()
			return
		}
		go func() {
			defer batch.wg.Done()
			run()
		}()
	}
}

// Dispatches a sync response through the syncer and waits for every handler it started
func (a *MatrixAdapter) dispatchSync(ctx context.Context, resp *mautrix.RespSync, since string) error {
	client := a.currentClient()
	if client == nil {
		return nil
	}
	syncer, ok := client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return nil
	}
	batch := &syncBatch{}
	err := syncer.ProcessResponse(context.WithValue(ctx, syncBatchKey{}, batch), resp, since)
	batch.wg.Wait()
	batch.mu.Lock()
	batch.closed = true
	errs := batch.errs
	batch.mu.Unlock()
	for _, herr := range errs {
		slog.Warn(fmt.Sprintf("Matrix: event handler failed during sync dispatch: %v", herr))
	}
	return err
}
