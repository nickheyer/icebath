package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

// Voice connection of the bot in one guild, discord.py's VoiceClient on this package's own voice transport
type voiceClient struct {
	session   *discordgo.Session
	ctx       context.Context
	cancel    context.CancelFunc
	guildID   int64
	userID    int64
	conn      *voiceConnState
	onCleanup func()

	mu        sync.Mutex
	channelID int64
	sequence  int
	timestamp int64
	incrNonce int64
	encoder   *opusEncoder
	player    *audioPlayer
	handlers  []func()
	cleaned   bool
}

// discord.py ClientException raised by voice calls in the wrong state
type voiceClientException struct{ msg string }

func (e *voiceClientException) Error() string { return e.msg }

// Voice client of a guild channel whose gateway events route to its connection until cleanup
func newVoiceClient(session *discordgo.Session, guildID, channelID, userID int64, onCleanup func()) *voiceClient {
	ctx, cancel := context.WithCancel(context.Background())
	vc := &voiceClient{session: session, ctx: ctx, cancel: cancel, guildID: guildID, channelID: channelID, userID: userID, onCleanup: onCleanup}
	vc.conn = newVoiceConnState(vc)
	guild := strconv.FormatInt(guildID, 10)
	self := strconv.FormatInt(userID, 10)
	vc.handlers = append(vc.handlers,
		session.AddHandler(func(_ *discordgo.Session, v *discordgo.VoiceStateUpdate) {
			if v.VoiceState != nil && v.GuildID == guild && v.UserID == self {
				vc.conn.voiceStateUpdate(vc.ctx, v.ChannelID, v.SessionID)
			}
		}),
		session.AddHandler(func(_ *discordgo.Session, v *discordgo.VoiceServerUpdate) {
			if v.GuildID == guild {
				vc.conn.voiceServerUpdate(vc.ctx, v.Token, v.GuildID, v.Endpoint)
			}
		}))
	return vc
}

func (vc *voiceClient) baseContext() context.Context { return vc.ctx }

func (vc *voiceClient) dialer() *websocket.Dialer { return vc.session.Dialer }

func (vc *voiceClient) channelIDValue() int64 {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.channelID
}

// Follows the channel the gateway reports the bot in
func (vc *voiceClient) updateVoiceChannel(channelID int64) {
	vc.mu.Lock()
	vc.channelID = channelID
	vc.mu.Unlock()
}

// Text id of the connected voice channel
func (vc *voiceClient) ChannelID() string {
	if id := vc.channelIDValue(); id != 0 {
		return strconv.FormatInt(id, 10)
	}
	return ""
}

// Cached voice channel of the connection, nil when absent from the client cache
func (vc *voiceClient) Channel() *discordgo.Channel {
	id := vc.channelIDValue()
	if id == 0 || vc.session.State == nil {
		return nil
	}
	ch, err := vc.session.State.Channel(strconv.FormatInt(id, 10))
	if err != nil {
		return nil
	}
	return ch
}

// Members the client cache holds in the connected voice channel, ok false without a cached channel
func (vc *voiceClient) channelMembers() ([]*discordgo.Member, bool) {
	ch := vc.Channel()
	if ch == nil {
		return nil, false
	}
	g, err := vc.session.State.Guild(strconv.FormatInt(vc.guildID, 10))
	if err != nil {
		return nil, true
	}
	vc.session.State.RLock()
	defer vc.session.State.RUnlock()
	var out []*discordgo.Member
	for _, vs := range g.VoiceStates {
		if vs.ChannelID != ch.ID {
			continue
		}
		m := vs.Member
		for _, gm := range g.Members {
			if gm.User != nil && gm.User.ID == vs.UserID {
				m = gm
				break
			}
		}
		if m == nil || m.User == nil {
			continue
		}
		out = append(out, m)
	}
	return out, true
}

// Cached members in the connected voice channel, empty without a cached channel
func (vc *voiceClient) ChannelMembers() []*discordgo.Member {
	members, _ := vc.channelMembers()
	return members
}

// Sends one raw datagram on the voice socket
func (vc *voiceClient) SendPacket(packet []byte) error { return vc.conn.sendPacket(packet) }

// Live voice state of the bot in the guild, nil when not cached
func (vc *voiceClient) selfVoiceState() *discordgo.VoiceState {
	if vc.session.State == nil {
		return nil
	}
	vs, err := vc.session.State.VoiceState(strconv.FormatInt(vc.guildID, 10), strconv.FormatInt(vc.userID, 10))
	if err != nil {
		return nil
	}
	return vs
}

