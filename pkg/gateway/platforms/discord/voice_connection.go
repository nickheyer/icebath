package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Handshake progress of a voice connection, discord.py's ConnectionFlowState
type voiceFlowState int

const (
	flowDisconnected voiceFlowState = iota
	flowSetGuildVoiceState
	flowGotVoiceStateUpdate
	flowGotVoiceServerUpdate
	flowGotBothVoiceUpdates
	flowWebsocketConnected
	flowGotWebsocketReady
	flowGotIPDiscovery
	flowConnected
)

var voiceFlowNames = [...]string{"disconnected", "set_guild_voice_state", "got_voice_state_update", "got_voice_server_update",
	"got_both_voice_updates", "websocket_connected", "got_websocket_ready", "got_ip_discovery", "connected"}

// Identity fields the voice gateway identifies and resumes with
type voiceIdentity struct {
	token, sessionID string
	serverID, userID int64
}

// Context key marking the websocket poller goroutine of a connection state
type voiceRunnerKey struct{ state *voiceConnState }

// Internal state of one voice connection, discord.py's VoiceConnectionState
type voiceConnState struct {
	vc        *voiceClient
	mu        sync.Mutex
	hook      voiceWSHook
	timeout   time.Duration
	reconnect bool
	selfDeaf  bool
	selfMute  bool

	token, sessionID, endpoint, endpointIP string
	serverID                               int64
	ip                                     string
	port, voicePort                        int
	secretKey                              []byte
	ssrc                                   uint32
	mode                                   string
	socket                                 *net.UDPConn
	ws                                     *voiceWebSocket

	dave                   *daveSession
	daveProtocolVersion    int
	davePendingTransitions map[int]int
	daveDowngraded         bool
	daveRecognized         map[string]bool

	state               voiceFlowState
	stateCh             chan struct{}
	expectingDisconnect bool
	disconnected        chan struct{}
	disconnectedSet     bool
	connected           chan struct{}

	runner    *bgTask
	connector *bgTask
	reader    *socketReader
}

func newVoiceConnState(vc *voiceClient) *voiceConnState {
	return &voiceConnState{vc: vc, timeout: 30 * time.Second, reconnect: true, davePendingTransitions: map[int]int{},
		daveRecognized: map[string]bool{}, stateCh: make(chan struct{}), disconnected: make(chan struct{}),
		connected: make(chan struct{}), reader: newSocketReader()}
}

// Moves to a flow state, waking every waiter and the connected event
func (s *voiceConnState) setStateLocked(st voiceFlowState) {
	if st != s.state {
		slog.Debug(fmt.Sprintf("Connection state changed to %s", voiceFlowNames[st]))
	}
	s.state = st
	close(s.stateCh)
	s.stateCh = make(chan struct{})
	select {
	case <-s.connected:
		if st != flowConnected {
			s.connected = make(chan struct{})
		}
	default:
		if st == flowConnected {
			close(s.connected)
		}
	}
}

func (s *voiceConnState) setState(st voiceFlowState) {
	s.mu.Lock()
	s.setStateLocked(st)
	s.mu.Unlock()
}

func (s *voiceConnState) flowState() voiceFlowState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *voiceConnState) isConnected() bool { return s.flowState() == flowConnected }

// Waits until the connection reaches one of the states, bounded by ctx
func (s *voiceConnState) waitForState(ctx context.Context, states ...voiceFlowState) error {
	for {
		s.mu.Lock()
		for _, st := range states {
			if s.state == st {
				s.mu.Unlock()
				return nil
			}
		}
		ch := s.stateCh
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Waits up to timeout for the connected state, reporting whether it holds
func (s *voiceConnState) wait(timeout time.Duration) bool {
	s.mu.Lock()
	ch := s.connected
	s.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(timeout):
	}
	return s.isConnected()
}

func (s *voiceConnState) endpointValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoint
}

func (s *voiceConnState) snapshotIdentity() voiceIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return voiceIdentity{token: s.token, sessionID: s.sessionID, serverID: s.serverID, userID: s.vc.userID}
}

