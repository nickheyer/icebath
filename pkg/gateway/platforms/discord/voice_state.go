package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
)

// Voice channel connection of one guild as the adapter drives it
type voiceConnection interface {
	ChannelID() string
	IsConnected() bool
	IsPlaying() bool
	Stop()
	Disconnect(ctx context.Context) error
	MoveTo(ctx context.Context, channelID int64)
	Play(source audioSource, after func(error)) error
	SendPacket(packet []byte) error
	Channel() *discordgo.Channel
	ChannelMembers() []*discordgo.Member
}

// Inbound speech capture of one guild voice connection
type voiceCapture interface {
	FlushPending() []voiceUtterance
	Stop()
	RefreshCredentials(reason string)
	CheckSilence() []voiceUtterance
	DiscardPending()
	Pause()
	Resume()
	Running() bool
	SpeakingUserIDs(window time.Duration) map[int64]bool
}

// Buffered speech of one user awaiting transcription
type voiceUtterance struct {
	userID int64
	pcm    []byte
}

// Per guild voice state the adapter holds, keyed by guild id like hermes
type voiceState struct {
	voiceClients       map[int64]voiceConnection
	voiceLocks         map[int64]*sync.Mutex
	voiceTextChannels  map[int64]int64
	voiceSources       map[int64]map[string]any
	voiceTimeoutTasks  map[int64]*bgTask
	voiceReceivers     map[int64]voiceCapture
	voiceListenTasks   map[int64]*bgTask
	voiceMixers        map[int64]*voiceMixer
	voiceInputCallback func(ctx context.Context, guildID, userID int64, transcript string) error
	onVoiceDisconnect  func(chatID string)
	voiceModeGetter    func(chatID any) string
	ambientPCMCache    []byte
	voiceProtocols     map[int64]*voiceClient
}

func newVoiceState() voiceState {
	return voiceState{voiceClients: map[int64]voiceConnection{}, voiceLocks: map[int64]*sync.Mutex{}, voiceTextChannels: map[int64]int64{},
		voiceSources: map[int64]map[string]any{}, voiceTimeoutTasks: map[int64]*bgTask{}, voiceReceivers: map[int64]voiceCapture{},
		voiceListenTasks: map[int64]*bgTask{}, voiceMixers: map[int64]*voiceMixer{}, voiceProtocols: map[int64]*voiceClient{}}
}

// Registers the runner callback that receives transcribed voice channel speech
func (a *DiscordAdapter) SetVoiceInputCallback(fn func(ctx context.Context, guildID, userID int64, transcript string) error) {
	a.mu.Lock()
	a.voiceInputCallback = fn
	a.mu.Unlock()
}

// Text channel the voice call of a guild is bound to
func (a *DiscordAdapter) VoiceTextChannel(guildID int64) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.voiceTextChannels[guildID]
	return id, ok
}

// Join and leave lock of one guild
func (a *DiscordAdapter) voiceLock(guildID int64) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.voiceLocks[guildID]
	if !ok {
		l = &sync.Mutex{}
		a.voiceLocks[guildID] = l
	}
	return l
}

// Refreshes the voice receiver of the bot channel when a member joins, leaves or moves through it
func (a *DiscordAdapter) onVoiceStateUpdate(ctx context.Context, before *discordgo.VoiceState, after *discordgo.VoiceState) {
	a.mu.Lock()
	if len(a.voiceClients) == 0 || after == nil {
		a.mu.Unlock()
		return
	}
	guildID, err := strconv.ParseInt(after.GuildID, 10, 64)
	vc, inGuild := a.voiceClients[guildID]
	receiver := a.voiceReceivers[guildID]
	a.mu.Unlock()
	if err != nil || !inGuild {
		return
	}
	if self := a.selfUser(); self != nil && after.UserID == self.ID {
		return
	}
	beforeChannel, afterChannel := "", after.ChannelID
	if before != nil {
		beforeChannel = before.ChannelID
	}
	joined := beforeChannel == "" && afterChannel != ""
	left := beforeChannel != "" && afterChannel == ""
	switched := beforeChannel != "" && afterChannel != "" && beforeChannel != afterChannel
	if !joined && !left && !switched {
		return
	}
	name := func(id string) string {
		if ch := a.stateChannel(id); ch != nil {
			return ch.Name
		}
		return id
	}
	what := ""
	switch {
	case joined:
		what = "joined " + name(afterChannel)
	case left:
		what = "left " + name(beforeChannel)
	default:
		what = "moved " + name(beforeChannel) + " -> " + name(afterChannel)
	}
	display := ""
	if after.Member != nil {
		display = memberDisplayName(after.Member)
	}
	slog.Info(fmt.Sprintf("Voice state: %s (%s) %s (guild %d)", display, after.UserID, what, guildID))
	if vc == nil || receiver == nil {
		return
	}
	if bot := vc.ChannelID(); bot != "" && (beforeChannel == bot || afterChannel == bot) {
		receiver.RefreshCredentials("membership change")
	}
}