// Sends the main gateway voice state update joining a channel, or leaving voice for channel zero
func (vc *voiceClient) changeVoiceState(channelID int64, selfDeaf, selfMute bool) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("ConnectionClosed: gateway is not connected (%v)", p)
		}
	}()
	cid := ""
	if channelID != 0 {
		cid = strconv.FormatInt(channelID, 10)
	}
	return vc.session.ChannelVoiceJoinManual(strconv.FormatInt(vc.guildID, 10), cid, selfMute, selfDeaf)
}

// Connects with discord.py's join defaults, raising the connect error
func (vc *voiceClient) connect(ctx context.Context, timeout time.Duration, reconnect, selfDeaf, selfMute bool) error {
	return vc.conn.connect(ctx, reconnect, timeout, selfDeaf, selfMute, false, true)
}

func (vc *voiceClient) IsConnected() bool { return vc.conn.isConnected() }

// Waits up to timeout for the connection to come back
func (vc *voiceClient) waitUntilConnected(timeout time.Duration) bool {
	vc.conn.wait(timeout)
	return vc.conn.isConnected()
}

// Moves the bot to another voice channel of the guild
func (vc *voiceClient) MoveTo(ctx context.Context, channelID int64) {
	vc.conn.moveTo(ctx, channelID, 30*time.Second)
}

// Stops playback, leaves voice waiting for the gateway confirmation and drops the client
func (vc *voiceClient) Disconnect(ctx context.Context) error {
	vc.disconnect(ctx, false)
	return nil
}

// Disconnect that also tears down a connection which never reached the connected state when forced
func (vc *voiceClient) disconnect(ctx context.Context, force bool) {
	vc.Stop()
	vc.conn.disconnect(ctx, force, true, true)
	vc.cleanup()
}

// Removes the client from the adapter registry and the gateway event routing, once
func (vc *voiceClient) cleanup() {
	vc.mu.Lock()
	if vc.cleaned {
		vc.mu.Unlock()
		return
	}
	vc.cleaned = true
	handlers := vc.handlers
	vc.handlers = nil
	vc.mu.Unlock()
	for _, remove := range handlers {
		remove()
	}
	if vc.onCleanup != nil {
		vc.onCleanup()
	}
	vc.cancel()
}

// Starts playing a source, encoding PCM sources with discord.py's default Opus settings
func (vc *voiceClient) Play(source audioSource, after func(error)) error {
	if !vc.IsConnected() {
		return &voiceClientException{msg: "Not connected to voice."}
	}
	if vc.IsPlaying() {
		return &voiceClientException{msg: "Already playing audio."}
	}
	if !source.isOpus() {
		enc, err := newOpusEncoder()
		if err != nil {
			return err
		}
		vc.mu.Lock()
		vc.encoder = enc
		vc.mu.Unlock()
	}
	p := newAudioPlayer(source, vc, after)
	vc.mu.Lock()
	vc.player = p
	vc.mu.Unlock()
	go p.run()
	return nil
}

func (vc *voiceClient) IsPlaying() bool {
	vc.mu.Lock()
	p := vc.player
	vc.mu.Unlock()
	return p != nil && p.isPlaying()
}

// Stops the current player
func (vc *voiceClient) Stop() { vc.stopPlayer() }