func (s *voiceConnState) ssrcValue() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ssrc
}

func (s *voiceConnState) setMode(mode string) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *voiceConnState) setSecretKey(key []byte) {
	s.mu.Lock()
	s.secretKey = key
	s.mu.Unlock()
}

func (s *voiceConnState) setReady(ssrc uint32, port int, ip string) {
	s.mu.Lock()
	s.ssrc, s.voicePort, s.endpointIP = ssrc, port, ip
	s.mu.Unlock()
}

func (s *voiceConnState) setDiscovered(ip string, port int) {
	s.mu.Lock()
	s.ip, s.port = ip, port
	s.mu.Unlock()
}

func (s *voiceConnState) hookValue() voiceWSHook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hook
}

func (s *voiceConnState) wsValue() *voiceWebSocket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws
}

func (s *voiceConnState) daveSessionValue() *daveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dave
}

func (s *voiceConnState) setDaveProtocolVersion(pv int) {
	s.mu.Lock()
	s.daveProtocolVersion = pv
	s.mu.Unlock()
}

func (s *voiceConnState) daveProtocolVersionValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daveProtocolVersion
}

func (s *voiceConnState) addPendingTransition(tid, pv int) {
	s.mu.Lock()
	s.davePendingTransitions[tid] = pv
	s.mu.Unlock()
}

// Whether frames can be end to end encrypted, a ready session under a nonzero protocol
func (s *voiceConnState) canEncrypt() bool {
	s.mu.Lock()
	pv, dave := s.daveProtocolVersion, s.dave
	s.mu.Unlock()
	return pv != 0 && dave != nil && dave.isReady()
}

// Records users the voice server announced so MLS accepts their credentials
func (s *voiceConnState) recognizeUsers(ids []string) {
	s.mu.Lock()
	for _, id := range ids {
		s.daveRecognized[id] = true
	}
	dave := s.dave
	s.mu.Unlock()
	if dave != nil {
		dave.addRecognizedUsers(ids)
	}
}

// Forgets a user who left the voice channel
func (s *voiceConnState) forgetUser(id string) {
	s.mu.Lock()
	delete(s.daveRecognized, id)
	dave := s.dave
	s.mu.Unlock()
	if dave != nil {
		dave.removeRecognizedUser(id)
	}
}

// Creates or restarts the DAVE session for the current protocol and sends the key package, resetting on protocol zero
func (s *voiceConnState) reinitDaveSession() error {
	s.mu.Lock()
	pv, dave, ws := s.daveProtocolVersion, s.dave, s.ws
	s.mu.Unlock()
	if pv > 0 {
		api, err := loadDave()
		if err != nil {
			return fmt.Errorf("RuntimeError: libdave library needed in order to use E2EE voice: %w", err)
		}
		channelID := s.vc.channelIDValue()
		if dave != nil {
			dave.reinit(pv, s.vc.userID, channelID)
		} else {
			dave = newDaveSession(api, pv, s.vc.userID, channelID)
			s.mu.Lock()
			s.dave = dave
			var ids []string
			for id := range s.daveRecognized {
				ids = append(ids, id)
			}
			s.mu.Unlock()
			dave.addRecognizedUsers(ids)
		}
		if ws != nil {
			return ws.sendBinary(voiceOpMLSKeyPackage, dave.keyPackage())
		}
		return nil
	}
	if dave != nil {
		dave.reset()
		dave.setPassthroughMode(true, 10)
	}
	return nil
}

// Flags a commit or welcome the session rejected and starts the group over
func (s *voiceConnState) recoverFromInvalidCommit(transitionID int) error {
	if ws := s.wsValue(); ws != nil {
		if err := ws.sendAsJSON(map[string]any{"op": voiceOpMLSInvalidCommitWelcome, "d": map[string]any{"transition_id": transitionID}}); err != nil {
			return err
		}
	}
	return s.reinitDaveSession()
}

