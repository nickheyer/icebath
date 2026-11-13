package discord

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Voice gateway opcodes of version 8 with the DAVE and MLS opcodes
const (
	voiceOpIdentify                    = 0
	voiceOpSelectProtocol              = 1
	voiceOpReady                       = 2
	voiceOpHeartbeat                   = 3
	voiceOpSessionDescription          = 4
	voiceOpSpeaking                    = 5
	voiceOpHeartbeatAck                = 6
	voiceOpResume                      = 7
	voiceOpHello                       = 8
	voiceOpResumed                     = 9
	voiceOpClientsConnect              = 11
	voiceOpClientDisconnect            = 13
	voiceOpDavePrepareTransition       = 21
	voiceOpDaveExecuteTransition       = 22
	voiceOpDaveTransitionReady         = 23
	voiceOpDavePrepareEpoch            = 24
	voiceOpMLSExternalSender           = 25
	voiceOpMLSKeyPackage               = 26
	voiceOpMLSProposals                = 27
	voiceOpMLSCommitWelcome            = 28
	voiceOpMLSAnnounceCommitTransition = 29
	voiceOpMLSWelcome                  = 30
	voiceOpMLSInvalidCommitWelcome     = 31
)

// Speaking flags of the SPEAKING opcode
const (
	speakingNone  = 0
	speakingVoice = 1
)

// One JSON frame of the voice gateway as the hook sees it
type voiceGatewayMessage struct {
	Op  int            `json:"op"`
	D   map[string]any `json:"d"`
	Seq *int           `json:"seq,omitempty"`
}

// Websocket hook called with every JSON frame after the client handled it
type voiceWSHook func(ctx context.Context, msg voiceGatewayMessage)

// Voice gateway connection closed by either side, carrying the close code
type voiceConnectionClosed struct{ code int }

func (e *voiceConnectionClosed) Error() string {
	return fmt.Sprintf("Shard ID None WebSocket closed with %d", e.code)
}

// Voice gateway read that saw no frame for 30 seconds
var errVoiceReadTimeout = errors.New("TimeoutError")

// Websocket of one voice gateway session
type voiceWebSocket struct {
	conn      *websocket.Conn
	state     *voiceConnState
	gateway   string
	writeMu   sync.Mutex
	mu        sync.Mutex
	keepAlive *voiceKeepAlive
	closeCode int
	secretKey []byte
	seqAck    int
}

// Dialer of voice gateway sockets, a copy of the bot client's dialer carrying its proxy and TLS settings
func voiceDialer(base *websocket.Dialer) *websocket.Dialer {
	d := &websocket.Dialer{HandshakeTimeout: 45 * time.Second, EnableCompression: true}
	if base != nil {
		d.Proxy, d.TLSClientConfig, d.HandshakeTimeout = base.Proxy, base.TLSClientConfig, base.HandshakeTimeout
	}
	return d
}

// Opens the voice gateway of a connection state and identifies, or resumes with the acknowledged sequence
func dialVoiceWebSocket(ctx context.Context, state *voiceConnState, resume bool, seqAck int) (*voiceWebSocket, error) {
	gw := "wss://" + state.endpointValue() + "/?v=8"
	conn, _, err := voiceDialer(state.vc.dialer()).DialContext(ctx, gw, nil)
	if err != nil {
		return nil, err
	}
	ws := &voiceWebSocket{conn: conn, state: state, gateway: gw, seqAck: seqAck}
	if resume {
		err = ws.resume()
	} else {
		err = ws.identify()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ws, nil
}

// Sends one JSON frame
func (ws *voiceWebSocket) sendAsJSON(data any) error {
	slog.Debug(fmt.Sprintf("Sending voice websocket frame: %v.", data))
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return ws.conn.WriteJSON(data)
}

// Sends one binary frame, the opcode byte followed by the payload
func (ws *voiceWebSocket) sendBinary(opcode int, data []byte) error {
	slog.Debug(fmt.Sprintf("Sending voice websocket binary frame: opcode=%d size=%d", opcode, len(data)))
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return ws.conn.WriteMessage(websocket.BinaryMessage, append([]byte{byte(opcode)}, data...))
}

// Last sequence number the server sent
func (ws *voiceWebSocket) sequenceAck() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.seqAck
}

// Secret key the session description delivered, nil until then
func (ws *voiceWebSocket) receivedSecretKey() []byte {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.secretKey
}

func (ws *voiceWebSocket) resume() error {
	st := ws.state.snapshotIdentity()
	return ws.sendAsJSON(map[string]any{"op": voiceOpResume, "d": map[string]any{
		"token": st.token, "server_id": strconv.FormatInt(st.serverID, 10), "session_id": st.sessionID, "seq_ack": ws.sequenceAck()}})
}