func (vc *voiceClient) stopPlayer() {
	vc.mu.Lock()
	p := vc.player
	vc.player = nil
	vc.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

// Adds value to a counter, wrapping to zero past the limit like discord.py's checked_add
func checkedAdd(val, value, limit int64) int64 {
	if val+value > limit {
		return 0
	}
	return val + value
}

// Encodes when asked, encrypts and sends one audio frame, dropping it when the socket refuses
func (vc *voiceClient) sendAudioPacket(data []byte, encode bool) error {
	vc.mu.Lock()
	vc.sequence = int(checkedAdd(int64(vc.sequence), 1, 65535))
	enc := vc.encoder
	vc.mu.Unlock()
	encoded := data
	if encode {
		if enc == nil {
			return &opusNotLoaded{}
		}
		var err error
		if encoded, err = enc.encode(data, opusSamplesPerFrame); err != nil {
			return err
		}
	}
	packet, err := vc.voicePacket(encoded)
	if err != nil {
		return err
	}
	if err := vc.conn.sendPacket(packet); err != nil {
		vc.mu.Lock()
		slog.Debug(fmt.Sprintf("A packet has been dropped (seq: %d, timestamp: %d)", vc.sequence, vc.timestamp))
		vc.mu.Unlock()
	}
	vc.mu.Lock()
	vc.timestamp = checkedAdd(vc.timestamp, opusSamplesPerFrame, 4294967295)
	vc.mu.Unlock()
	return nil
}

// RTP packet of an Opus frame, end to end encrypted when DAVE can encrypt, then sealed with the transport mode
func (vc *voiceClient) voicePacket(data []byte) ([]byte, error) {
	st := vc.conn
	st.mu.Lock()
	dave, ssrc, mode, key := st.dave, st.ssrc, st.mode, st.secretKey
	st.mu.Unlock()
	packet := data
	if dave != nil && st.canEncrypt() {
		var err error
		if packet, err = dave.encryptOpus(ssrc, data); err != nil {
			return nil, err
		}
	}
	vc.mu.Lock()
	header := rtpHeader(uint16(vc.sequence), uint32(vc.timestamp), ssrc)
	nonce := uint32(vc.incrNonce)
	vc.incrNonce = checkedAdd(vc.incrNonce, 1, 4294967295)
	vc.mu.Unlock()
	aead, err := transportAEAD(mode, key)
	if err != nil {
		return nil, err
	}
	return encryptRTPSize(aead, header, packet, nonce), nil
}

// Audio stream a player drains in 20 millisecond frames
type audioSource interface {
	read() ([]byte, error)
	isOpus() bool
	cleanup()
}

// Thread sending a source's frames at real time pace, discord.py's AudioPlayer
type audioPlayer struct {
	source  audioSource
	client  *voiceClient
	after   func(error)
	end     chan struct{}
	endOnce sync.Once
	mu      sync.Mutex
	err     error
}

const audioPlayerDelay = opusFrameLength * time.Millisecond

func newAudioPlayer(source audioSource, client *voiceClient, after func(error)) *audioPlayer {
	return &audioPlayer{source: source, client: client, after: after, end: make(chan struct{})}
}

func (p *audioPlayer) ended() bool {
	select {
	case <-p.end:
		return true
	default:
		return false
	}
}

// Reads, sends and paces frames until the source ends, waiting out reconnects, then silences the stream
func (p *audioPlayer) doRun() error {
	loops := 0
	start := time.Now()
	client := p.client
	p.speak(speakingVoice)
	for !p.ended() {
		data, err := p.source.read()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			p.mu.Lock()
			if p.err == nil {
				if se, ok := p.source.(interface{ currentError() error }); ok {
					p.err = se.currentError()
				}
			}
			p.mu.Unlock()
			p.stop()
			break
		}
		if !client.IsConnected() {
			timeout := client.conn.timeoutValue()
			slog.Debug(fmt.Sprintf("Not connected, waiting for %ss...", strconv.FormatFloat(timeout.Seconds(), 'f', 1, 64)))
			connected := client.waitUntilConnected(timeout)
			if p.ended() || !connected {
				slog.Debug("Aborting playback")
				return nil
			}
			slog.Debug("Reconnected, resuming playback")
			p.speak(speakingVoice)
			loops = 0
			start = time.Now()
		}
		if err := client.sendAudioPacket(data, !p.source.isOpus()); err != nil {
			return err
		}
		loops++
		next := start.Add(audioPlayerDelay * time.Duration(loops))
		delay := max(0, audioPlayerDelay+time.Until(next))
		time.Sleep(delay)
	}
	if client.IsConnected() {
		p.sendSilence(5)
	}
	return nil
}

// Runs the player, recording a failure, then calls after and cleans the source up
func (p *audioPlayer) run() {
	if err := p.doRun(); err != nil {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		p.stop()
	}
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	if p.after != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error(fmt.Sprintf("Calling the after function failed. %v", r))
				}
			}()
			p.after(err)
		}()
	} else if err != nil {
		slog.Error(fmt.Sprintf("Exception in voice thread audio-player: %s", err))
	}
	p.source.cleanup()
}

// Ends playback and announces the bot stopped speaking
func (p *audioPlayer) stop() {
	p.endOnce.Do(func() { close(p.end) })
	p.speak(speakingNone)
}

func (p *audioPlayer) isPlaying() bool { return !p.ended() }

// Sends a speaking update without blocking playback on failure
func (p *audioPlayer) speak(state int) {
	ws := p.client.conn.wsValue()
	if ws == nil {
		slog.Error("Speaking call in player failed")
		return
	}
	if err := ws.speak(state); err != nil {
		slog.Error(fmt.Sprintf("Speaking call in player failed: %s", err))
	}
}

