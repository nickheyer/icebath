package discord

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Receiver tuning of hermes's VoiceReceiver
const (
	voiceSilenceThreshold    = 1500 * time.Millisecond
	voiceMinSpeechDuration   = 0.5
	voiceReceiverSampleRate  = 48000
	voiceReceiverChannels    = 2
	voiceRekeyFailureStreak  = 25
	voicePassthroughDowngrad = 120.0
	voicePassthroughUpgrade  = 10.0
)

// One consistent decryption generation, swapped whole so a packet never pairs a new key with an old session
type voiceCreds struct {
	secretKey           []byte
	mode                string
	dave                voiceDaveSession
	daveProtocolVersion int
	daveDowngraded      bool
}

// Captures channel audio, decrypting transport and DAVE layers and decoding Opus per user until silence ends an utterance
type voiceReceiver struct {
	vc             *voiceClient
	allowedUserIDs strSet
	running        atomic.Bool
	paused         atomic.Bool
	creds          atomic.Pointer[voiceCreds]
	botSSRC        atomic.Uint32

	passthroughMu        sync.Mutex
	davePassthroughUntil time.Time

	mu             sync.Mutex
	ssrcToUser     map[uint32]int64
	buffers        map[uint32][]byte
	lastPacketTime map[uint32]time.Time
	decoders       map[uint32]voiceOpusDecoder

	packetDebugCount    int
	decodeOK            int
	decodeFailed        int
	daveUnmappedDropped int
	decryptFailStreak   int
	listenerID          int
}

func newVoiceReceiver(vc *voiceClient, allowedUserIDs strSet) *voiceReceiver {
	if allowedUserIDs == nil {
		allowedUserIDs = strSet{}
	}
	r := &voiceReceiver{vc: vc, allowedUserIDs: allowedUserIDs, ssrcToUser: map[uint32]int64{}, buffers: map[uint32][]byte{},
		lastPacketTime: map[uint32]time.Time{}, decoders: map[uint32]voiceOpusDecoder{}}
	r.creds.Store(&voiceCreds{})
	return r
}

// Resolves credentials, hooks SPEAKING events and starts taking packets from the socket reader
func (r *voiceReceiver) start() error {
	conn := r.vc.conn
	if err := r.resolveCredentials(conn); err != nil {
		return err
	}
	r.installSpeakingHook(conn)
	r.listenerID = conn.addSocketListener(r.onPacket)
	r.running.Store(true)
	slog.Info(fmt.Sprintf("VoiceReceiver started (bot_ssrc=%d)", r.botSSRC.Load()))
	return nil
}

// Reads the live decryption state, seeding the passthrough window when a downgrade is already pending
func (r *voiceReceiver) resolveCredentials(conn *voiceConnState) error {
	conn.mu.Lock()
	if conn.secretKey == nil {
		conn.mu.Unlock()
		return fmt.Errorf("TypeError: cannot convert 'MissingSentinel' object to bytes")
	}
	c := &voiceCreds{secretKey: append([]byte(nil), conn.secretKey...), mode: conn.mode,
		daveProtocolVersion: conn.daveProtocolVersion, daveDowngraded: conn.daveDowngraded}
	if conn.dave != nil {
		c.dave = conn.dave
	}
	ssrc := conn.ssrc
	pendingDowngrade := false
	for _, pv := range conn.davePendingTransitions {
		if pv == 0 {
			pendingDowngrade = true
		}
	}
	conn.mu.Unlock()
	r.creds.Store(c)
	r.botSSRC.Store(ssrc)
	r.passthroughMu.Lock()
	expired := !time.Now().Before(r.davePassthroughUntil)
	r.passthroughMu.Unlock()
	if pendingDowngrade && expired {
		r.noteDavePassthroughWindow(voicePassthroughDowngrad)
	}
	return nil
}

func (r *voiceReceiver) daveSession() voiceDaveSession { return r.creds.Load().dave }

func (r *voiceReceiver) daveProtocolVersion() int { return r.creds.Load().daveProtocolVersion }

func (r *voiceReceiver) daveDowngraded() bool { return r.creds.Load().daveDowngraded }

// Opens a plaintext grace window for unmapped SSRCs, each grant replacing the previous deadline
func (r *voiceReceiver) noteDavePassthroughWindow(seconds float64) {
	r.passthroughMu.Lock()
	r.davePassthroughUntil = time.Now().Add(time.Duration(seconds * float64(time.Second)))
	r.passthroughMu.Unlock()
}