// Applies a prepared transition's protocol version, tracking downgrades and the upgrade grace
func (s *voiceConnState) executeTransition(transitionID int) {
	slog.Debug(fmt.Sprintf("Executing transition id %d", transitionID))
	s.mu.Lock()
	pv, ok := s.davePendingTransitions[transitionID]
	if !ok {
		s.mu.Unlock()
		slog.Warn(fmt.Sprintf("Received execute transition, but we don't have a pending transition for id %d", transitionID))
		return
	}
	delete(s.davePendingTransitions, transitionID)
	old := s.daveProtocolVersion
	s.daveProtocolVersion = pv
	dave := s.dave
	upgraded := false
	if old != pv && pv == 0 {
		s.daveDowngraded = true
		slog.Debug("DAVE Session downgraded")
	} else if transitionID > 0 && s.daveDowngraded {
		s.daveDowngraded = false
		upgraded = true
	}
	s.mu.Unlock()
	if dave != nil {
		dave.executeTransition(transitionID)
		if upgraded {
			dave.setPassthroughMode(true, 10)
		}
	}
	if upgraded {
		slog.Debug("DAVE Session upgraded")
	}
	slog.Debug(fmt.Sprintf("Transition id %d executed", transitionID))
}

// Main gateway VOICE_STATE_UPDATE of the bot, advancing the handshake or following a channel move
func (s *voiceConnState) voiceStateUpdate(ctx context.Context, channelID, sessionID string) {
	if channelID == "" {
		s.mu.Lock()
		if !s.disconnectedSet {
			s.disconnectedSet = true
			close(s.disconnected)
		}
		expecting := s.expectingDisconnect
		s.expectingDisconnect = false
		s.mu.Unlock()
		if !expecting {
			slog.Debug("We were externally disconnected from voice.")
			s.disconnect(ctx, true, true, false)
		}
		return
	}
	cid, _ := strconv.ParseInt(channelID, 10, 64)
	s.mu.Lock()
	s.sessionID = sessionID
	st := s.state
	switch st {
	case flowSetGuildVoiceState:
		s.setStateLocked(flowGotVoiceStateUpdate)
		s.mu.Unlock()
		if cid != s.vc.channelIDValue() {
			s.vc.updateVoiceChannel(cid)
		}
		return
	case flowGotVoiceServerUpdate:
		s.setStateLocked(flowGotBothVoiceUpdates)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if st == flowConnected {
		s.vc.updateVoiceChannel(cid)
		return
	}
	if st == flowDisconnected {
		return
	}
	if cid != s.vc.channelIDValue() {
		slog.Info("Handling channel move while connecting...")
		s.vc.updateVoiceChannel(cid)
		s.softDisconnect(flowGotVoiceStateUpdate)
		deaf, mute := s.selfFlags()
		s.connect(ctx, s.reconnectValue(), s.timeoutValue(), deaf, mute, false, false)
		return
	}
	slog.Debug("Ignoring unexpected voice_state_update event")
}

// Main gateway VOICE_SERVER_UPDATE, recording the endpoint and restarting the socket of a live connection
func (s *voiceConnState) voiceServerUpdate(ctx context.Context, token, guildID, endpoint string) {
	s.mu.Lock()
	prevToken, prevServer, prevEndpoint := s.token, s.serverID, s.endpoint
	s.token = token
	s.serverID, _ = strconv.ParseInt(guildID, 10, 64)
	if token == "" || endpoint == "" {
		s.mu.Unlock()
		slog.Warn("Awaiting endpoint... This requires waiting. If timeout occurred considering raising the timeout and reconnecting.")
		return
	}
	s.endpoint = strings.TrimPrefix(endpoint, "wss://")
	st := s.state
	switch st {
	case flowSetGuildVoiceState, flowGotVoiceStateUpdate:
		s.endpointIP = ""
		s.createSocketLocked()
		if st == flowSetGuildVoiceState {
			s.setStateLocked(flowGotVoiceServerUpdate)
		} else {
			s.setStateLocked(flowGotBothVoiceUpdates)
		}
		s.mu.Unlock()
		return
	case flowConnected:
		ws := s.ws
		s.mu.Unlock()
		slog.Debug("Voice server update, closing old voice websocket")
		if ws != nil {
			ws.close(4014)
		}
		s.setState(flowGotVoiceServerUpdate)
		return
	case flowDisconnected:
		s.mu.Unlock()
		return
	}
	same := prevToken == s.token && prevServer == s.serverID && prevEndpoint == s.endpoint
	s.mu.Unlock()
	if same {
		return
	}
	slog.Debug("Unexpected server update event, attempting to handle")
	s.softDisconnect(flowGotVoiceServerUpdate)
	deaf, mute := s.selfFlags()
	s.connect(ctx, s.reconnectValue(), s.timeoutValue(), deaf, mute, false, false)
	s.mu.Lock()
	s.createSocketLocked()
	s.mu.Unlock()
}

func (s *voiceConnState) reconnectValue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconnect
}

