package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Registers the speech sections, the services offered for each choice listed as the plugins registered them
func Register(cfg *config.Config, voices, recognizers, engines func() []config.Option) {
	cfg.Register(config.Sections{
		{Key: Voice, Title: "Voice", About: "Talking to the agent instead of typing, and hearing its replies", Settings: []config.Setting{
			{Key: Input, Title: "Voice input", About: "Talk instead of typing: a key records you and sends the words", Default: false},
			{Key: SpeakReplies, Title: "Speak replies", About: "Read the assistant's replies aloud, or send voice messages in chats. The terminal UI speaks while voice input " +
				"is on; a chat keeps its own choice once /voice is used there", Default: false},
			{Key: RecordKey, Title: "Record key", About: "Extra key that starts and stops a recording, such as ctrl+space; alt+r always records, " +
				"and a binding in your keys file wins over this", Default: ""},
			{Key: SubmitMode, Title: "Send or draft", About: "Send what you said at once, or leave it in the input box to edit", Default: SubmitDirect,
				Options: config.Options(
					config.Option{Value: SubmitDirect, Title: "Send", About: "Send the words as soon as you stop"},
					config.Option{Value: SubmitDraft, Title: "Draft", About: "Put the words in the input box"},
				)},
			{Key: BargeIn, Title: "Interrupt by talking", About: "Stop spoken replies when you start talking over them", Default: true},
			{Key: BargeInGrace, Title: "Interrupt grace", About: "Time after a reply starts before talking can interrupt it, in seconds or as a length such as 500ms",
				Default: 500 * time.Millisecond},
			{Key: BargeInMultiplier, Title: "Interrupt sensitivity", About: "How much louder than the room you must be to interrupt; 0 turns the loudness check off", Default: 3.0},
			{Key: Beep, Title: "Recording beep", About: "Play a beep when recording starts and stops", Default: true},
			{Key: BeepVolume, Title: "Beep volume", About: "How loud the recording beep is, from 0 to 1", Default: DefaultBeepVolume},
			{Key: Debug, Title: "Voice debug log", About: "Print each step of voice capture and recognition to the error output", Default: false, Env: []string{DebugEnv}},
			{Key: MaxRecording, Title: "Longest recording", About: "How long voice input listens before it stops, in seconds or as a length such as 2m", Default: 2 * time.Minute},
			{Key: SilenceDuration, Title: "Silence length", About: "Seconds of quiet before recording stops on its own", Default: DefaultSilenceSecs * time.Second},
			{Key: SilenceThreshold, Title: "Silence level", About: "Loudness below which audio counts as quiet; raise it in noisy rooms", Default: DefaultSilence},
			{Key: StopPhrases, Title: "Stop phrases", About: "Spoken words that end voice conversation mode", Default: []string{"stop"}},
			{Key: ThinkingSound, Title: "Thinking sound", About: "Play soft blips while waiting for a reply", Default: true},
		}},
		{Key: TTS, Title: "Speech", About: "The voice that reads replies aloud and speaks for text_to_speech; each service's own settings are under it", Settings: []config.Setting{
			{Key: TTSProvider, Title: "Speech voice", About: "Which service speaks for icebath, or a program of yours under Speech commands", Default: DefaultVoice, Options: voices},
			{Key: Speed, Title: "Speech speed", About: "How fast voices speak, 1 being normal; 0 leaves it to each service. Services with a speed setting of their own use that", Default: 0.0},
			{Key: PluginVoice, Title: "Speech voice name", About: "Voice handed to the speech services of plugins; empty for their own", Default: ""},
			{Key: PluginModel, Title: "Speech model", About: "Model handed to services of plugins; empty for their own", Default: ""},
			{Key: OutputFormat, Title: "Output format", About: "Audio format services of plugins write, such as mp3; empty for mp3", Default: ""},
			{Key: Latency, Title: "Live latency boost", About: "Trade quality for a faster start when speech plays as it arrives, 0 to 2; used by xAI speech when its own setting is unset", Default: 0},
			{Key: KeepWarm, Title: "Keep voice loaded", About: "How long a local voice stays loaded after use, in seconds or as a length such as 5m; 0 unloads it at once",
				Default: time.Minute},
			{Key: Delivery, Title: "Audio upload limits", About: "Largest speech file sent to each chat app, by app name, each with max_file_bytes and safety_ratio; default covers " +
				"the apps not named. Longer speech is sent as several files", Default: map[string]any{}},
			{Key: TTSCommands, Title: "Speech commands", About: "Your own speech programs, by the name Speech voice picks them with: each a command with {input_path}, {output_path}, " +
				"{format}, {voice}, {model} and {speed}, and output_format, voice, model, speed, timeout, voice_compatible, max_text_length, " +
				"env_passthrough, warm_command and release_command. A program may also sit directly under tts by its name", Default: map[string]any{}},
			{Key: StreamingMinLen, Title: "Live chunk size", About: "Smallest piece of text in characters spoken at once while speech streams", Default: 20},
			{Key: StreamingProvider, Title: "Live speech service", About: "Service for speech that plays as the text arrives, auto for the first that can stream; empty for Speech voice", Default: ""},
		}},
		{Key: STT, Title: "Speech recognition", About: "Turning your recordings and voice messages into text; each service's own settings are under it", Settings: []config.Setting{
			{Key: STTEnabled, Title: "Speech recognition", About: "Turn your recordings and voice messages into text", Default: true},
			{Key: STTProvider, Title: "Recognition service", About: "Which service turns your speech into text, or a program of yours under Recognition commands", Default: DefaultRecognize,
				Options: recognizers},
			{Key: EchoTranscripts, Title: "Show transcripts", About: "Post the text of your voice messages back into the chat", Default: true},
			{Key: Language, Title: "Recognition language", About: "Language of your speech, as a code such as en, used when the service has no language of its own; empty detects it",
				Default: "", Env: []string{LanguageEnv}},
			{Key: Prompt, Title: "Recognition prompt", About: "Text that primes recognition, such as names or jargon", Default: ""},
			{Key: Streaming, Title: "Live transcription", About: "Show words as you speak when the chosen service supports it", Default: false},
			{Key: TrimSilence, Title: "Trim silence", About: "Cut silence from recordings before sending them to a cloud service", Default: true},
			{Key: TrimThreshold, Title: "Trim threshold", About: "Loudness in dB below which audio counts as silence when trimming", Default: -40},
			{Key: TrimKeep, Title: "Trim padding", About: "Quiet kept around speech when trimming, in milliseconds or as a length such as 0.3s", Default: 300 * time.Millisecond,
				Unit: time.Millisecond},
			{Key: STTCommands, Title: "Recognition commands", About: "Your own speech recognition programs, by the name Recognition service picks them with: each a command with " +
				"{input_path}, {output_path}, {output_dir}, {format}, {language} and {model}, and output_format, language, model, timeout, normalize and env_passthrough. " +
				"A program may also sit directly under stt by its name", Default: map[string]any{}},
			{Key: LocalCommand, Title: "Local command", About: "Program that transcribes a recording on this machine, with {input_path}, {output_dir}, {language} and {model} " +
				"where they belong; it must leave a .txt file in {output_dir}. Empty uses the bundled whisper.cpp engine, else a whisper program on PATH",
				Default: "", Env: []string{LocalCommandEnv}},
		}},
		{Key: Wake, Title: "Wake word", About: "Saying a phrase to start recording without the keyboard", Settings: []config.Setting{
			{Key: WakeEnabled, Title: "Wake word", About: "Say the wake phrase to start recording without the keyboard", Default: false},
			{Key: WakeSurface, Title: "Wake listens in", About: "Which screen runs the wake word listener", Default: AutoWake, Options: config.Options(
				config.Option{Value: AutoWake, Title: "Auto", About: "Any screen"},
				config.Option{Value: "cli", Title: "CLI", About: "Plain terminal only"},
				config.Option{Value: "tui", Title: "TUI", About: "Terminal UI only"},
				config.Option{Value: "gui", Title: "GUI", About: "Desktop app only"},
			)},
			{Key: WakeInput, Title: "Wake microphone", About: "Microphone to listen on, by number or name; empty for the default microphone", Default: ""},
			{Key: WakeProvider, Title: "Wake engine", About: "Engine that listens for the wake word; auto picks one for you", Default: AutoWake, Options: engines},
			{Key: WakePhrase, Title: "Wake phrase", About: "Phrase that wakes the assistant", Default: DefaultPhrase},
			{Key: WakeSensitivity, Title: "Wake sensitivity", About: "How easily the wake word triggers, from 0 to 1; higher catches more but triggers by mistake more often", Default: 0.6},
			{Key: WakeConfirmationFrames, Title: "Confirmation frames", About: "Checks in a row, 1 to 10, that must hear the wake word before it triggers", Default: 3},
			{Key: WakeNewSession, Title: "Start fresh session", About: "Begin a new session each time the wake word is heard", Default: true},
			{Key: WakeProfileRouting, Title: "Route by profile", About: "Each profile can have its own wake phrase and answers to it", Default: true},
		}},
	})
}