func (ws *voiceWebSocket) identify() error {
	st := ws.state.snapshotIdentity()
	return ws.sendAsJSON(map[string]any{"op": voiceOpIdentify, "d": map[string]any{
		"server_id": strconv.FormatInt(st.serverID, 10), "user_id": strconv.FormatInt(st.userID, 10), "session_id": st.sessionID,
		"token": st.token, "max_dave_protocol_version": daveMaxProtocolVersion()}})
}

func (ws *voiceWebSocket) selectProtocol(ip string, port int, mode string) error {
	return ws.sendAsJSON(map[string]any{"op": voiceOpSelectProtocol, "d": map[string]any{
		"protocol": "udp", "data": map[string]any{"address": ip, "port": port, "mode": mode}}})
}

// Announces the speaking state of the bot's SSRC
func (ws *voiceWebSocket) speak(state int) error {
	return ws.sendAsJSON(map[string]any{"op": voiceOpSpeaking, "d": map[string]any{"speaking": state, "delay": 0, "ssrc": ws.state.ssrcValue()}})
}

func (ws *voiceWebSocket) sendTransitionReady(transitionID int) error {
	return ws.sendAsJSON(map[string]any{"op": voiceOpDaveTransitionReady, "d": map[string]any{"transition_id": transitionID}})
}