// Sends Opus silence frames so receivers end the stream cleanly
func (p *audioPlayer) sendSilence(count int) {
	for range count {
		if err := p.client.sendAudioPacket(opusSilence, false); err != nil {
			return
		}
	}
}

// discord.py FFmpegProcessError carrying ffmpeg's exit code
type ffmpegProcessError struct{ msg string }

func (e *ffmpegProcessError) Error() string { return e.msg }

// PCM source decoding a file through an ffmpeg subprocess, discord.py's FFmpegPCMAudio
type ffmpegPCMAudio struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	mu      sync.Mutex
	err     error
	stopped bool
}

// Starts ffmpeg decoding the file to 48 kHz stereo signed 16 bit PCM, with extra output options
func newFFmpegPCMAudio(path, executable, options string) (*ffmpegPCMAudio, error) {
	args := []string{"-i", path, "-f", "s16le", "-ar", "48000", "-ac", "2", "-loglevel", "warning", "-blocksize", "8192"}
	if options != "" {
		extra, err := pyshlex.SplitPOSIX(options, false)
		if err != nil {
			return nil, err
		}
		args = append(args, extra...)
	}
	args = append(args, "pipe:1")
	cmd := ffmpegCommand(context.Background(), executable, args...)
	cmd.Stdin = nil
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	slog.Debug(fmt.Sprintf("Spawning ffmpeg process with command: %v", cmd.Args))
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, &voiceClientException{msg: executable + " was not found."}
		}
		return nil, &voiceClientException{msg: fmt.Sprintf("Popen failed: %s", err)}
	}
	return &ffmpegPCMAudio{cmd: cmd, stdout: stdout}, nil
}

// One 20 millisecond frame, empty at the end of the stream
func (f *ffmpegPCMAudio) read() ([]byte, error) {
	buf := make([]byte, opusFrameSize)
	n, _ := io.ReadFull(f.stdout, buf)
	if n != opusFrameSize {
		f.checkProcessReturncode()
		return nil, nil
	}
	return buf, nil
}

func (f *ffmpegPCMAudio) isOpus() bool { return false }

// Error the exited ffmpeg left, nil while it ran or exited cleanly
func (f *ffmpegPCMAudio) currentError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Records a nonzero ffmpeg exit unless playback stopped it
func (f *ffmpegPCMAudio) checkProcessReturncode() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.err != nil {
		return
	}
	if err := f.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() != 0 {
			f.err = &ffmpegProcessError{msg: fmt.Sprintf("FFmpeg exited with code %d. Stderr: <no stderr>", ee.ExitCode())}
		}
	}
}

// Kills ffmpeg and reaps it
func (f *ffmpegPCMAudio) cleanup() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	pid := f.cmd.Process.Pid
	slog.Debug(fmt.Sprintf("Preparing to terminate ffmpeg process %d.", pid))
	if err := f.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Error(fmt.Sprintf("Ignoring error attempting to kill ffmpeg process %d", pid))
	}
	f.cmd.Wait()
	code := -1
	if ps := f.cmd.ProcessState; ps != nil {
		code = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = -int(ws.Signal())
		}
	}
	slog.Info(fmt.Sprintf("ffmpeg process %d successfully terminated with return code of %d.", pid, code))
}

// PCM source scaling another source's samples, discord.py's PCMVolumeTransformer
type pcmVolumeTransformer struct {
	original audioSource
	volume   float64
}

func newPCMVolumeTransformer(original audioSource, volume float64) (*pcmVolumeTransformer, error) {
	if original.isOpus() {
		return nil, &voiceClientException{msg: "AudioSource must not be Opus encoded."}
	}
	return &pcmVolumeTransformer{original: original, volume: max(volume, 0)}, nil
}

func (v *pcmVolumeTransformer) read() ([]byte, error) {
	ret, err := v.original.read()
	if err != nil {
		return nil, err
	}
	return audioopMul(ret, min(v.volume, 2.0)), nil
}

func (v *pcmVolumeTransformer) isOpus() bool { return false }

func (v *pcmVolumeTransformer) cleanup() { v.original.cleanup() }

func (v *pcmVolumeTransformer) currentError() error {
	if se, ok := v.original.(interface{ currentError() error }); ok {
		return se.currentError()
	}
	return nil
}

// Signed 16 bit samples multiplied by a factor, clamped and floored like audioop.mul
func audioopMul(pcm []byte, factor float64) []byte {
	out := make([]byte, len(pcm)&^1)
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		v = math.Floor(math.Max(-32768, math.Min(32767, v*factor)))
		s := uint16(int16(v))
		out[i], out[i+1] = byte(s), byte(s>>8)
	}
	return out
}
