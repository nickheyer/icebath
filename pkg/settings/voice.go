package settings

// Talking to the agent instead of typing
var VoiceInput = Register(&Bool{Meta: Meta{Key: "voice.input_enabled", Label: "Voice input",
	Short: "Talk instead of typing: a key records you and sends the words.",
	Group: GroupVoice, Order: 10, Scopes: Global | Session, Slash: "voice"},
	Default: false})

// Replies read aloud or sent as voice messages
var SpeakReplies = Register(&Bool{Meta: Meta{Key: "voice.speak_replies", Label: "Speak replies",
	Short: "Read the assistant's replies aloud, or send voice messages in chats.",
	Help:  "The terminal UI speaks while voice input is on. A chat keeps its own choice once /voice is used there.",
	Group: GroupVoice, Order: 20, Scopes: Global | Session, Slash: "speak"},
	Default: false})

// Key that starts and stops a recording
var RecordKey = Register(&String{Meta: Meta{Key: "voice.record_key", Label: "Record key",
	Short: "Extra key that starts and stops a recording (default: none).",
	Help:  "The leader key followed by R always records; a binding in your keys file wins over this.",
	Group: GroupVoice, Order: 30},
	Default: ""})

// Whether a transcript is sent or left to edit
var VoiceSubmit = Register(&Enum{Meta: Meta{Key: "voice.submit_mode", Label: "Send or draft",
	Short: "Send what you said at once, or leave it in the input box to edit.",
	Group: GroupVoice, Order: 40},
	Default: "direct",
	Values: []Choice{
		{"direct", "Send", "Send the words as soon as you stop."},
		{"draft", "Draft", "Put the words in the input box."},
	}})

// Service that speaks for icebath
var SpeechVoice = Register(&Enum{Meta: Meta{Key: "tts.provider", Label: "Speech voice",
	Short: "Which service speaks for icebath.",
	Help:  "Each service's voice is under advanced.",
	Group: GroupVoice, Order: 50},
	Default: "edge", Open: true,
	Values: []Choice{
		{"edge", "Edge", "Free Microsoft voices, no key."},
		{"elevenlabs", "ElevenLabs", ""}, {"openai", "OpenAI", ""}, {"minimax", "MiniMax", ""}, {"xai", "xAI", ""},
		{"mistral", "Mistral", ""}, {"gemini", "Gemini", ""}, {"deepinfra", "DeepInfra", ""},
		{"neutts", "NeuTTS", "Runs on this machine."}, {"kittentts", "KittenTTS", "Runs on this machine."},
		{"piper", "Piper", "Runs on this machine."},
	}})

// Speech to text of recordings and voice messages
var SpeechRecognition = Register(&Bool{Meta: Meta{Key: "stt.enabled", Label: "Speech recognition",
	Short: "Turn your recordings and voice messages into text.",
	Group: GroupVoice, Order: 60},
	Default: true})

// Service that turns speech into text
var RecognitionService = Register(&Enum{Meta: Meta{Key: "stt.provider", Label: "Recognition service",
	Short: "Which service turns your speech into text.",
	Group: GroupVoice, Order: 61},
	Default: "local", Open: true,
	Values: []Choice{
		{"local", "This machine", "A local Whisper model, no key."},
		{"local_command", "Local command", "A program you name."},
		{"groq", "Groq", ""}, {"openai", "OpenAI", ""}, {"mistral", "Mistral", ""}, {"xai", "xAI", ""},
		{"elevenlabs", "ElevenLabs", ""}, {"deepinfra", "DeepInfra", ""},
	}})

// Transcript of a voice message posted back into the chat
var EchoTranscripts = Register(&Bool{Meta: Meta{Key: "stt.echo_transcripts", Label: "Show transcripts",
	Short: "Post the text of your voice messages back into the chat.",
	Group: GroupVoice, Order: 100, Advanced: true, Apply: Restart},
	Default: true})

// Hands free recording start
var WakeWord = Register(&Bool{Meta: Meta{Key: "wake_word.enabled", Label: "Wake word",
	Short: "Say the wake phrase to start recording without the keyboard.",
	Group: GroupVoice, Order: 70, Slash: "wake"},
	Default: false})

// Voice reply mode of one chat
var chatVoiceMode = &Enum{Meta: Meta{Key: "voice.chat_modes.*"},
	Values: []Choice{
		{"off", "Off", "Text replies only."},
		{"voice_only", "To voice", "A voice reply when you sent a voice message."},
		{"all", "Always", "A voice reply to every message."},
	}}

// Voice reply mode each chat chose with /voice, keyed by chat
var ChatVoiceModes = Register(&Map{Meta: Meta{Key: "voice.chat_modes", Label: "Voice per chat",
	Short: "Each chat's own choice of voice replies, set by /voice there.",
	Help:  "Chats missing here follow Speak replies.",
	Group: GroupChatApps, Order: 100, Advanced: true, State: true},
	Value: chatVoiceMode})
