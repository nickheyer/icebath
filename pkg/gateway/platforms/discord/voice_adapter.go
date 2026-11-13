package discord

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
)

// Seconds added to a clip's duration when sizing its playback timeout
const playbackTimeoutPadding = 30

// Seconds between the keepalive datagrams the listen loop sends
const voiceKeepaliveInterval = 15 * time.Second

// Speech synthesis of an ack phrase, the text_to_speech tool tests replace
var voiceTextToSpeech = media.TextToSpeechTool

// Guild voice channel the adapter joins, carrying the cached channel
type voiceChannelHandle struct{ ch *discordgo.Channel }

func (h *voiceChannelHandle) Name() string { return h.ch.Name }

// Context of the adapter's background voice tasks, alive for the adapter's run
func (a *DiscordAdapter) voiceTaskContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runCtx != nil {
		return a.runCtx
	}
	return context.Background()
}

// Guild id text for the authorization check, empty when the client does not cache the guild
func (a *DiscordAdapter) voiceAuthGuild(guildID int64) string {
	id := strconv.FormatInt(guildID, 10)
	if a.stateGuild(id) == nil {
		return ""
	}
	return id
}

// Plays auto TTS in the voice channel bound to the chat, else sends it as a voice attachment
func (a *DiscordAdapter) PlayTTS(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	bound := make(map[int64]int64, len(a.voiceTextChannels))
	for gid, ch := range a.voiceTextChannels {
		bound[gid] = ch
	}
	a.mu.Unlock()
	for gid, textCh := range bound {
		if strconv.FormatInt(textCh, 10) == chatID && a.IsInVoiceChannel(gid) {
			slog.Info(fmt.Sprintf("[%s] Playing TTS in voice channel (guild=%d)", a.Name(), gid))
			success, err := a.playInVoiceChannel(ctx, gid, audioPath)
			if err != nil {
				return gateway.SendResult{}, err
			}
			return gateway.SendResult{Success: success}, nil
		}
	}
	return a.SendVoice(ctx, chatID, audioPath, caption, replyTo, metadata, extra)
}

// Configured inactivity timeout seconds
func (a *DiscordAdapter) voiceTimeoutLimit() int { return a.voiceTimeoutSeconds }

// Configured minimum playback wait seconds
func (a *DiscordAdapter) playbackTimeoutLimit() int { return a.playbackTimeoutSeconds }

// Duration of an audio file from ffprobe, nil when it cannot be read
func probeAudioDurationSeconds(ctx context.Context, audioPath string) *float64 {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", audioPath)
	cmd.Stdin = nil
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8(stdout.Bytes())
	if err != nil {
		return nil
	}
	raw := pystr.Strip(text)
	if raw == "" {
		return nil
	}
	seconds, err := pyint.ParseFloat(raw)
	if err != nil {
		return nil
	}
	return &seconds
}

// Playback timeout of a clip, the configured floor or its duration plus padding when longer
func (a *DiscordAdapter) playbackTimeoutForAudio(ctx context.Context, audioPath string) float64 {
	floor := float64(a.playbackTimeoutLimit())
	duration := probeAudioDurationSeconds(ctx, audioPath)
	if duration == nil || *duration <= 0 {
		return floor
	}
	if padded := *duration + playbackTimeoutPadding; padded > floor {
		return padded
	}
	return floor
}

// Python float of a voice_fx setting, the default when the key is absent
func (a *DiscordAdapter) voiceFXFloat(key string, def float64) (float64, error) {
	v, ok := a.voiceFXCfg[key]
	if !ok {
		return def, nil
	}
	return pyint.Float(v)
}

// Cached ambient bed PCM, a decodable custom file else the synthesised bed, nil when ambient is off
func (a *DiscordAdapter) getAmbientPCM(ctx context.Context) ([]byte, error) {
	a.mu.Lock()
	cached := a.ambientPCMCache
	a.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if !pyrepr.Truthy(a.voiceFXCfg["ambient_enabled"]) {
		return nil, nil
	}
	path := ""
	if raw := a.voiceFXCfg["ambient_path"]; pyrepr.Truthy(raw) {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
		}
		path = pystr.Strip(s)
	}
	var pcm []byte
	if path != "" {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			pcm = decodeToPCM(ctx, path, 30*time.Second)
			if len(pcm) == 0 {
				slog.Warn(fmt.Sprintf("Ambient file %s failed to decode; using synth bed", path))
			}
		}
	}
	if len(pcm) == 0 {
		synth, err := synthAmbientPCM(4.0)
		if err != nil {
			return nil, err
		}
		pcm = synth
	}
	a.mu.Lock()
	a.ambientPCMCache = pcm
	a.mu.Unlock()
	return pcm, nil
}