func (s *voiceConnState) timeoutValue() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeout
}

// Self deaf and mute of the bot's live voice state, else of this connection's settings
func (s *voiceConnState) selfFlags() (bool, bool) {
	if vs := s.vc.selfVoiceState(); vs != nil {
		return vs.SelfDeaf, vs.SelfMute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selfDeaf, s.selfMute
}

// Starts the connector, cancelling an earlier connector and poller, and waits for it when asked
func (s *voiceConnState) connect(ctx context.Context, reconnect bool, timeout time.Duration, selfDeaf, selfMute, resume, wait bool) error {
	s.mu.Lock()
	connector, runner := s.connector, s.runner
	s.connector, s.runner = nil, nil
	s.timeout, s.reconnect = timeout, reconnect
	s.mu.Unlock()
	if connector != nil {
		connector.cancel()
	}
	if runner != nil {
		runner.cancel()
	}
	errCh := make(chan error, 1)
	task := spawnTask(s.vc.baseContext(), func(tctx context.Context) {
		errCh <- s.wrapConnect(tctx, reconnect, timeout, selfDeaf, selfMute, resume)
	})
	s.mu.Lock()
	s.connector = task
	s.mu.Unlock()
	if !wait {
		return nil
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		task.cancel()
		return ctx.Err()
	}
}

// Runs one connect, disconnecting on timeout or failure and soft disconnecting on cancellation
func (s *voiceConnState) wrapConnect(ctx context.Context, reconnect bool, timeout time.Duration, selfDeaf, selfMute, resume bool) error {
	err := s.connectOnce(ctx, reconnect, timeout, selfDeaf, selfMute, resume)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		slog.Debug("Cancelling voice connection")
		s.softDisconnect(flowGotBothVoiceUpdates)
	case errors.Is(err, context.DeadlineExceeded):
		slog.Info("Timed out connecting to voice")
		s.disconnect(context.Background(), true, true, false)
		err = errVoiceConnectTimeout
	default:
		slog.Error(fmt.Sprintf("Error connecting to voice... disconnecting: %s", err))
		s.disconnect(context.Background(), true, true, false)
	}
	return err
}

// Voice connect that did not finish within its timeout
var errVoiceConnectTimeout = errors.New("TimeoutError")

// Handshake bounded by the timeout, then the websocket poller
func (s *voiceConnState) connectOnce(ctx context.Context, reconnect bool, timeout time.Duration, selfDeaf, selfMute, resume bool) error {
	slog.Info("Connecting to voice...")
	tctx, cancel := context.WithTimeout(ctx, timeout)
	err := s.innerConnect(tctx, reconnect, selfDeaf, selfMute, resume)
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return err
	}
	slog.Info("Voice connection complete.")
	s.mu.Lock()
	if s.runner == nil {
		s.runner = spawnTask(s.vc.baseContext(), func(rctx context.Context) {
			s.pollVoiceWS(context.WithValue(rctx, voiceRunnerKey{s}, true), reconnect)
		})
	}
	s.mu.Unlock()
	return nil
}