// Integer of a JSON number or numeric string, the Python int() of a payload field
func voiceInt(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	case int:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

// Handles one JSON frame then hands it to the hook
func (ws *voiceWebSocket) receivedMessage(ctx context.Context, msg voiceGatewayMessage) error {
	slog.Debug(fmt.Sprintf("Voice websocket frame received: %v", msg))
	state := ws.state
	data := msg.D
	if msg.Seq != nil {
		ws.mu.Lock()
		ws.seqAck = *msg.Seq
		ws.mu.Unlock()
	}
	intField := func(key string) (int, error) {
		n, ok := voiceInt(data[key])
		if !ok {
			return 0, fmt.Errorf("KeyError: '%s'", key)
		}
		return int(n), nil
	}
	switch {
	case msg.Op == voiceOpReady:
		if err := ws.initialConnection(ctx, data); err != nil {
			return err
		}
	case msg.Op == voiceOpHeartbeatAck:
		ws.mu.Lock()
		ka := ws.keepAlive
		ws.mu.Unlock()
		if ka != nil {
			ka.ack()
		}
	case msg.Op == voiceOpResumed:
		slog.Debug("Voice RESUME succeeded.")
	case msg.Op == voiceOpSessionDescription:
		mode, _ := data["mode"].(string)
		state.setMode(mode)
		if err := ws.loadSecretKey(data); err != nil {
			return err
		}
		pv, err := intField("dave_protocol_version")
		if err != nil {
			return err
		}
		state.setDaveProtocolVersion(pv)
		if pv > 0 {
			if err := state.reinitDaveSession(); err != nil {
				return err
			}
		}
	case msg.Op == voiceOpHello:
		interval, err := intField("heartbeat_interval")
		if err != nil {
			return err
		}
		ka := newVoiceKeepAlive(ws, min(time.Duration(interval)*time.Millisecond, 5*time.Second))
		ws.mu.Lock()
		ws.keepAlive = ka
		ws.mu.Unlock()
		go ka.run()
	case msg.Op == voiceOpClientsConnect:
		var ids []string
		raw, _ := data["user_ids"].([]any)
		for _, v := range raw {
			if id, ok := voiceInt(v); ok {
				ids = append(ids, strconv.FormatInt(id, 10))
			}
		}
		state.recognizeUsers(ids)
	case msg.Op == voiceOpClientDisconnect:
		if id, ok := voiceInt(data["user_id"]); ok {
			state.forgetUser(strconv.FormatInt(id, 10))
		}
	case state.daveSessionValue() != nil:
		if err := ws.receivedDaveMessage(msg.Op, data, intField); err != nil {
			return err
		}
	}
	if hook := state.hookValue(); hook != nil {
		hook(ctx, msg)
	}
	return nil
}

// Handles the DAVE transition and epoch opcodes of an active session
func (ws *voiceWebSocket) receivedDaveMessage(op int, data map[string]any, intField func(string) (int, error)) error {
	state := ws.state
	switch op {
	case voiceOpDavePrepareTransition:
		tid, err := intField("transition_id")
		if err != nil {
			return err
		}
		pv, err := intField("protocol_version")
		if err != nil {
			return err
		}
		slog.Debug(fmt.Sprintf("Preparing for DAVE transition id %d for protocol version %d", tid, pv))
		state.addPendingTransition(tid, pv)
		if dave := state.daveSessionValue(); dave != nil {
			dave.prepareTransition(tid, pv)
		}
		if tid == 0 {
			state.executeTransition(tid)
			return nil
		}
		if dave := state.daveSessionValue(); pv == 0 && dave != nil {
			dave.setPassthroughMode(true, 120)
		}
		return ws.sendTransitionReady(tid)
	case voiceOpDaveExecuteTransition:
		tid, err := intField("transition_id")
		if err != nil {
			return err
		}
		slog.Debug(fmt.Sprintf("Executing DAVE transition id %d", tid))
		state.executeTransition(tid)
	case voiceOpDavePrepareEpoch:
		epoch, err := intField("epoch")
		if err != nil {
			return err
		}
		slog.Debug(fmt.Sprintf("Preparing for DAVE epoch %d", epoch))
		if epoch == 1 {
			pv, err := intField("protocol_version")
			if err != nil {
				return err
			}
			state.setDaveProtocolVersion(pv)
			return state.reinitDaveSession()
		}
	}
	return nil
}

// Handles one binary frame of the MLS exchange
func (ws *voiceWebSocket) receivedBinaryMessage(msg []byte) error {
	if len(msg) < 3 {
		return fmt.Errorf("struct.error: unpack_from requires a buffer of at least 3 bytes")
	}
	seq := int(binary.BigEndian.Uint16(msg))
	ws.mu.Lock()
	ws.seqAck = seq
	ws.mu.Unlock()
	op := int(msg[2])
	slog.Debug(fmt.Sprintf("Voice websocket binary frame received: %d bytes; seq=%d op=%d", len(msg), seq, op))
	state := ws.state
	dave := state.daveSessionValue()
	if dave == nil {
		return nil
	}
	switch op {
	case voiceOpMLSExternalSender:
		dave.setExternalSender(msg[3:])
		slog.Debug("Set MLS external sender")
	case voiceOpMLSProposals:
		if len(msg) < 4 {
			return fmt.Errorf("IndexError: index out of range")
		}
		if commitWelcome := dave.processProposals(msg[3:]); len(commitWelcome) > 0 {
			if err := ws.sendBinary(voiceOpMLSCommitWelcome, commitWelcome); err != nil {
				return err
			}
		}
		slog.Debug("MLS proposals processed")
	case voiceOpMLSAnnounceCommitTransition, voiceOpMLSWelcome:
		if len(msg) < 5 {
			return fmt.Errorf("struct.error: unpack_from requires a buffer of at least 5 bytes")
		}
		tid := int(binary.BigEndian.Uint16(msg[3:]))
		var err error
		what := "commit"
		if op == voiceOpMLSWelcome {
			what = "welcome"
			err = dave.processWelcome(msg[5:])
		} else {
			err = dave.processCommit(msg[5:])
		}
		if err != nil {
			return state.recoverFromInvalidCommit(tid)
		}
		dave.prepareTransition(tid, state.daveProtocolVersionValue())
		if tid != 0 {
			state.addPendingTransition(tid, state.daveProtocolVersionValue())
			if err := ws.sendTransitionReady(tid); err != nil {
				return err
			}
		}
		slog.Debug(fmt.Sprintf("MLS %s processed for transition id %d", what, tid))
	}
	return nil
}

// Stores the session's ssrc and voice server address, then runs IP discovery and selects the transport mode
func (ws *voiceWebSocket) initialConnection(ctx context.Context, data map[string]any) error {
	state := ws.state
	ssrc, ok := voiceInt(data["ssrc"])
	if !ok {
		return fmt.Errorf("KeyError: 'ssrc'")
	}
	port, ok := voiceInt(data["port"])
	if !ok {
		return fmt.Errorf("KeyError: 'port'")
	}
	ip, _ := data["ip"].(string)
	state.setReady(uint32(ssrc), int(port), ip)
	slog.Debug("Connecting to voice socket")
	if err := state.connectSocket(); err != nil {
		return err
	}
	extIP, extPort, err := ws.discoverIP(ctx)
	if err != nil {
		return err
	}
	state.setDiscovered(extIP, extPort)
	var offered, supported []string
	raw, _ := data["modes"].([]any)
	for _, m := range raw {
		if s, ok := m.(string); ok {
			offered = append(offered, s)
			if slices.Contains(voiceSupportedModes, s) {
				supported = append(supported, s)
			}
		}
	}
	slog.Debug(fmt.Sprintf("received supported encryption modes: %s", strings.Join(supported, ", ")))
	mode, err := selectVoiceMode(offered)
	if err != nil {
		return err
	}
	if err := ws.selectProtocol(extIP, extPort, mode); err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("selected the voice protocol for use (%s)", mode))
	return nil
}