// Installs one mixer as the connection's lifelong source, layering speech over the ambient bed
func (a *DiscordAdapter) installVoiceMixer(ctx context.Context, guildID int64, vc voiceConnection) error {
	ambientGain, err := a.voiceFXFloat("ambient_gain", 0.18)
	if err != nil {
		return err
	}
	duckGain, err := a.voiceFXFloat("duck_gain", 0.06)
	if err != nil {
		return err
	}
	speechGain, err := a.voiceFXFloat("speech_gain", 1.0)
	if err != nil {
		return err
	}
	mixer := newVoiceMixer(ambientGain, duckGain, speechGain, 400)
	ambient, err := a.getAmbientPCM(ctx)
	if err != nil {
		return err
	}
	if len(ambient) > 0 {
		mixer.setAmbient(ambient, nil)
	}
	after := func(err error) {
		if err != nil {
			slog.Error(fmt.Sprintf("Voice mixer stream error (guild=%d): %s", guildID, err))
		}
	}
	if vc.IsPlaying() {
		vc.Stop()
	}
	if err := vc.Play(mixer, after); err != nil {
		return err
	}
	a.mu.Lock()
	a.voiceMixers[guildID] = mixer
	a.mu.Unlock()
	hasAmbient := "False"
	if len(ambient) > 0 {
		hasAmbient = "True"
	}
	slog.Info(fmt.Sprintf("Voice mixer installed (guild=%d, ambient=%s)", guildID, hasAmbient))
	return nil
}

// Configured lead silence milliseconds, zero when unset or not an integer
func (a *DiscordAdapter) leadSilenceMS() int64 {
	raw := a.voiceFXCfg["lead_silence_ms"]
	if !pyrepr.Truthy(raw) {
		return 0
	}
	ms, err := pyint.Int64(raw)
	if err != nil {
		return 0
	}
	return ms
}

// Silence prepended to speech clips so the voice socket warm up does not clip their start
func (a *DiscordAdapter) leadSilenceBytes() []byte {
	ms := a.leadSilenceMS()
	if ms <= 0 {
		return nil
	}
	return make([]byte, mixerBytesPerMS*ms)
}

// One ack phrase drawn from the configured phrases, the localized defaults when none are set
func (a *DiscordAdapter) pickAckPhrase(ctx context.Context) (string, error) {
	raw := a.voiceFXCfg["ack_phrases"]
	if !pyrepr.Truthy(raw) {
		phrases := defaultVoiceAckPhrases(ctx)
		return phrases[mrand.IntN(len(phrases))], nil
	}
	switch p := raw.(type) {
	case []any:
		item := p[mrand.IntN(len(p))]
		if s, ok := item.(string); ok {
			return s, nil
		}
		return pyrepr.Str(item), nil
	case []string:
		return p[mrand.IntN(len(p))], nil
	case string:
		r := []rune(p)
		return string(r[mrand.IntN(len(r))]), nil
	}
	return "", fmt.Errorf("TypeError: object of type '%s' has no len()", pyrepr.TypeName(raw))
}