// Re-reads decryption state from the live connection and resets the failure streak
func (r *voiceReceiver) RefreshCredentials(reason string) {
	if !r.running.Load() {
		return
	}
	if err := r.resolveCredentials(r.vc.conn); err != nil {
		slog.Warn(fmt.Sprintf("VoiceReceiver credential refresh failed (%s): %s", reason, err))
		return
	}
	r.mu.Lock()
	r.decryptFailStreak = 0
	r.mu.Unlock()
	dave := "off"
	if r.daveSession() != nil {
		dave = "on"
	}
	slog.Info(fmt.Sprintf("VoiceReceiver credentials refreshed (%s; bot_ssrc=%d, dave=%s)", reason, r.botSSRC.Load(), dave))
}

// Stops taking packets, clears every per SSRC state and logs the decode health counters
func (r *voiceReceiver) Stop() {
	r.running.Store(false)
	r.vc.conn.removeSocketListener(r.listenerID)
	r.mu.Lock()
	clear(r.buffers)
	clear(r.lastPacketTime)
	clear(r.decoders)
	clear(r.ssrcToUser)
	ok, failed, dropped := r.decodeOK, r.decodeFailed, r.daveUnmappedDropped
	r.mu.Unlock()
	slog.Info(fmt.Sprintf("VoiceReceiver stopped (frames ok=%d, decrypt_failed=%d, dave_unmapped_dropped=%d)", ok, failed, dropped))
}

// Ignores packets while the bot plays audio
func (r *voiceReceiver) Pause() { r.paused.Store(true) }

func (r *voiceReceiver) Resume() { r.paused.Store(false) }

// Maps an SSRC to the user speaking on it
func (r *voiceReceiver) mapSSRC(ssrc uint32, userID int64) {
	r.mu.Lock()
	r.ssrcToUser[ssrc] = userID
	r.mu.Unlock()
}

// Wraps the connection hook to map SPEAKING events and refresh credentials after the events that change them
func (r *voiceReceiver) installSpeakingHook(conn *voiceConnState) {
	conn.mu.Lock()
	original := conn.hook
	wrapped := func(ctx context.Context, msg voiceGatewayMessage) {
		if msg.Op == voiceOpSpeaking {
			ssrc, okS := voiceInt(msg.D["ssrc"])
			userID, okU := voiceInt(msg.D["user_id"])
			if okS && okU && ssrc != 0 && userID != 0 {
				slog.Info(fmt.Sprintf("SPEAKING event: ssrc=%d -> user=%d", ssrc, userID))
				r.mapSSRC(uint32(ssrc), userID)
			}
		}
		if original != nil {
			original(ctx, msg)
		}
		switch msg.Op {
		case voiceOpSessionDescription, voiceOpDavePrepareEpoch:
			r.RefreshCredentials(fmt.Sprintf("voice ws op %d", msg.Op))
		case voiceOpDavePrepareTransition:
			if pv, ok := voiceInt(msg.D["protocol_version"]); ok && pv == 0 {
				r.noteDavePassthroughWindow(voicePassthroughDowngrad)
			}
		case voiceOpDaveExecuteTransition:
			wasDowngraded := r.daveDowngraded()
			r.RefreshCredentials("voice ws op 22")
			if wasDowngraded && !r.daveDowngraded() && r.daveProtocolVersion() > 0 {
				r.noteDavePassthroughWindow(voicePassthroughUpgrade)
			}
		}
	}
	conn.hook = wrapped
	live := conn.ws != nil
	conn.mu.Unlock()
	if live {
		slog.Info("Speaking hook installed on live websocket")
	}
}