// Sends the discovery packet and waits for the server to echo the external address
func (ws *voiceWebSocket) discoverIP(ctx context.Context) (string, int, error) {
	state := ws.state
	got := make(chan []byte, 1)
	id := state.addSocketListener(func(data []byte) {
		if len(data) == 74 && data[1] == 0x02 {
			select {
			case got <- data:
			default:
			}
		}
	})
	defer state.removeSocketListener(id)
	slog.Debug("Sending ip discovery packet")
	if err := state.sendPacket(ipDiscoveryRequest(state.ssrcValue())); err != nil {
		return "", 0, err
	}
	select {
	case recv := <-got:
		slog.Debug(fmt.Sprintf("Received ip discovery packet: %v", recv))
		ip, port, err := parseIPDiscovery(recv)
		if err == nil {
			slog.Debug(fmt.Sprintf("detected ip: %s port: %d", ip, port))
		}
		return ip, port, err
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
}

// Stores the session secret and announces the bot as not speaking
func (ws *voiceWebSocket) loadSecretKey(data map[string]any) error {
	slog.Debug("received secret key for voice connection")
	raw, ok := data["secret_key"].([]any)
	if !ok {
		return fmt.Errorf("KeyError: 'secret_key'")
	}
	key := make([]byte, len(raw))
	for i, v := range raw {
		n, _ := voiceInt(v)
		key[i] = byte(n)
	}
	ws.mu.Lock()
	ws.secretKey = key
	ws.mu.Unlock()
	ws.state.setSecretKey(key)
	return ws.speak(speakingNone)
}

// Reads and handles one frame within 30 seconds, a close or read failure surfacing as the close code
func (ws *voiceWebSocket) pollEvent(ctx context.Context) error {
	ws.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	kind, data, err := ws.conn.ReadMessage()
	if err != nil {
		ws.mu.Lock()
		code := ws.closeCode
		ws.mu.Unlock()
		var ne net.Error
		var ce *websocket.CloseError
		switch {
		case code != 0:
		case errors.As(err, &ce):
			code = ce.Code
		case errors.As(err, &ne) && ne.Timeout():
			return errVoiceReadTimeout
		default:
			code = 1006
		}
		slog.Debug(fmt.Sprintf("Received voice %s", err))
		return &voiceConnectionClosed{code: code}
	}
	switch kind {
	case websocket.TextMessage:
		var msg voiceGatewayMessage
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil {
			return err
		}
		return ws.receivedMessage(ctx, msg)
	case websocket.BinaryMessage:
		return ws.receivedBinaryMessage(data)
	}
	return nil
}

// Stops the heartbeat and closes the socket with a code
func (ws *voiceWebSocket) close(code int) error {
	ws.mu.Lock()
	ka := ws.keepAlive
	ws.closeCode = code
	ws.mu.Unlock()
	if ka != nil {
		ka.stop()
	}
	ws.writeMu.Lock()
	ws.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(5*time.Second))
	ws.writeMu.Unlock()
	return ws.conn.Close()
}

// Heartbeat loop of a voice gateway session, closing the socket after 60 seconds without an ack
type voiceKeepAlive struct {
	ws               *voiceWebSocket
	interval         time.Duration
	heartbeatTimeout time.Duration
	stopCh           chan struct{}
	stopOnce         sync.Once
	mu               sync.Mutex
	lastRecv         time.Time
}

func newVoiceKeepAlive(ws *voiceWebSocket, interval time.Duration) *voiceKeepAlive {
	return &voiceKeepAlive{ws: ws, interval: interval, heartbeatTimeout: 60 * time.Second, stopCh: make(chan struct{}), lastRecv: time.Now()}
}

// Sends a heartbeat every interval until stopped or until acks stop arriving
func (k *voiceKeepAlive) run() {
	t := time.NewTicker(k.interval)
	defer t.Stop()
	for {
		select {
		case <-k.stopCh:
			return
		case <-t.C:
		}
		k.mu.Lock()
		silent := time.Since(k.lastRecv) > k.heartbeatTimeout
		k.mu.Unlock()
		if silent {
			slog.Warn("Shard ID None has stopped responding to the gateway. Closing and restarting.")
			if err := k.ws.close(4000); err != nil {
				slog.Error("An error occurred while stopping the gateway. Ignoring.")
			}
			k.stop()
			return
		}
		ts := time.Now().UnixMilli()
		seq := k.ws.sequenceAck()
		slog.Debug(fmt.Sprintf("Keeping shard ID None voice websocket alive with timestamp %v.", map[string]any{"t": ts, "seq_ack": seq}))
		if err := k.ws.sendAsJSON(map[string]any{"op": voiceOpHeartbeat, "d": map[string]any{"t": ts, "seq_ack": seq}}); err != nil {
			k.stop()
			return
		}
	}
}

func (k *voiceKeepAlive) stop() { k.stopOnce.Do(func() { close(k.stopCh) }) }

// Records a heartbeat ack as the last sign of life
func (k *voiceKeepAlive) ack() {
	k.mu.Lock()
	k.lastRecv = time.Now()
	k.mu.Unlock()
}