// Speaks a short ack over the ambient bed, false without acks enabled, a mixer or a synthesized clip
func (a *DiscordAdapter) PlayAckInVoice(ctx context.Context, guildID int, phrase *string) (bool, error) {
	gid := int64(guildID)
	if !pyrepr.Truthy(a.voiceFXCfg["ack_enabled"]) {
		return false, nil
	}
	a.mu.Lock()
	mixer := a.voiceMixers[gid]
	a.mu.Unlock()
	if mixer == nil {
		return false, nil
	}
	text := ""
	if phrase != nil {
		text = *phrase
	} else {
		picked, err := a.pickAckPhrase(ctx)
		if err != nil {
			return false, err
		}
		text = picked
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return false, err
	}
	audioPath := filepath.Join(os.TempDir(), "icebath_voice", "ack_"+hex.EncodeToString(raw[:])+".mp3")
	if err := os.MkdirAll(filepath.Dir(audioPath), 0o777); err != nil {
		return false, err
	}
	actual := ""
	defer func() {
		for _, p := range []string{audioPath, actual} {
			if p == "" {
				continue
			}
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				os.Remove(p)
			}
		}
	}()
	fail := func(err error) (bool, error) {
		slog.Debug(fmt.Sprintf("play_ack_in_voice failed: %s", err))
		return false, nil
	}
	resultJSON, err := voiceTextToSpeech(ctx, text, audioPath)
	if err != nil {
		return fail(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return fail(err)
	}
	actual = audioPath
	fp, present := result["file_path"]
	path, isPath := fp.(string)
	if present && isPath {
		actual = path
	}
	if !pyrepr.Truthy(result["success"]) {
		return false, nil
	}
	if present && !isPath {
		return fail(fmt.Errorf("TypeError: stat: path should be string, bytes, os.PathLike or integer, not %s", pyrepr.TypeName(fp)))
	}
	if st, err := os.Stat(actual); err != nil || !st.Mode().IsRegular() {
		return false, nil
	}
	pcm := decodeToPCM(ctx, actual, 30*time.Second)
	if len(pcm) == 0 {
		return false, nil
	}
	gain, err := a.voiceFXFloat("speech_gain", 1.0)
	if err != nil {
		return fail(err)
	}
	mixer.playSpeech(append(a.leadSilenceBytes(), pcm...), &gain, 40)
	a.resetVoiceTimeout(gid)
	return true, nil
}

// Whether a continuous mixer is installed for the guild
func (a *DiscordAdapter) VoiceMixerActive(guildID int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.voiceMixers) > 0 && a.voiceMixers[int64(guildID)] != nil
}

// Connects a voice client to a guild channel the way discord.py's channel.connect does
func (a *DiscordAdapter) connectVoiceChannel(ctx context.Context, guildID int64, channel *discordgo.Channel) (*voiceClient, error) {
	a.mu.Lock()
	_, busy := a.voiceProtocols[guildID]
	client := a.client
	a.mu.Unlock()
	if busy {
		return nil, &voiceClientException{msg: "Already connected to a voice channel."}
	}
	if client == nil {
		return nil, errors.New("Not connected")
	}
	if _, err := loadDave(); err != nil {
		return nil, err
	}
	self := a.selfUser()
	if self == nil {
		return nil, errors.New("AttributeError: 'NoneType' object has no attribute 'id'")
	}
	userID, err := strconv.ParseInt(self.ID, 10, 64)
	if err != nil {
		return nil, err
	}
	channelID, err := strconv.ParseInt(channel.ID, 10, 64)
	if err != nil {
		return nil, err
	}
	var vc *voiceClient
	vc = newVoiceClient(client, guildID, channelID, userID, func() {
		a.mu.Lock()
		if a.voiceProtocols[guildID] == vc {
			delete(a.voiceProtocols, guildID)
		}
		a.mu.Unlock()
	})
	a.mu.Lock()
	a.voiceProtocols[guildID] = vc
	a.mu.Unlock()
	if err := vc.connect(ctx, 30*time.Second, true, false, false); err != nil {
		if errors.Is(err, errVoiceConnectTimeout) {
			vc.disconnect(context.WithoutCancel(ctx), true)
		}
		return nil, err
	}
	return vc, nil
}