// Leaves the voice channel of a guild, transcribing speech still buffered by allowed users first
func (a *DiscordAdapter) leaveVoiceChannel(ctx context.Context, guildID int64) error {
	lock := a.voiceLock(guildID)
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	receiver := a.voiceReceivers[guildID]
	delete(a.voiceReceivers, guildID)
	listen := a.voiceListenTasks[guildID]
	delete(a.voiceListenTasks, guildID)
	captured, hasCaptured := a.voiceTextChannels[guildID]
	a.mu.Unlock()
	var pending []voiceUtterance
	if receiver != nil {
		pending = receiver.FlushPending()
		receiver.Stop()
	}
	if listen != nil {
		listen.cancel()
	}
	guild := a.voiceAuthGuild(guildID)
	var capturedFor *int64
	if hasCaptured {
		capturedFor = &captured
	}
	for _, u := range pending {
		if a.isAllowedUser(ctx, strconv.FormatInt(u.userID, 10), nil, guild, false, nil) {
			a.processVoiceInput(ctx, guildID, u.userID, u.pcm, capturedFor)
		}
	}
	a.mu.Lock()
	delete(a.voiceMixers, guildID)
	vc := a.voiceClients[guildID]
	delete(a.voiceClients, guildID)
	a.mu.Unlock()
	if vc != nil && vc.IsConnected() {
		if vc.IsPlaying() {
			vc.Stop()
		}
		if err := vc.Disconnect(ctx); err != nil {
			return err
		}
	}
	a.mu.Lock()
	task := a.voiceTimeoutTasks[guildID]
	delete(a.voiceTimeoutTasks, guildID)
	delete(a.voiceTextChannels, guildID)
	delete(a.voiceSources, guildID)
	a.mu.Unlock()
	if task != nil {
		task.cancel()
	}
	return nil
}

// Command that runs ffmpeg, replaced by tests that fake the subprocess
var ffmpegCommand = exec.CommandContext

// Converts raw 48 kHz stereo PCM to a 16 kHz mono WAV file through ffmpeg
func pcmToWav(ctx context.Context, pcm []byte, outputPath string, srcRate, srcChannels int) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := ffmpegCommand(cctx, resolveFFmpegExecutable(), "-y", "-loglevel", "error", "-f", "s16le", "-ar", strconv.Itoa(srcRate),
		"-ac", strconv.Itoa(srcChannels), "-i", "pipe:0", "-ar", "16000", "-ac", "1", outputPath)
	cmd.Stdin = bytes.NewReader(pcm)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &ffmpegError{err: err, stderr: stderr.String()}
	}
	return nil
}

// Failed ffmpeg run carrying its captured stderr
type ffmpegError struct {
	err    error
	stderr string
}

func (e *ffmpegError) Error() string { return e.err.Error() }

// Speech to text of one utterance, handed to the runner unless the binding moved during transcription
func (a *DiscordAdapter) processVoiceInput(ctx context.Context, guildID, userID int64, pcm []byte, capturedFor *int64) {
	f, err := os.CreateTemp("", "vc_listen_*.wav")
	if err != nil {
		slog.Warn(fmt.Sprintf("Voice input processing failed: %s", err))
		return
	}
	wavPath := f.Name()
	f.Close()
	defer os.Remove(wavPath)
	fail := func(err error) {
		var fe *ffmpegError
		if errors.As(err, &fe) && fe.stderr != "" {
			slog.Warn(fmt.Sprintf("Voice input processing failed: %s (ffmpeg: %s)", err, pystr.Strip(fe.stderr)))
			return
		}
		slog.Warn(fmt.Sprintf("Voice input processing failed: %s", err))
	}
	if err := pcmToWav(ctx, pcm, wavPath, 48000, 2); err != nil {
		fail(err)
		return
	}
	result, err := media.TranscribeAudio(ctx, wavPath, "", "")
	if err != nil {
		fail(err)
		return
	}
	if ok, _ := result["success"].(bool); !ok {
		return
	}
	transcript, _ := result["transcript"].(string)
	transcript = pystr.Strip(transcript)
	if transcript == "" || media.IsWhisperHallucination(transcript) {
		return
	}
	slog.Info(fmt.Sprintf("Voice input from user %d: %s", userID, cut(transcript, 100)))
	a.mu.Lock()
	current, bound := a.voiceTextChannels[guildID]
	callback := a.voiceInputCallback
	a.mu.Unlock()
	if bound != (capturedFor != nil) || (bound && current != *capturedFor) {
		slog.Info(fmt.Sprintf("Dropping voice input from user %d: binding moved during transcription", userID))
		return
	}
	if callback != nil {
		if err := callback(ctx, guildID, userID, transcript); err != nil {
			fail(err)
		}
	}
}

// Guild ids with a live voice connection
func (a *DiscordAdapter) voiceGuildIDs() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]int64, 0, len(a.voiceClients))
	for id := range a.voiceClients {
		out = append(out, id)
	}
	return out
}