// Decrypts, strips and decodes one RTP packet into the speaking user's PCM buffer
func (r *voiceReceiver) onPacket(data []byte) {
	if !r.running.Load() || r.paused.Load() {
		return
	}
	creds := r.creds.Load()
	r.mu.Lock()
	r.packetDebugCount++
	debugCount := r.packetDebugCount
	r.mu.Unlock()
	if debugCount <= 5 {
		first := "short"
		if len(data) >= 4 {
			first = hex.EncodeToString(data[:4])
		}
		slog.Debug(fmt.Sprintf("Raw UDP packet: len=%d, first_bytes=%s", len(data), first))
	}
	if len(data) < 16 {
		return
	}
	if data[0]>>6 != 2 || data[1]&0x7f != 0x78 {
		if debugCount <= 5 {
			slog.Debug(fmt.Sprintf("Skipped non-RTP: byte0=0x%02x byte1=0x%02x", data[0], data[1]))
		}
		return
	}
	firstByte := data[0]
	seq := binary.BigEndian.Uint16(data[2:])
	ssrc := binary.BigEndian.Uint32(data[8:])
	if ssrc == r.botSSRC.Load() {
		return
	}
	cc := int(firstByte & 0x0f)
	hasExtension := firstByte&0x10 != 0
	hasPadding := firstByte&0x20 != 0
	headerSize := 12 + 4*cc
	if hasExtension {
		headerSize += 4
	}
	if len(data) < headerSize+4 {
		return
	}
	extDataLen := 0
	if hasExtension {
		off := 12 + 4*cc
		extDataLen = int(binary.BigEndian.Uint16(data[off+2:])) * 4
	}
	if debugCount <= 10 {
		r.mu.Lock()
		known := "unknown"
		if uid, ok := r.ssrcToUser[ssrc]; ok {
			known = strconv.FormatInt(uid, 10)
		}
		r.mu.Unlock()
		slog.Debug(fmt.Sprintf("RTP packet: ssrc=%d, seq=%d, user=%s, hdr=%d, ext_data=%d", ssrc, seq, known, headerSize, extDataLen))
	}
	header := data[:headerSize]
	payloadWithNonce := data[headerSize:]
	encrypted := payloadWithNonce[:len(payloadWithNonce)-4]
	decrypted, err := func() ([]byte, error) {
		aead, err := transportAEAD(creds.mode, creds.secretKey)
		if err != nil {
			return nil, err
		}
		return aead.Open(nil, rtpsizeNonce(aead, payloadWithNonce[len(payloadWithNonce)-4:]), encrypted, header)
	}()
	if err != nil {
		r.mu.Lock()
		r.decodeFailed++
		r.decryptFailStreak++
		streak := r.decryptFailStreak
		r.mu.Unlock()
		if debugCount <= 10 || streak%250 == 0 {
			slog.Warn(fmt.Sprintf("Voice transport decrypt failed: %s (hdr=%d, enc=%d, streak=%d)", err, headerSize, len(encrypted), streak))
		}
		if streak >= voiceRekeyFailureStreak {
			r.RefreshCredentials("decrypt-failure streak")
		}
		return
	}
	r.mu.Lock()
	r.decryptFailStreak = 0
	r.mu.Unlock()
	if extDataLen != 0 && len(decrypted) > extDataLen {
		decrypted = decrypted[extDataLen:]
	}
	if hasPadding {
		if len(decrypted) == 0 {
			if debugCount <= 10 {
				slog.Warn(fmt.Sprintf("RTP padding bit set but no payload (ssrc=%d)", ssrc))
			}
			return
		}
		padLen := int(decrypted[len(decrypted)-1])
		if padLen == 0 || padLen > len(decrypted) {
			if debugCount <= 10 {
				slog.Warn(fmt.Sprintf("Invalid RTP padding length %d for payload size %d (ssrc=%d)", padLen, len(decrypted), ssrc))
			}
			return
		}
		decrypted = decrypted[:len(decrypted)-padLen]
		if len(decrypted) == 0 {
			return
		}
	}
	if creds.dave != nil {
		r.mu.Lock()
		userID := r.ssrcToUser[ssrc]
		if userID == 0 {
			userID = r.inferUserForSSRCLocked(ssrc)
		}
		r.mu.Unlock()
		if userID != 0 {
			plain, err := creds.dave.decrypt(userID, daveMediaAudio, decrypted)
			if err != nil {
				if !strings.Contains(err.Error(), "Unencrypted") {
					if debugCount <= 10 {
						slog.Warn(fmt.Sprintf("DAVE decrypt failed for ssrc=%d: %s", ssrc, err))
					}
					return
				}
			} else {
				decrypted = plain
			}
		} else {
			r.passthroughMu.Lock()
			closed := !time.Now().Before(r.davePassthroughUntil)
			r.passthroughMu.Unlock()
			if creds.daveProtocolVersion > 0 && closed {
				r.mu.Lock()
				r.daveUnmappedDropped++
				dropped := r.daveUnmappedDropped
				r.mu.Unlock()
				if debugCount <= 10 || dropped%250 == 1 {
					slog.Debug(fmt.Sprintf("Dropping DAVE frame for unmapped ssrc=%d (dropped=%d)", ssrc, dropped))
				}
				return
			}
		}
	}
	r.mu.Lock()
	dec, ok := r.decoders[ssrc]
	r.mu.Unlock()
	if !ok {
		var err error
		if dec, err = newOpusDecoder(); err != nil {
			slog.Debug(fmt.Sprintf("Opus decode error for SSRC %d; reset decoder: %s", ssrc, err))
			return
		}
		r.mu.Lock()
		r.decoders[ssrc] = dec
		r.mu.Unlock()
	}
	pcm, err := dec.decode(decrypted)
	if err != nil {
		r.mu.Lock()
		delete(r.decoders, ssrc)
		r.mu.Unlock()
		slog.Debug(fmt.Sprintf("Opus decode error for SSRC %d; reset decoder: %s", ssrc, err))
		return
	}
	r.mu.Lock()
	r.decodeOK++
	r.buffers[ssrc] = append(r.buffers[ssrc], pcm...)
	r.lastPacketTime[ssrc] = time.Now()
	r.mu.Unlock()
}