// Joins a voice channel, binding transcription to the text channel and starting capture and the mixer
func (a *DiscordAdapter) joinVoiceChannel(ctx context.Context, channel *discordgo.Channel, textChannelID *int64, source map[string]any) (bool, error) {
	if a.liveClient() == nil {
		return false, nil
	}
	guildID, err := strconv.ParseInt(channel.GuildID, 10, 64)
	if err != nil {
		return false, fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'id'")
	}
	lock := a.voiceLock(guildID)
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	existing := a.voiceClients[guildID]
	a.mu.Unlock()
	if existing != nil && existing.IsConnected() {
		if existing.ChannelID() != channel.ID {
			target, err := strconv.ParseInt(channel.ID, 10, 64)
			if err != nil {
				return false, err
			}
			existing.MoveTo(ctx, target)
		}
		a.resetVoiceTimeout(guildID)
		a.bindVoiceTextChannel(guildID, textChannelID, source)
		return true, nil
	}
	vc, err := a.connectVoiceChannel(ctx, guildID, channel)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.voiceClients[guildID] = vc
	allowed := a.allowedUserIDs
	a.mu.Unlock()
	a.resetVoiceTimeout(guildID)
	a.bindVoiceTextChannel(guildID, textChannelID, source)
	receiver := newVoiceReceiver(vc, allowed)
	if err := receiver.start(); err != nil {
		slog.Warn(fmt.Sprintf("Voice receiver failed to start: %s", err))
	} else {
		taskCtx := a.voiceTaskContext()
		a.mu.Lock()
		a.voiceReceivers[guildID] = receiver
		a.voiceListenTasks[guildID] = spawnTask(taskCtx, func(lctx context.Context) { a.voiceListenLoop(lctx, guildID) })
		a.mu.Unlock()
	}
	if pyrepr.Truthy(a.voiceFXCfg["enabled"]) {
		if err := a.installVoiceMixer(ctx, guildID, vc); err != nil {
			slog.Warn(fmt.Sprintf("Voice mixer failed to start: %s", err))
		}
	}
	return true, nil
}

// Joins the voice channel GetUserVoiceChannel returned, the runner's /voice join entry
func (a *DiscordAdapter) JoinVoiceChannel(ctx context.Context, channel interface{ Name() string }) (bool, error) {
	h, ok := channel.(*voiceChannelHandle)
	if !ok || h == nil || h.ch == nil {
		return false, fmt.Errorf("AttributeError: '%s' object has no attribute 'guild'", pyrepr.TypeName(channel))
	}
	return a.joinVoiceChannel(ctx, h.ch, nil, nil)
}

// Leaves the guild's voice channel, the runner's /voice leave entry
func (a *DiscordAdapter) LeaveVoiceChannel(ctx context.Context, guildID int64) error {
	return a.leaveVoiceChannel(ctx, guildID)
}