// Up to five handshakes, retrying closed websockets with a growing wait when reconnecting
func (s *voiceConnState) innerConnect(ctx context.Context, reconnect, selfDeaf, selfMute, resume bool) error {
	for i := range 5 {
		slog.Info(fmt.Sprintf("Starting voice handshake... (connection attempt %d)", i+1))
		if err := s.vc.changeVoiceState(s.vc.channelIDValue(), selfDeaf, selfMute); err != nil {
			return err
		}
		s.mu.Lock()
		if s.state == flowDisconnected {
			s.setStateLocked(flowSetGuildVoiceState)
		}
		s.mu.Unlock()
		if err := s.waitForState(ctx, flowGotBothVoiceUpdates); err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("Voice handshake complete. Endpoint found: %s", s.endpointValue()))
		err := s.connectWebsocket(ctx, resume)
		if err == nil {
			err = s.handshakeWebsocket(ctx)
		}
		if err == nil {
			return nil
		}
		var closed *voiceConnectionClosed
		if !errors.As(err, &closed) {
			return err
		}
		if !reconnect {
			s.disconnect(context.Background(), true, true, false)
			return err
		}
		wait := time.Duration(1+i*2) * time.Second
		slog.Error(fmt.Sprintf("Failed to connect to voice... Retrying in %ss...", strconv.FormatFloat(wait.Seconds(), 'f', 1, 64)))
		s.disconnect(context.Background(), true, false, false)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Opens the voice websocket carrying the previous socket's acknowledged sequence
func (s *voiceConnState) connectWebsocket(ctx context.Context, resume bool) error {
	seqAck := -1
	if prev := s.wsValue(); prev != nil {
		seqAck = prev.sequenceAck()
	}
	ws, err := dialVoiceWebSocket(ctx, s, resume, seqAck)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ws = ws
	s.setStateLocked(flowWebsocketConnected)
	s.mu.Unlock()
	return nil
}

// Polls the websocket until IP discovery finished and the secret key arrived
func (s *voiceConnState) handshakeWebsocket(ctx context.Context) error {
	ws := s.wsValue()
	for {
		s.mu.Lock()
		ip := s.ip
		s.mu.Unlock()
		if ip != "" {
			break
		}
		if err := s.pollWithin(ctx, ws); err != nil {
			return err
		}
	}
	s.setState(flowGotIPDiscovery)
	for ws.receivedSecretKey() == nil {
		if err := s.pollWithin(ctx, ws); err != nil {
			return err
		}
	}
	s.setState(flowConnected)
	return nil
}

// One websocket poll that also ends when ctx does, closing the socket to unblock the read
func (s *voiceConnState) pollWithin(ctx context.Context, ws *voiceWebSocket) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ws.conn.Close()
		case <-done:
		}
	}()
	err := ws.pollEvent(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Replaces the UDP socket of the next handshake, resuming the reader when listeners wait
func (s *voiceConnState) createSocketLocked() {
	if s.socket != nil {
		s.socket.Close()
		s.socket = nil
	}
	s.reader.setSocket(nil)
	s.reader.resume(false)
}

// Connects the UDP socket to the voice server address READY named
func (s *voiceConnState) connectSocket() error {
	s.mu.Lock()
	ip, port := s.endpointIP, s.voicePort
	s.mu.Unlock()
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	sock, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.socket != nil {
		s.socket.Close()
	}
	s.socket = sock
	s.mu.Unlock()
	s.reader.setSocket(sock)
	s.reader.resume(false)
	return nil
}

// Sends one datagram on the voice socket
func (s *voiceConnState) sendPacket(packet []byte) error {
	s.mu.Lock()
	sock := s.socket
	s.mu.Unlock()
	if sock == nil {
		return &net.OpError{Op: "write", Net: "udp", Err: errors.New("socket is not connected")}
	}
	_, err := sock.Write(packet)
	return err
}

func (s *voiceConnState) addSocketListener(fn func([]byte)) int {
	slog.Debug("Registering socket listener callback")
	return s.reader.register(fn)
}

func (s *voiceConnState) removeSocketListener(id int) {
	slog.Debug("Unregistering socket listener callback")
	s.reader.unregister(id)
}

// Leaves voice, closing the websocket and socket, cleaning the client up and waiting for the gateway to confirm when asked
func (s *voiceConnState) disconnect(ctx context.Context, force, cleanup, wait bool) {
	if !force && !s.isConnected() {
		return
	}
	s.voiceDisconnect()
	if ws := s.wsValue(); ws != nil {
		ws.close(1000)
	}
	s.mu.Lock()
	s.setStateLocked(flowDisconnected)
	s.mu.Unlock()
	s.reader.pause()
	if cleanup {
		s.reader.stop()
		s.vc.stopPlayer()
	}
	s.mu.Lock()
	select {
	case <-s.connected:
	default:
		close(s.connected)
	}
	s.connected = make(chan struct{})
	if s.socket != nil {
		s.socket.Close()
		s.socket = nil
	}
	s.ip, s.port = "", 0
	disconnected, timeout := s.disconnected, s.timeout
	s.mu.Unlock()
	if wait && ctx.Value(voiceRunnerKey{s}) == nil {
		select {
		case <-disconnected:
		case <-time.After(timeout):
			slog.Debug("Timed out waiting for voice disconnection confirmation")
		case <-ctx.Done():
		}
	}
	if cleanup {
		s.vc.cleanup()
	}
}

// Ends the websocket and socket while keeping the voice state, ready for a fresh handshake
func (s *voiceConnState) softDisconnect(with voiceFlowState) {
	slog.Debug("Soft disconnecting from voice")
	s.mu.Lock()
	runner := s.runner
	s.runner = nil
	ws := s.ws
	s.mu.Unlock()
	if runner != nil {
		runner.cancel()
	}
	if ws != nil {
		ws.close(1000)
	}
	s.mu.Lock()
	s.setStateLocked(with)
	if s.socket != nil {
		s.socket.Close()
		s.socket = nil
	}
	s.ip, s.port = "", 0
	s.mu.Unlock()
	s.reader.pause()
}

// Tells the main gateway the bot leaves voice and expects the confirming state update
func (s *voiceConnState) voiceDisconnect() {
	slog.Info(fmt.Sprintf("The voice handshake is being terminated for Channel ID %d (Guild ID %d)", s.vc.channelIDValue(), s.vc.guildID))
	s.setState(flowDisconnected)
	if err := s.vc.changeVoiceState(0, false, false); err != nil {
		slog.Debug(fmt.Sprintf("Ignoring exception disconnecting from voice: %s", err))
	}
	s.mu.Lock()
	s.expectingDisconnect = true
	if s.disconnectedSet {
		s.disconnected = make(chan struct{})
		s.disconnectedSet = false
	}
	s.mu.Unlock()
}

// Moves to another voice channel of the guild, reverting the flow state when the move times out
func (s *voiceConnState) moveTo(ctx context.Context, channelID int64, timeout time.Duration) {
	if channelID == 0 {
		s.disconnect(ctx, true, true, true)
		return
	}
	if channelID == s.vc.channelIDValue() {
		return
	}
	previous := s.flowState()
	if err := s.vc.changeVoiceState(channelID, false, false); err != nil {
		slog.Debug(fmt.Sprintf("Voice move request failed: %s", err))
	}
	s.setState(flowSetGuildVoiceState)
	last := s.flowState()
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.waitForState(wctx, flowConnected); err != nil {
		slog.Warn(fmt.Sprintf("Timed out trying to move to channel %d in guild %d", channelID, s.vc.guildID))
		s.mu.Lock()
		if s.state == last {
			slog.Debug(fmt.Sprintf("Reverting to previous state %s", voiceFlowNames[previous]))
			s.setStateLocked(previous)
		}
		s.mu.Unlock()
	}
}

// Reads the websocket for the connection's lifetime, resuming, reconnecting or disconnecting by close code
func (s *voiceConnState) pollVoiceWS(ctx context.Context, reconnect bool) {
	backoff := &exponentialBackoff{}
	for {
		ws := s.wsValue()
		err := ws.pollEvent(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		var closed *voiceConnectionClosed
		isClosed := errors.As(err, &closed)
		if !isClosed && !errors.Is(err, errVoiceReadTimeout) {
			slog.Error(fmt.Sprintf("Voice websocket poller failed: %s", err))
			return
		}
		if isClosed {
			switch closed.code {
			case 1000:
				s.mu.Lock()
				expecting := s.expectingDisconnect
				s.mu.Unlock()
				if !expecting {
					slog.Info(fmt.Sprintf("Disconnecting from voice normally, close code %d.", closed.code))
					s.disconnect(ctx, true, true, false)
				}
				return
			case 4014, 4022:
				s.mu.Lock()
				gone := s.disconnectedSet
				s.mu.Unlock()
				if gone {
					slog.Info(fmt.Sprintf("Disconnected from voice by discord, close code %d.", closed.code))
					s.disconnect(ctx, true, true, false)
					return
				}
				slog.Info("Disconnected from voice by force... potentially reconnecting.")
				reconnected, err := s.potentialReconnect(ctx)
				if err != nil {
					slog.Error(fmt.Sprintf("Voice websocket poller failed: %s", err))
					return
				}
				if !reconnected {
					slog.Info("Reconnect was unsuccessful, disconnecting from voice normally...")
					if s.flowState() != flowDisconnected {
						s.disconnect(ctx, true, true, false)
					}
					return
				}
				continue
			case 4021:
				slog.Warn("We are being ratelimited while trying to connect to voice. Disconnecting...")
				if s.flowState() != flowDisconnected {
					s.disconnect(ctx, true, true, false)
				}
				return
			case 4015:
				slog.Info("Disconnected from voice, attempting a resume...")
				deaf, mute := s.selfFlags()
				if err := s.connectOnce(ctx, reconnect, s.timeoutValue(), deaf, mute, true); err != nil {
					if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
						slog.Error(fmt.Sprintf("Voice websocket poller failed: %s", err))
						return
					}
					slog.Info("Could not resume the voice connection... Disconnecting...")
					if s.flowState() != flowDisconnected {
						s.disconnect(ctx, true, true, false)
					}
					return
				}
				slog.Info("Successfully resumed voice connection")
				continue
			}
			slog.Debug(fmt.Sprintf("Not handling close code %d (no reason)", closed.code))
		}
		if !reconnect {
			s.disconnect(ctx, true, true, false)
			return
		}
		retry := backoff.delay()
		slog.Error(fmt.Sprintf("Disconnected from voice... Reconnecting in %.2fs.", retry.Seconds()))
		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return
		}
		s.disconnect(ctx, true, false, false)
		deaf, mute := s.selfFlags()
		if err := s.connectOnce(ctx, reconnect, s.timeoutValue(), deaf, mute, false); err != nil {
			if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
				slog.Error(fmt.Sprintf("Voice websocket poller failed: %s", err))
				return
			}
			slog.Warn("Could not connect to voice... Retrying...")
			continue
		}
	}
}

// Waits for the replacement voice server and handshakes a new websocket, closing the old one either way
func (s *voiceConnState) potentialReconnect(ctx context.Context) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, s.timeoutValue())
	err := s.waitForState(wctx, flowGotVoiceServerUpdate, flowGotBothVoiceUpdates, flowDisconnected)
	cancel()
	if err != nil || s.flowState() == flowDisconnected {
		return false, nil
	}
	previous := s.wsValue()
	defer func() {
		if previous != nil {
			previous.close(1000)
		}
	}()
	err = s.connectWebsocket(ctx, false)
	if err == nil {
		err = s.handshakeWebsocket(ctx)
	}
	var closed *voiceConnectionClosed
	if err == nil || errors.As(err, &closed) || errors.Is(err, errVoiceReadTimeout) || ctx.Err() != nil {
		return err == nil, nil
	}
	return false, err
}