// Maps an unmapped SSRC to the sole allowed member of the channel after a rejoin that lost SPEAKING
func (r *voiceReceiver) inferUserForSSRCLocked(ssrc uint32) int64 {
	members, ok := r.vc.channelMembers()
	if !ok {
		return 0
	}
	var candidates []int64
	for _, m := range members {
		if m.User == nil {
			continue
		}
		id, err := strconv.ParseInt(m.User.ID, 10, 64)
		if err != nil || id == r.vc.userID {
			continue
		}
		if len(r.allowedUserIDs) == 0 || r.allowedUserIDs.has(m.User.ID) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) != 1 {
		return 0
	}
	r.ssrcToUser[ssrc] = candidates[0]
	slog.Info(fmt.Sprintf("Auto-mapped ssrc=%d -> user=%d (sole allowed member)", ssrc, candidates[0]))
	return candidates[0]
}

// Seconds of 48 kHz stereo 16 bit PCM a buffer holds
func pcmSeconds(buf []byte) float64 {
	return float64(len(buf)) / float64(voiceReceiverSampleRate*voiceReceiverChannels*2)
}

// Utterances whose speaker fell silent long enough, discarding stale buffers no user claims
func (r *voiceReceiver) CheckSilence() []voiceUtterance {
	now := time.Now()
	var completed []voiceUtterance
	r.mu.Lock()
	defer r.mu.Unlock()
	users := make(map[uint32]int64, len(r.ssrcToUser))
	for k, v := range r.ssrcToUser {
		users[k] = v
	}
	for ssrc, buf := range r.buffers {
		last, ok := r.lastPacketTime[ssrc]
		if !ok {
			last = now
		}
		silence := now.Sub(last)
		if silence >= voiceSilenceThreshold && pcmSeconds(buf) >= voiceMinSpeechDuration {
			userID := users[ssrc]
			if userID == 0 {
				userID = r.inferUserForSSRCLocked(ssrc)
			}
			if userID != 0 {
				completed = append(completed, voiceUtterance{userID: userID, pcm: append([]byte(nil), buf...)})
			}
			r.buffers[ssrc] = []byte{}
			delete(r.lastPacketTime, ssrc)
		} else if silence >= 2*voiceSilenceThreshold {
			delete(r.buffers, ssrc)
			delete(r.lastPacketTime, ssrc)
		}
	}
	return completed
}

// Buffered utterances that have not reached silence yet, emptying every buffer
func (r *voiceReceiver) FlushPending() []voiceUtterance {
	var completed []voiceUtterance
	r.mu.Lock()
	defer r.mu.Unlock()
	users := make(map[uint32]int64, len(r.ssrcToUser))
	for k, v := range r.ssrcToUser {
		users[k] = v
	}
	for ssrc, buf := range r.buffers {
		if pcmSeconds(buf) >= voiceMinSpeechDuration {
			userID := users[ssrc]
			if userID == 0 {
				userID = r.inferUserForSSRCLocked(ssrc)
			}
			if userID != 0 {
				completed = append(completed, voiceUtterance{userID: userID, pcm: append([]byte(nil), buf...)})
			}
		}
		delete(r.buffers, ssrc)
		delete(r.lastPacketTime, ssrc)
	}
	return completed
}

// Drops buffered PCM no poll or flush has emitted yet
func (r *voiceReceiver) DiscardPending() {
	r.mu.Lock()
	clear(r.buffers)
	clear(r.lastPacketTime)
	r.mu.Unlock()
}

// Whether the receiver still takes packets
func (r *voiceReceiver) Running() bool { return r.running.Load() }

// Mapped users whose last packet arrived within the window
func (r *voiceReceiver) SpeakingUserIDs(window time.Duration) map[int64]bool {
	now := time.Now()
	out := map[int64]bool{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ssrc, last := range r.lastPacketTime {
		if now.Sub(last) < window {
			if uid := r.ssrcToUser[ssrc]; uid != 0 {
				out[uid] = true
			}
		}
	}
	return out
}