// Duration of a seconds value, saturating at the longest duration for infinite or huge values
func secondsDuration(seconds float64) time.Duration {
	if math.IsNaN(seconds) || seconds*float64(time.Second) >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

// Sleeps for d unless ctx ends first
func voiceSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Plays a clip through the mixer over the ambient bed when installed, else as one ffmpeg source
func (a *DiscordAdapter) playInVoiceChannel(ctx context.Context, guildID int64, audioPath string) (bool, error) {
	a.mu.Lock()
	vc := a.voiceClients[guildID]
	a.mu.Unlock()
	if vc == nil || !vc.IsConnected() {
		return false, nil
	}
	a.cancelVoiceTimeout(guildID)
	defer a.resetVoiceTimeout(guildID)
	playbackTimeout := a.playbackTimeoutForAudio(ctx, audioPath)
	limit := secondsDuration(playbackTimeout)
	a.mu.Lock()
	mixer := a.voiceMixers[guildID]
	a.mu.Unlock()
	if mixer != nil {
		pcm := decodeToPCM(ctx, audioPath, 30*time.Second)
		if len(pcm) > 0 {
			gain, err := a.voiceFXFloat("speech_gain", 1.0)
			if err != nil {
				return false, err
			}
			mixer.playSpeech(append(a.leadSilenceBytes(), pcm...), &gain, 40)
			start := time.Now()
			for mixer.speechIsActive() {
				if time.Since(start) > limit {
					slog.Warn(fmt.Sprintf("Mixer speech playback timed out after %.1fs", playbackTimeout))
					mixer.stopSpeech()
					break
				}
				if err := voiceSleep(ctx, 50*time.Millisecond); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		slog.Warn(fmt.Sprintf("Mixer decode failed for %s; falling back to legacy playback", audioPath))
	}
	a.mu.Lock()
	receiver := a.voiceReceivers[guildID]
	a.mu.Unlock()
	if receiver != nil {
		receiver.Pause()
		defer receiver.Resume()
	}
	start := time.Now()
	for vc.IsPlaying() {
		if time.Since(start) > limit {
			slog.Warn("Timed out waiting for previous playback to finish")
			vc.Stop()
			break
		}
		if err := voiceSleep(ctx, 100*time.Millisecond); err != nil {
			return false, err
		}
	}
	done := make(chan struct{})
	var doneOnce sync.Once
	after := func(err error) {
		if err != nil {
			slog.Error(fmt.Sprintf("Voice playback error: %s", err))
		}
		doneOnce.Do(func() { close(done) })
	}
	options := ""
	if ms := a.leadSilenceMS(); ms > 0 {
		options = fmt.Sprintf("-af adelay=%d:all=1", ms)
	}
	pcmSource, err := newFFmpegPCMAudio(audioPath, resolveFFmpegExecutable(), options)
	if err != nil {
		return false, err
	}
	source, err := newPCMVolumeTransformer(pcmSource, 1.0)
	if err != nil {
		pcmSource.cleanup()
		return false, err
	}
	if err := vc.Play(source, after); err != nil {
		source.cleanup()
		return false, err
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn(fmt.Sprintf("Voice playback timed out after %.1fs", playbackTimeout))
		vc.Stop()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return true, nil
}

// Plays a clip in the guild's voice channel, the runner's spoken reply entry
func (a *DiscordAdapter) PlayInVoiceChannel(ctx context.Context, guildID int64, audioPath string) error {
	_, err := a.playInVoiceChannel(ctx, guildID, audioPath)
	return err
}

// Voice channel the member sits in, nil when the guild, member, voice state or channel is not cached
func (a *DiscordAdapter) GetUserVoiceChannel(ctx context.Context, guildID int64, userID *string) (interface{ Name() string }, error) {
	client := a.liveClient()
	if client == nil {
		return nil, nil
	}
	guild := strconv.FormatInt(guildID, 10)
	if a.stateGuild(guild) == nil {
		return nil, nil
	}
	if userID == nil {
		return nil, errors.New("TypeError: int() argument must be a string, a bytes-like object or a real number, not 'NoneType'")
	}
	uid, err := pyint.Int64(*userID)
	if err != nil {
		return nil, err
	}
	user := strconv.FormatInt(uid, 10)
	if _, err := client.State.Member(guild, user); err != nil {
		return nil, nil
	}
	vs, err := client.State.VoiceState(guild, user)
	if err != nil || vs == nil || vs.ChannelID == "" {
		return nil, nil
	}
	ch := a.stateChannel(vs.ChannelID)
	if ch == nil {
		return nil, nil
	}
	return &voiceChannelHandle{ch: ch}, nil
}

// Cancels the guild's inactivity timer
func (a *DiscordAdapter) cancelVoiceTimeout(guildID int64) {
	a.mu.Lock()
	task := a.voiceTimeoutTasks[guildID]
	delete(a.voiceTimeoutTasks, guildID)
	a.mu.Unlock()
	if task != nil {
		task.cancel()
	}
}

// Restarts the guild's auto disconnect inactivity timer
func (a *DiscordAdapter) resetVoiceTimeout(guildID int64) {
	a.cancelVoiceTimeout(guildID)
	timeout := a.voiceTimeoutLimit()
	if timeout <= 0 {
		slog.Debug(fmt.Sprintf("Voice inactivity timeout disabled (guild=%d)", guildID))
		return
	}
	task := spawnTask(a.voiceTaskContext(), func(tctx context.Context) { a.voiceTimeoutHandler(tctx, guildID, &timeout) })
	a.mu.Lock()
	a.voiceTimeoutTasks[guildID] = task
	a.mu.Unlock()
}

// Leaves voice after the inactivity timeout unless the bound chat turned voice replies off
func (a *DiscordAdapter) voiceTimeoutHandler(ctx context.Context, guildID int64, timeout *int) {
	limit := a.voiceTimeoutLimit()
	if timeout != nil {
		limit = *timeout
	}
	if limit <= 0 {
		return
	}
	if err := voiceSleep(ctx, time.Duration(limit)*time.Second); err != nil {
		return
	}
	a.mu.Lock()
	textCh, bound := a.voiceTextChannels[guildID]
	getter := a.voiceModeGetter
	a.mu.Unlock()
	if bound && getter != nil {
		off := func() (off bool) {
			defer func() {
				if recover() != nil {
					off = false
				}
			}()
			return getter(strconv.FormatInt(textCh, 10)) == "off"
		}()
		if off {
			return
		}
	}
	if err := a.leaveVoiceChannel(ctx, guildID); err != nil {
		slog.Error(fmt.Sprintf("Task exception was never retrieved: %s", err))
		return
	}
	a.mu.Lock()
	notify := a.onVoiceDisconnect
	a.mu.Unlock()
	if notify != nil && bound && textCh != 0 {
		func() {
			defer func() { recover() }()
			notify(strconv.FormatInt(textCh, 10))
		}()
	}
	if bound && textCh != 0 && a.liveClient() != nil {
		if ch := a.stateChannel(strconv.FormatInt(textCh, 10)); ch != nil {
			a.channelSend(ctx, ch.ID, i18n.T(ctx, "platform.discord.voice.left_inactivity", "", nil), nil)
		}
	}
}

// Whether the bot holds a connected voice client in the guild
func (a *DiscordAdapter) IsInVoiceChannel(guildID int64) bool {
	a.mu.Lock()
	vc := a.voiceClients[guildID]
	a.mu.Unlock()
	return vc != nil && vc.IsConnected()
}

// Drops speech the receiver buffered but has not emitted, before the text binding moves
func (a *DiscordAdapter) DiscardPendingVoiceInput(guildID int64) {
	a.mu.Lock()
	receiver := a.voiceReceivers[guildID]
	a.mu.Unlock()
	if receiver != nil {
		receiver.DiscardPending()
	}
}

// Polls the receiver for finished utterances, transcribing allowed speakers and keeping the socket alive
func (a *DiscordAdapter) voiceListenLoop(ctx context.Context, guildID int64) {
	a.mu.Lock()
	receiver := a.voiceReceivers[guildID]
	a.mu.Unlock()
	if receiver == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error(fmt.Sprintf("Voice listen loop error: %v", p))
		}
	}()
	lastKeepalive := time.Now()
	for receiver.Running() {
		if err := voiceSleep(ctx, 200*time.Millisecond); err != nil {
			return
		}
		if now := time.Now(); now.Sub(lastKeepalive) >= voiceKeepaliveInterval {
			lastKeepalive = now
			a.mu.Lock()
			vc := a.voiceClients[guildID]
			a.mu.Unlock()
			if vc != nil && vc.IsConnected() {
				vc.SendPacket(opusSilence)
			}
		}
		completed := receiver.CheckSilence()
		a.mu.Lock()
		textCh, bound := a.voiceTextChannels[guildID]
		a.mu.Unlock()
		var capturedFor *int64
		if bound {
			capturedFor = &textCh
		}
		guild := a.voiceAuthGuild(guildID)
		for _, u := range completed {
			if !a.isAllowedUser(ctx, strconv.FormatInt(u.userID, 10), nil, guild, false, nil) {
				continue
			}
			a.resetVoiceTimeout(guildID)
			a.processVoiceInput(ctx, guildID, u.userID, u.pcm, capturedFor)
		}
	}
}

// Registers the runner callback told the text channel of a voice call that timed out
func (a *DiscordAdapter) SetOnVoiceDisconnect(fn func(chatID string)) {
	a.mu.Lock()
	a.onVoiceDisconnect = fn
	a.mu.Unlock()
}

// Registers the runner's reader of a text channel's live voice reply mode
func (a *DiscordAdapter) SetVoiceModeGetter(fn func(chatID any) string) {
	a.mu.Lock()
	a.voiceModeGetter = fn
	a.mu.Unlock()
}

// Binds the guild's voice transcription to a text channel
func (a *DiscordAdapter) BindVoiceTextChannel(guildID, textChannelID int64) {
	a.mu.Lock()
	a.voiceTextChannels[guildID] = textChannelID
	a.mu.Unlock()
}

// Stores the source of the message that joined the guild's voice channel
func (a *DiscordAdapter) SetVoiceSource(guildID int64, source *pyjson.Object) {
	stored := map[string]any{}
	if source != nil {
		for _, k := range source.Keys {
			stored[k] = source.Vals[k]
		}
	}
	a.mu.Lock()
	a.voiceSources[guildID] = stored
	a.mu.Unlock()
}

// Source stored for the guild's voice binding
func (a *DiscordAdapter) VoiceSource(guildID int64) (map[string]any, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.voiceSources[guildID]
	return s, ok
}

// Bound text channel of every guild with a voice binding
func (a *DiscordAdapter) VoiceTextChannels() map[int]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[int]string, len(a.voiceTextChannels))
	for gid, ch := range a.voiceTextChannels {
		out[int(gid)] = strconv.FormatInt(ch, 10)
	}
	return out
}
