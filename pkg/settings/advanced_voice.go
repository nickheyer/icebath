package settings

import "time"

// Audio formats a speech program may write
var commandAudioFormats = []Choice{{"mp3", "MP3", ""}, {"wav", "WAV", ""}, {"ogg", "Ogg", ""}, {"flac", "FLAC", ""}, {"m4a", "M4A", ""}, {"aac", "AAC", ""}, {"amr", "AMR", ""}, {"opus", "Opus", ""}}

// Text formats a recognition program may write
var commandTextFormats = []Choice{{"txt", "Text", "Plain text."}, {"json", "JSON", "A JSON document."}, {"srt", "SRT", "SubRip subtitles."}, {"vtt", "VTT", "WebVTT subtitles."}}

// Character cap of one speech request to a service, unset for the service's own limit
func maxTextLength(key, label, service string, order int) *Int {
	return &Int{Meta: Meta{Key: key, Label: label, Short: "Most text " + service + " speaks in one request.", Help: "Longer text is split into several requests.", Group: GroupVoice, Order: order, Advanced: true},
		Null: "the service's own limit", Min: at(1), Unit: "chars"}
}

// Key for the model that adds tone tags to speech text
var AuxiliaryTtsAudioTagsApiKey = Register(&String{Meta: Meta{Key: "auxiliary.tts_audio_tags.api_key", Label: "Tag helper key", Short: "Key for the model that adds tone tags to speech text.", Group: GroupVoice, Order: 1000, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint for the model that adds tone tags to speech
var AuxiliaryTtsAudioTagsBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.tts_audio_tags.base_url", Label: "Tag helper endpoint", Short: "Custom endpoint for the model that adds tone tags to speech.", Group: GroupVoice, Order: 1010, Advanced: true}, Default: ""})

// Extra request fields sent to the tone tagging model
var AuxiliaryTtsAudioTagsExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.tts_audio_tags.extra_body", Label: "Tag helper fields", Short: "Extra request fields sent to the tone tagging model.", Group: GroupVoice, Order: 1020, Advanced: true}})

// Model that adds tone tags to speech text
var AuxiliaryTtsAudioTagsModel = Register(&String{Meta: Meta{Key: "auxiliary.tts_audio_tags.model", Label: "Tag helper model", Short: "Model that adds tone tags to speech text.", Help: "Empty picks one for you.", Group: GroupVoice, Order: 1030, Advanced: true}, Default: ""})

// Service for the model that adds tone tags
var AuxiliaryTtsAudioTagsProvider = Register(&String{Meta: Meta{Key: "auxiliary.tts_audio_tags.provider", Label: "Tag helper provider", Short: "Service for the model that adds tone tags.", Help: "Auto picks one for you.", Group: GroupVoice, Order: 1040, Advanced: true}, Default: "auto"})

// How long the tone tagging model thinks
var AuxiliaryTtsAudioTagsReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.tts_audio_tags.reasoning_effort", Label: "Tag helper effort", Short: "How long the tone tagging model thinks.", Group: GroupVoice, Order: 1050, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// How long to wait for the tone tagging model before giving up
var AuxiliaryTtsAudioTagsTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.tts_audio_tags.timeout", Label: "Tag helper timeout", Short: "How long to wait for the tone tagging model before giving up.", Group: GroupVoice, Order: 1060, Advanced: true}, Default: 30 * time.Second})

// Key for the model that answers in voice conversations
var AuxiliaryVoiceChatApiKey = Register(&String{Meta: Meta{Key: "auxiliary.voice_chat.api_key", Label: "Voice chat key", Short: "Key for the model that answers in voice conversations.", Group: GroupVoice, Order: 1070, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint for the model that answers in voice conversations
var AuxiliaryVoiceChatBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.voice_chat.base_url", Label: "Voice chat endpoint", Short: "Custom endpoint for the model that answers in voice conversations.", Group: GroupVoice, Order: 1080, Advanced: true}, Default: ""})

// Extra request fields sent to the voice conversation model
var AuxiliaryVoiceChatExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.voice_chat.extra_body", Label: "Voice chat fields", Short: "Extra request fields sent to the voice conversation model.", Group: GroupVoice, Order: 1090, Advanced: true}})

// Model that answers in voice conversations
var AuxiliaryVoiceChatModel = Register(&String{Meta: Meta{Key: "auxiliary.voice_chat.model", Label: "Voice chat model", Short: "Model that answers in voice conversations.", Help: "Empty picks one for you.", Group: GroupVoice, Order: 1100, Advanced: true}, Default: ""})

// Service for the voice conversation model
var AuxiliaryVoiceChatProvider = Register(&String{Meta: Meta{Key: "auxiliary.voice_chat.provider", Label: "Voice chat provider", Short: "Service for the voice conversation model.", Help: "Auto picks one for you.", Group: GroupVoice, Order: 1110, Advanced: true}, Default: "auto"})

// How long the voice conversation model thinks (default: off)
var AuxiliaryVoiceChatReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.voice_chat.reasoning_effort", Label: "Voice chat effort", Short: "How long the voice conversation model thinks (default: off).", Help: "Less thinking answers sooner.", Group: GroupVoice, Order: 1120, Advanced: true}, Default: "none", Values: reasoningChoices})

// How long to wait for a voice reply before giving up
var AuxiliaryVoiceChatTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.voice_chat.timeout", Label: "Voice chat timeout", Short: "How long to wait for a voice reply before giving up.", Group: GroupVoice, Order: 1130, Advanced: true}, Default: 2 * time.Minute})

// Quiet audio kept around speech when trimming before upload
var SttCloudTrimKeepMs = Register(&Duration{Meta: Meta{Key: "stt.cloud_trim_keep_ms", Label: "Trim padding", Short: "Quiet audio kept around speech when trimming before upload.", Group: GroupVoice, Order: 1140, Advanced: true}, Default: 300 * time.Millisecond, Unit: time.Millisecond})

// Cut silence from recordings before sending them to a cloud service
var SttCloudTrimSilence = Register(&Bool{Meta: Meta{Key: "stt.cloud_trim_silence", Label: "Trim silence", Short: "Cut silence from recordings before sending them to a cloud service.", Group: GroupVoice, Order: 1150, Advanced: true}, Default: true})

// Loudness below which audio counts as silence when trimming
var SttCloudTrimThresholdDb = Register(&Int{Meta: Meta{Key: "stt.cloud_trim_threshold_db", Label: "Trim threshold", Short: "Loudness below which audio counts as silence when trimming.", Group: GroupVoice, Order: 1160, Advanced: true}, Default: -40, Unit: "dB"})

// Custom endpoint for DeepInfra speech recognition
var SttDeepinfraBaseUrl = Register(&String{Meta: Meta{Key: "stt.deepinfra.base_url", Label: "DeepInfra recognition endpoint", Short: "Custom endpoint for DeepInfra speech recognition.", Group: GroupVoice, Order: 1165, Advanced: true}, Null: "the provider endpoint"})

// Speech recognition model used with DeepInfra
var SttDeepinfraModel = Register(&String{Meta: Meta{Key: "stt.deepinfra.model", Label: "DeepInfra model", Short: "Speech recognition model used with DeepInfra.", Help: "Empty uses the service's default.", Group: GroupVoice, Order: 1170, Advanced: true}, Default: ""})

// Custom endpoint for ElevenLabs speech recognition
var SttElevenlabsBaseUrl = Register(&String{Meta: Meta{Key: "stt.elevenlabs.base_url", Label: "ElevenLabs endpoint", Short: "Custom endpoint for ElevenLabs speech recognition.", Group: GroupVoice, Order: 1180, Advanced: true, Env: "ELEVENLABS_STT_BASE_URL"}, Null: "the provider endpoint"})

// Have ElevenLabs mark who is speaking in transcripts
var SttElevenlabsDiarize = Register(&Bool{Meta: Meta{Key: "stt.elevenlabs.diarize", Label: "Label speakers", Short: "Have ElevenLabs mark who is speaking in transcripts.", Group: GroupVoice, Order: 1190, Advanced: true}, Default: false})

// Language code for ElevenLabs transcripts
var SttElevenlabsLanguageCode = Register(&String{Meta: Meta{Key: "stt.elevenlabs.language_code", Label: "ElevenLabs language", Short: "Language code for ElevenLabs transcripts.", Help: "Empty detects it.", Group: GroupVoice, Order: 1200, Advanced: true}, Default: ""})

// Speech recognition model used with ElevenLabs
var SttElevenlabsModelId = Register(&String{Meta: Meta{Key: "stt.elevenlabs.model_id", Label: "ElevenLabs model", Short: "Speech recognition model used with ElevenLabs.", Group: GroupVoice, Order: 1210, Advanced: true}, Default: "scribe_v2"})

// Mark non-speech sounds such as laughter in ElevenLabs transcripts
var SttElevenlabsTagAudioEvents = Register(&Bool{Meta: Meta{Key: "stt.elevenlabs.tag_audio_events", Label: "Tag sounds", Short: "Mark non-speech sounds such as laughter in ElevenLabs transcripts.", Group: GroupVoice, Order: 1220, Advanced: true}, Default: false})

// Custom endpoint for Groq speech recognition
var SttGroqBaseUrl = Register(&String{Meta: Meta{Key: "stt.groq.base_url", Label: "Groq endpoint", Short: "Custom endpoint for Groq speech recognition.", Group: GroupVoice, Order: 1225, Advanced: true, Env: "GROQ_BASE_URL"}, Null: "the provider endpoint"})

// Language hint for Groq transcripts
var SttGroqLanguage = Register(&String{Meta: Meta{Key: "stt.groq.language", Label: "Groq language", Short: "Language hint for Groq transcripts.", Help: "Empty detects it.", Group: GroupVoice, Order: 1230, Advanced: true}, Default: ""})

// Speech recognition model used with Groq
var SttGroqModel = Register(&String{Meta: Meta{Key: "stt.groq.model", Label: "Groq model", Short: "Speech recognition model used with Groq.", Group: GroupVoice, Order: 1240, Advanced: true}, Default: "whisper-large-v3-turbo"})

// Language of your speech, as a language code such as en
var SttLanguage = Register(&String{Meta: Meta{Key: "stt.language", Label: "Recognition language", Short: "Language of your speech, as a language code such as en.", Group: GroupVoice, Order: 1250, Help: "Used when the chosen service has no language of its own.", Env: "ICEBATH_LOCAL_STT_LANGUAGE", Advanced: true}, Null: "detected from the speech"})

// Number format the on-device recognizer computes in
var SttLocalComputeType = Register(&String{Meta: Meta{Key: "stt.local.compute_type", Label: "Local compute type", Short: "Number format the on-device recognizer computes in, such as int8.", Help: "Auto picks one for the device.", Group: GroupVoice, Order: 1255, Advanced: true}, Default: "auto"})

// Where on-device recognition runs
var SttLocalDevice = Register(&String{Meta: Meta{Key: "stt.local.device", Label: "Local device", Short: "Where on-device recognition runs, such as cpu or cuda.", Help: "Auto uses the graphics card when it works.", Group: GroupVoice, Order: 1257, Advanced: true}, Default: "auto"})

// Text that primes the on-device recognizer, such as names or jargon
var SttLocalInitialPrompt = Register(&String{Meta: Meta{Key: "stt.local.initial_prompt", Label: "Local prompt", Short: "Text that primes the on-device recognizer, such as names or jargon.", Group: GroupVoice, Order: 1260, Advanced: true}, Default: ""})

// Language hint for on-device recognition
var SttLocalLanguage = Register(&String{Meta: Meta{Key: "stt.local.language", Label: "Local language", Short: "Language hint for on-device recognition.", Help: "Empty detects it.", Group: GroupVoice, Order: 1270, Advanced: true}, Default: ""})

// Average confidence below which on-device text is retried
var SttLocalLogprobThreshold = Register(&Float{Meta: Meta{Key: "stt.local.logprob_threshold", Label: "Confidence cutoff", Short: "Average confidence below which on-device text is retried.", Group: GroupVoice, Order: 1280, Advanced: true}, Default: -1})

// Size of the on-device speech recognition model
var SttLocalModel = Register(&String{Meta: Meta{Key: "stt.local.model", Label: "Local model", Short: "Size of the on-device speech recognition model.", Group: GroupVoice, Order: 1290, Advanced: true}, Default: "base"})

// Chance of no speech above which a clip is treated as silence
var SttLocalNoSpeechProbThreshold = Register(&Float{Meta: Meta{Key: "stt.local.no_speech_prob_threshold", Label: "No-speech cutoff", Short: "Chance of no speech above which a clip is treated as silence.", Group: GroupVoice, Order: 1300, Advanced: true}, Default: 0.6})

// Free the on-device recognizer after this long unused
var SttLocalUnloadAfterIdleSeconds = Register(&Duration{Meta: Meta{Key: "stt.local.unload_after_idle_seconds", Label: "Unload when idle", Short: "Free the on-device recognizer after this long unused.", Group: GroupVoice, Order: 1310, Help: "Zero keeps it loaded.", Advanced: true}, Default: 0})

// Detect speech and skip silent parts before on-device recognition
var SttLocalVad = Register(&Bool{Meta: Meta{Key: "stt.local.vad", Label: "Skip silence", Short: "Detect speech and skip silent parts before on-device recognition.", Group: GroupVoice, Order: 1320, Advanced: true}, Default: true})

// How long a pause must last to count as a break in speech
var SttLocalVadMinSilenceMs = Register(&Duration{Meta: Meta{Key: "stt.local.vad_min_silence_ms", Label: "Silence length", Short: "How long a pause must last to count as a break in speech.", Group: GroupVoice, Order: 1330, Advanced: true}, Default: 500 * time.Millisecond, Unit: time.Millisecond})

// Program that transcribes a recording on this machine
var SttLocalCommand = Register(&String{Meta: Meta{Key: "stt.local_command", Label: "Local command", Short: "Program that transcribes a recording on this machine.", Help: "Write {input_path}, {output_dir}, {language} and {model} where they belong; it must leave a .txt file in {output_dir}.", Env: "ICEBATH_LOCAL_STT_COMMAND", Group: GroupVoice, Order: 1335, Advanced: true}, Null: "the bundled whisper.cpp engine, else a whisper program on PATH"})

// Language hint for Mistral transcripts
var SttMistralLanguage = Register(&String{Meta: Meta{Key: "stt.mistral.language", Label: "Mistral language", Short: "Language hint for Mistral transcripts.", Help: "Empty detects it.", Group: GroupVoice, Order: 1340, Advanced: true}, Default: ""})

// Speech recognition model used with Mistral
var SttMistralModel = Register(&String{Meta: Meta{Key: "stt.mistral.model", Label: "Mistral model", Short: "Speech recognition model used with Mistral.", Group: GroupVoice, Order: 1350, Advanced: true}, Default: "voxtral-mini-latest"})

// Key for OpenAI speech recognition
var SttOpenaiApiKey = Register(&String{Meta: Meta{Key: "stt.openai.api_key", Label: "OpenAI recognition key", Short: "Key for OpenAI speech recognition.", Group: GroupVoice, Order: 1360, Advanced: true}, Masked: true, Null: "the provider key from the environment"})

// Custom endpoint for OpenAI-compatible speech recognition
var SttOpenaiBaseUrl = Register(&String{Meta: Meta{Key: "stt.openai.base_url", Label: "OpenAI recognition endpoint", Short: "Custom endpoint for OpenAI-compatible speech recognition.", Group: GroupVoice, Order: 1370, Advanced: true, Env: "STT_OPENAI_BASE_URL"}, Null: "the provider endpoint"})

// Language hint for OpenAI transcripts
var SttOpenaiLanguage = Register(&String{Meta: Meta{Key: "stt.openai.language", Label: "OpenAI language", Short: "Language hint for OpenAI transcripts.", Help: "Empty detects it.", Group: GroupVoice, Order: 1380, Advanced: true}, Default: ""})

// How many times to retry a failed recognition request
var SttOpenaiMaxRetries = Register(&Int{Meta: Meta{Key: "stt.openai.max_retries", Label: "Retry attempts", Short: "How many times to retry a failed recognition request.", Group: GroupVoice, Order: 1390, Advanced: true}, Default: 1})

// Speech recognition model used with OpenAI
var SttOpenaiModel = Register(&String{Meta: Meta{Key: "stt.openai.model", Label: "OpenAI model", Short: "Speech recognition model used with OpenAI.", Group: GroupVoice, Order: 1400, Advanced: true}, Default: "whisper-1"})

// Model used for live transcription with OpenAI
var SttOpenaiStreamingModel = Register(&String{Meta: Meta{Key: "stt.openai.streaming_model", Label: "OpenAI live model", Short: "Model used for live transcription with OpenAI.", Group: GroupVoice, Order: 1410, Advanced: true}, Default: "gpt-live-transcribe"})

// How long to wait for OpenAI to transcribe before giving up
var SttOpenaiTimeout = Register(&Duration{Meta: Meta{Key: "stt.openai.timeout", Label: "OpenAI timeout", Short: "How long to wait for OpenAI to transcribe before giving up.", Group: GroupVoice, Order: 1420, Advanced: true}, Default: time.Minute})

// Text that primes cloud recognition, such as names or jargon
var SttPrompt = Register(&String{Meta: Meta{Key: "stt.prompt", Label: "Recognition prompt", Short: "Text that primes cloud recognition, such as names or jargon.", Group: GroupVoice, Order: 1430, Advanced: true}, Null: "no static prompt"})

// Your own speech recognition programs, by the name stt.provider picks them with
var SttProviders = Register(&Map{Meta: Meta{Key: "stt.providers", Label: "Recognition commands", Short: "Your own speech recognition programs, by name.", Help: "Set Recognition service to a name here to use it. A program may also sit directly under stt by its name.", Group: GroupVoice, Order: 1435, Advanced: true},
	Beside: true,
	Value: &Object{Meta: Meta{Key: "stt.providers.*", Label: "Recognition command", Short: "One program that turns a recording into text."}, Fields: []Entry{
		&String{Meta: Meta{Key: "type", Label: "Kind", Short: "Kind of program; only command entries run."}, Null: "command"},
		&String{Meta: Meta{Key: "command", Label: "Command", Short: "Shell command run for each recording."}, Default: ""},
		&String{Meta: Meta{Key: "language", Label: "Language", Short: "Language code passed to the command."}, Null: "the recognition language"},
		&String{Meta: Meta{Key: "model", Label: "Model", Short: "Model name passed to the command."}, Default: ""},
		&Enum{Meta: Meta{Key: "output_format", Label: "Output format", Short: "Format of the text file the command writes."}, Default: "txt", Values: commandTextFormats},
		&Enum{Meta: Meta{Key: "format", Label: "Format", Short: "Format of the text file, before Output format."}, Null: "Output format", Values: commandTextFormats},
		&Duration{Meta: Meta{Key: "timeout", Label: "Time limit", Short: "How long the command may stay silent before it is stopped."}, Null: "Time limit in seconds, else five minutes", Min: at(time.Second)},
		&Duration{Meta: Meta{Key: "timeout_seconds", Label: "Time limit in seconds", Short: "The same limit, used when Time limit is not set."}, Null: "five minutes", Min: at(time.Second)},
		&Bool{Meta: Meta{Key: "normalize", Label: "Convert audio", Short: "Hand the command 16 kHz mono audio instead of the original file."}, Default: false},
		&List{Meta: Meta{Key: "env_passthrough", Label: "Passed variables", Short: "Environment variables the command may see."}},
	}}})

// Show words as you speak when the chosen service supports it
var SttStreaming = Register(&Bool{Meta: Meta{Key: "stt.streaming", Label: "Live transcription", Short: "Show words as you speak when the chosen service supports it.", Group: GroupVoice, Order: 1440, Advanced: true}, Default: false})

// Custom endpoint for xAI speech recognition
var SttXaiBaseUrl = Register(&String{Meta: Meta{Key: "stt.xai.base_url", Label: "xAI recognition endpoint", Short: "Custom endpoint for xAI speech recognition.", Group: GroupVoice, Order: 1450, Advanced: true, Env: "XAI_STT_BASE_URL"}, Null: "the provider endpoint"})

// Have xAI mark who is speaking in transcripts
var SttXaiDiarize = Register(&Bool{Meta: Meta{Key: "stt.xai.diarize", Label: "xAI speakers", Short: "Have xAI mark who is speaking in transcripts.", Group: GroupVoice, Order: 1453, Advanced: true}, Default: false})

// Have xAI tidy numbers and punctuation in transcripts
var SttXaiFormat = Register(&Bool{Meta: Meta{Key: "stt.xai.format", Label: "xAI formatting", Short: "Have xAI tidy numbers and punctuation in transcripts.", Help: "Applies when a language is set.", Group: GroupVoice, Order: 1456, Advanced: true}, Default: true})

// Language hint for xAI transcripts
var SttXaiLanguage = Register(&String{Meta: Meta{Key: "stt.xai.language", Label: "xAI language", Short: "Language hint for xAI transcripts.", Help: "Empty detects it.", Group: GroupVoice, Order: 1460, Advanced: true}, Default: ""})

// Speech recognition model used with xAI
var SttXaiModel = Register(&String{Meta: Meta{Key: "stt.xai.model", Label: "xAI model", Short: "Speech recognition model used with xAI.", Help: "Empty uses the service's default.", Group: GroupVoice, Order: 1470, Advanced: true}, Default: ""})

// Custom endpoint for DeepInfra speech
var TtsDeepinfraBaseUrl = Register(&String{Meta: Meta{Key: "tts.deepinfra.base_url", Label: "DeepInfra endpoint", Short: "Custom endpoint for DeepInfra speech.", Group: GroupVoice, Order: 1475, Advanced: true}, Null: "the provider endpoint"})

// Most text DeepInfra speaks in one request
var TtsDeepinfraMaxTextLength = Register(maxTextLength("tts.deepinfra.max_text_length", "DeepInfra text limit", "DeepInfra", 1477))

// Speech model used with DeepInfra
var TtsDeepinfraModel = Register(&String{Meta: Meta{Key: "tts.deepinfra.model", Label: "DeepInfra model", Short: "Speech model used with DeepInfra.", Help: "Empty uses the service's default.", Group: GroupVoice, Order: 1480, Advanced: true}, Default: ""})

// How fast DeepInfra speaks
var TtsDeepinfraSpeed = Register(&Float{Meta: Meta{Key: "tts.deepinfra.speed", Label: "DeepInfra speed", Short: "How fast DeepInfra speaks.", Help: "1 is normal.", Group: GroupVoice, Order: 1490, Advanced: true}, Null: "the general speech speed", Min: at(0.25), Max: at(4.0)})

// Voice used with DeepInfra
var TtsDeepinfraVoice = Register(&String{Meta: Meta{Key: "tts.deepinfra.voice", Label: "DeepInfra voice", Short: "Voice used with DeepInfra.", Group: GroupVoice, Order: 1500, Advanced: true}, Default: "default"})

// Largest audio file sent to each chat app, by app name
var TtsDeliveryProfiles = Register(&Map{Meta: Meta{Key: "tts.delivery_profiles", Label: "Audio upload limits", Short: "Largest speech file sent to each chat app, by app name.", Help: "Longer speech is sent as several files; default covers apps not named.", Group: GroupVoice, Order: 1505, Advanced: true},
	Value: &Object{Meta: Meta{Key: "tts.delivery_profiles.*", Label: "Upload limit", Short: "Size limits of speech files sent to one chat app."}, Fields: []Entry{
		&Int{Meta: Meta{Key: "max_file_bytes", Label: "Largest file", Short: "Most bytes one speech file may have."}, Null: "the app's own limit", Min: at(1), Unit: "bytes"},
		&Float{Meta: Meta{Key: "safety_ratio", Label: "Safety margin", Short: "Share of the largest file size actually filled."}, Null: "0.85", Min: at(0.01), Max: at(1.0)},
	}}})

// Most text Edge speaks in one request
var TtsEdgeMaxTextLength = Register(maxTextLength("tts.edge.max_text_length", "Edge text limit", "Edge", 1507))

// How fast the Edge voice speaks
var TtsEdgeSpeed = Register(&Float{Meta: Meta{Key: "tts.edge.speed", Label: "Edge speed", Short: "How fast the Edge voice speaks.", Help: "1 is normal.", Group: GroupVoice, Order: 1510, Advanced: true}, Null: "the general speech speed"})

// Voice used with Edge speech
var TtsEdgeVoice = Register(&String{Meta: Meta{Key: "tts.edge.voice", Label: "Edge voice", Short: "Voice used with Edge speech.", Group: GroupVoice, Order: 1520, Advanced: true}, Default: "en-US-AriaNeural"})

// Custom endpoint for ElevenLabs speech
var TtsElevenlabsBaseUrl = Register(&String{Meta: Meta{Key: "tts.elevenlabs.base_url", Label: "ElevenLabs endpoint", Short: "Custom endpoint for ElevenLabs speech.", Group: GroupVoice, Order: 1530, Advanced: true}, Null: "the provider endpoint"})

// Most text ElevenLabs speaks in one request
var TtsElevenlabsMaxTextLength = Register(maxTextLength("tts.elevenlabs.max_text_length", "ElevenLabs text limit", "ElevenLabs", 1535))

// Speech model used with ElevenLabs
var TtsElevenlabsModelId = Register(&String{Meta: Meta{Key: "tts.elevenlabs.model_id", Label: "ElevenLabs model", Short: "Speech model used with ElevenLabs.", Group: GroupVoice, Order: 1540, Advanced: true}, Default: "eleven_multilingual_v2"})

// Speech model for ElevenLabs when speaking as text arrives
var TtsElevenlabsStreamingModelId = Register(&String{Meta: Meta{Key: "tts.elevenlabs.streaming_model_id", Label: "ElevenLabs live model", Short: "Speech model for ElevenLabs when speaking as text arrives.", Group: GroupVoice, Order: 1550, Advanced: true}, Null: "the ElevenLabs model"})

// Voice used with ElevenLabs, by its ID
var TtsElevenlabsVoiceId = Register(&String{Meta: Meta{Key: "tts.elevenlabs.voice_id", Label: "ElevenLabs voice", Short: "Voice used with ElevenLabs, by its ID.", Group: GroupVoice, Order: 1560, Advanced: true}, Default: "pNInz6obpgDQGcFmaJgB"})

// Add tone cues to text before Gemini speaks it
var TtsGeminiAudioTags = Register(&Bool{Meta: Meta{Key: "tts.gemini.audio_tags", Label: "Gemini tone tags", Short: "Add tone cues to text before Gemini speaks it.", Group: GroupVoice, Order: 1570, Advanced: true}, Default: false})

// Custom endpoint for Gemini speech
var TtsGeminiBaseUrl = Register(&String{Meta: Meta{Key: "tts.gemini.base_url", Label: "Gemini endpoint", Short: "Custom endpoint for Gemini speech.", Group: GroupVoice, Order: 1580, Advanced: true}, Null: "the provider endpoint"})

// Most text Gemini speaks in one request
var TtsGeminiMaxTextLength = Register(maxTextLength("tts.gemini.max_text_length", "Gemini text limit", "Gemini", 1585))

// Speech model used with Gemini
var TtsGeminiModel = Register(&String{Meta: Meta{Key: "tts.gemini.model", Label: "Gemini model", Short: "Speech model used with Gemini.", Group: GroupVoice, Order: 1590, Advanced: true}, Default: "gemini-2.5-flash-preview-tts"})

// Path to a text file describing how Gemini should sound
var TtsGeminiPersonaPromptFile = Register(&String{Meta: Meta{Key: "tts.gemini.persona_prompt_file", Label: "Gemini persona file", Short: "Path to a text file describing how Gemini should sound.", Group: GroupVoice, Order: 1600, Advanced: true}, Default: ""})

// Voice used with Gemini
var TtsGeminiVoice = Register(&String{Meta: Meta{Key: "tts.gemini.voice", Label: "Gemini voice", Short: "Voice used with Gemini.", Group: GroupVoice, Order: 1610, Advanced: true}, Default: "Kore"})

// How long a local voice stays loaded after use
var TtsKeepWarmSeconds = Register(&Duration{Meta: Meta{Key: "tts.keep_warm_seconds", Label: "Keep voice loaded", Short: "How long a local voice stays loaded after use.", Help: "Zero unloads it at once.", Group: GroupVoice, Order: 1620, Advanced: true}, Default: time.Minute})

// Tidy text before KittenTTS speaks it
var TtsKittenttsCleanText = Register(&Bool{Meta: Meta{Key: "tts.kittentts.clean_text", Label: "Clean text", Short: "Tidy text before KittenTTS speaks it.", Group: GroupVoice, Order: 1630, Advanced: true}, Default: true})

// Most text KittenTTS speaks in one request
var TtsKittenttsMaxTextLength = Register(maxTextLength("tts.kittentts.max_text_length", "KittenTTS text limit", "KittenTTS", 1635))

// On-device speech model used with KittenTTS
var TtsKittenttsModel = Register(&String{Meta: Meta{Key: "tts.kittentts.model", Label: "KittenTTS model", Short: "On-device speech model used with KittenTTS.", Group: GroupVoice, Order: 1640, Advanced: true}, Default: "KittenML/kitten-tts-nano-0.8-int8"})

// How fast KittenTTS speaks
var TtsKittenttsSpeed = Register(&Float{Meta: Meta{Key: "tts.kittentts.speed", Label: "KittenTTS speed", Short: "How fast KittenTTS speaks.", Help: "1 is normal.", Group: GroupVoice, Order: 1650, Advanced: true}, Null: "the general speech speed"})

// Voice used with KittenTTS
var TtsKittenttsVoice = Register(&String{Meta: Meta{Key: "tts.kittentts.voice", Label: "KittenTTS voice", Short: "Voice used with KittenTTS.", Group: GroupVoice, Order: 1660, Advanced: true}, Default: "Jasper"})

// Custom endpoint for MiniMax speech
var TtsMinimaxBaseUrl = Register(&String{Meta: Meta{Key: "tts.minimax.base_url", Label: "MiniMax endpoint", Short: "Custom endpoint for MiniMax speech.", Group: GroupVoice, Order: 1670, Advanced: true}, Null: "the endpoint of the region"})

// Audio quality of MiniMax speech, in bits per second
var TtsMinimaxBitrate = Register(&Int{Meta: Meta{Key: "tts.minimax.bitrate", Label: "MiniMax bitrate", Short: "Audio quality of MiniMax speech, in bits per second.", Group: GroupVoice, Order: 1680, Advanced: true}, Default: 128000, Unit: "bps"})

// Emotion MiniMax puts in its speech, such as neutral or happy
var TtsMinimaxEmotion = Register(&String{Meta: Meta{Key: "tts.minimax.emotion", Label: "MiniMax emotion", Short: "Emotion MiniMax puts in its speech, such as neutral or happy.", Group: GroupVoice, Order: 1690, Advanced: true}, Default: "neutral"})

// Account group ID sent to MiniMax
var TtsMinimaxGroupId = Register(&String{Meta: Meta{Key: "tts.minimax.group_id", Label: "MiniMax group ID", Short: "Account group ID sent to MiniMax.", Group: GroupVoice, Order: 1700, Advanced: true}, Null: "none"})

// Most text MiniMax speaks in one request
var TtsMinimaxMaxTextLength = Register(maxTextLength("tts.minimax.max_text_length", "MiniMax text limit", "MiniMax", 1705))

// Speech model used with MiniMax
var TtsMinimaxModel = Register(&String{Meta: Meta{Key: "tts.minimax.model", Label: "MiniMax model", Short: "Speech model used with MiniMax.", Group: GroupVoice, Order: 1710, Advanced: true}, Default: "speech-02-hd"})

// Raise or lower the MiniMax voice pitch
var TtsMinimaxPitch = Register(&Int{Meta: Meta{Key: "tts.minimax.pitch", Label: "MiniMax pitch", Short: "Raise or lower the MiniMax voice pitch.", Group: GroupVoice, Order: 1720, Advanced: true}, Default: 0})

// Which MiniMax service region to use
var TtsMinimaxRegion = Register(&Enum{Meta: Meta{Key: "tts.minimax.region", Label: "MiniMax region", Short: "Which MiniMax service region to use.", Group: GroupVoice, Order: 1730, Advanced: true}, Null: "global, or China when only a China key is set", Values: []Choice{{Value: "global", Label: "Global", Meaning: "International service."}, {Value: "cn", Label: "China", Meaning: "Mainland China service."}}})

// Audio sample rate for MiniMax speech, in Hz
var TtsMinimaxSampleRate = Register(&Int{Meta: Meta{Key: "tts.minimax.sample_rate", Label: "MiniMax sample rate", Short: "Audio sample rate for MiniMax speech, in Hz.", Group: GroupVoice, Order: 1740, Advanced: true}, Default: 32000, Unit: "Hz"})

// How fast MiniMax speaks
var TtsMinimaxSpeed = Register(&Float{Meta: Meta{Key: "tts.minimax.speed", Label: "MiniMax speed", Short: "How fast MiniMax speaks.", Help: "1 is normal.", Group: GroupVoice, Order: 1750, Advanced: true}, Null: "the general speech speed"})

// Voice used with MiniMax, by its ID
var TtsMinimaxVoiceId = Register(&String{Meta: Meta{Key: "tts.minimax.voice_id", Label: "MiniMax voice", Short: "Voice used with MiniMax, by its ID.", Group: GroupVoice, Order: 1760, Advanced: true}, Default: "English_expressive_narrator"})

// How loud MiniMax speaks
var TtsMinimaxVol = Register(&Float{Meta: Meta{Key: "tts.minimax.vol", Label: "MiniMax volume", Short: "How loud MiniMax speaks.", Group: GroupVoice, Order: 1770, Advanced: true}, Default: 1})

// Custom endpoint for Mistral speech
var TtsMistralBaseUrl = Register(&String{Meta: Meta{Key: "tts.mistral.base_url", Label: "Mistral endpoint", Short: "Custom endpoint for Mistral speech.", Group: GroupVoice, Order: 1780, Advanced: true}, Null: "the provider endpoint"})

// Most text Mistral speaks in one request
var TtsMistralMaxTextLength = Register(maxTextLength("tts.mistral.max_text_length", "Mistral text limit", "Mistral", 1785))

// Speech model used with Mistral
var TtsMistralModel = Register(&String{Meta: Meta{Key: "tts.mistral.model", Label: "Mistral model", Short: "Speech model used with Mistral.", Group: GroupVoice, Order: 1790, Advanced: true}, Default: "voxtral-mini-tts-2603"})

// Voice used with Mistral, by its ID
var TtsMistralVoiceId = Register(&String{Meta: Meta{Key: "tts.mistral.voice_id", Label: "Mistral voice", Short: "Voice used with Mistral, by its ID.", Group: GroupVoice, Order: 1800, Advanced: true}, Default: "c69964a6-ab8b-4f8a-9465-ec0925096ec8"})

// Speech model for plugin voice services
var TtsModel = Register(&String{Meta: Meta{Key: "tts.model", Label: "Speech model", Short: "Speech model for plugin voice services.", Group: GroupVoice, Order: 1810, Advanced: true}, Null: "the plugin provider's model"})

// Where NeuTTS runs, such as cpu or cuda
var TtsNeuttsDevice = Register(&String{Meta: Meta{Key: "tts.neutts.device", Label: "NeuTTS device", Short: "Where NeuTTS runs, such as cpu or cuda.", Group: GroupVoice, Order: 1820, Advanced: true}, Default: "cpu"})

// Most text NeuTTS speaks in one request
var TtsNeuttsMaxTextLength = Register(maxTextLength("tts.neutts.max_text_length", "NeuTTS text limit", "NeuTTS", 1825))

// On-device speech model used with NeuTTS
var TtsNeuttsModel = Register(&String{Meta: Meta{Key: "tts.neutts.model", Label: "NeuTTS model", Short: "On-device speech model used with NeuTTS.", Group: GroupVoice, Order: 1830, Advanced: true}, Default: "neuphonic/neutts-air-q4-gguf"})

// Path to a sample recording of the voice to copy
var TtsNeuttsRefAudio = Register(&String{Meta: Meta{Key: "tts.neutts.ref_audio", Label: "NeuTTS sample audio", Short: "Path to a sample recording of the voice to copy.", Group: GroupVoice, Order: 1840, Advanced: true}, Default: ""})

// Path to the transcript of the sample recording
var TtsNeuttsRefText = Register(&String{Meta: Meta{Key: "tts.neutts.ref_text", Label: "NeuTTS sample text", Short: "Path to the transcript of the sample recording.", Group: GroupVoice, Order: 1850, Advanced: true}, Default: ""})

// Key for OpenAI speech
var TtsOpenaiApiKey = Register(&String{Meta: Meta{Key: "tts.openai.api_key", Label: "OpenAI speech key", Short: "Key for OpenAI speech.", Group: GroupVoice, Order: 1860, Advanced: true}, Masked: true, Null: "the provider key from the environment"})

// Custom endpoint for OpenAI-compatible speech
var TtsOpenaiBaseUrl = Register(&String{Meta: Meta{Key: "tts.openai.base_url", Label: "OpenAI speech endpoint", Short: "Custom endpoint for OpenAI-compatible speech.", Group: GroupVoice, Order: 1870, Advanced: true}, Null: "the provider endpoint"})

// Consent statement sent when using a custom cloned voice
var TtsOpenaiConsentAttestation = Register(&String{Meta: Meta{Key: "tts.openai.consent_attestation", Label: "Voice consent", Short: "Consent statement sent when using a custom cloned voice.", Group: GroupVoice, Order: 1880, Advanced: true}, Default: ""})

// Language hint sent with OpenAI-compatible speech
var TtsOpenaiLanguage = Register(&String{Meta: Meta{Key: "tts.openai.language", Label: "OpenAI language", Short: "Language hint sent with OpenAI-compatible speech.", Group: GroupVoice, Order: 1890, Advanced: true}, Null: "no language hint"})

// Most text OpenAI speaks in one request
var TtsOpenaiMaxTextLength = Register(maxTextLength("tts.openai.max_text_length", "OpenAI text limit", "OpenAI", 1895))

// Speech model used with OpenAI
var TtsOpenaiModel = Register(&String{Meta: Meta{Key: "tts.openai.model", Label: "OpenAI model", Short: "Speech model used with OpenAI.", Group: GroupVoice, Order: 1900, Advanced: true}, Default: "gpt-4o-mini-tts"})

// Audio sample rate when OpenAI speech plays as it arrives, in Hz
var TtsOpenaiPcmSampleRate = Register(&Int{Meta: Meta{Key: "tts.openai.pcm_sample_rate", Label: "Live sample rate", Short: "Audio sample rate when OpenAI speech plays as it arrives, in Hz.", Group: GroupVoice, Order: 1910, Advanced: true}, Default: 24000, Unit: "Hz"})

// How fast OpenAI speaks
var TtsOpenaiSpeed = Register(&Float{Meta: Meta{Key: "tts.openai.speed", Label: "OpenAI speed", Short: "How fast OpenAI speaks.", Group: GroupVoice, Order: 1920, Help: "1 is normal, from 0.25 to 4.", Advanced: true}, Null: "the general speech speed", Min: at(0.25), Max: at(4.0)})

// Voice used with OpenAI
var TtsOpenaiVoice = Register(&String{Meta: Meta{Key: "tts.openai.voice", Label: "OpenAI voice", Short: "Voice used with OpenAI.", Group: GroupVoice, Order: 1930, Advanced: true}, Default: "alloy"})

// Trade quality for faster start when speech plays as it arrives
var TtsOptimizeStreamingLatency = Register(&Int{Meta: Meta{Key: "tts.optimize_streaming_latency", Label: "Live latency boost", Short: "Trade quality for faster start when speech plays as it arrives.", Group: GroupVoice, Order: 1940, Help: "Used by xAI speech when its own setting is absent.", Advanced: true}, Null: "no boost", Min: at(0), Max: at(2)})

// Audio format produced by command-based voices
var TtsOutputFormat = Register(&String{Meta: Meta{Key: "tts.output_format", Label: "Output format", Short: "Audio format produced by command-based voices.", Group: GroupVoice, Order: 1950, Advanced: true}, Null: "the default format of command providers"})

// Stretches or shortens Piper speech
var TtsPiperLengthScale = Register(&Float{Meta: Meta{Key: "tts.piper.length_scale", Label: "Piper pace", Short: "Stretches or shortens Piper speech.", Help: "Higher is slower.", Group: GroupVoice, Order: 1960, Advanced: true}, Null: "the voice's own value"})

// Most text Piper speaks in one request
var TtsPiperMaxTextLength = Register(maxTextLength("tts.piper.max_text_length", "Piper text limit", "Piper", 1965))

// How much natural variation Piper adds to the voice
var TtsPiperNoiseScale = Register(&Float{Meta: Meta{Key: "tts.piper.noise_scale", Label: "Piper variation", Short: "How much natural variation Piper adds to the voice.", Group: GroupVoice, Order: 1970, Advanced: true}, Null: "the voice's own value"})

// How much Piper varies the timing of words
var TtsPiperNoiseWScale = Register(&Float{Meta: Meta{Key: "tts.piper.noise_w_scale", Label: "Piper timing variation", Short: "How much Piper varies the timing of words.", Group: GroupVoice, Order: 1980, Advanced: true}, Null: "the voice's own value"})

// Level out loudness in Piper speech
var TtsPiperNormalizeAudio = Register(&Bool{Meta: Meta{Key: "tts.piper.normalize_audio", Label: "Even out volume", Short: "Level out loudness in Piper speech.", Group: GroupVoice, Order: 1990, Advanced: true}, Default: true})

// Which speaker to use in a multi-speaker Piper voice
var TtsPiperSpeakerId = Register(&Int{Meta: Meta{Key: "tts.piper.speaker_id", Label: "Piper speaker", Short: "Which speaker to use in a multi-speaker Piper voice.", Group: GroupVoice, Order: 2000, Advanced: true}, Default: 0})

// Run Piper on the graphics card instead of the processor
var TtsPiperUseCuda = Register(&Bool{Meta: Meta{Key: "tts.piper.use_cuda", Label: "Piper on GPU", Short: "Run Piper on the graphics card instead of the processor.", Group: GroupVoice, Order: 2010, Advanced: true}, Default: false})

// Voice used with Piper
var TtsPiperVoice = Register(&String{Meta: Meta{Key: "tts.piper.voice", Label: "Piper voice", Short: "Voice used with Piper.", Group: GroupVoice, Order: 2020, Advanced: true}, Default: "en_US-lessac-medium"})

// Folder where Piper voices are stored and downloaded
var TtsPiperVoicesDir = Register(&String{Meta: Meta{Key: "tts.piper.voices_dir", Label: "Piper voices folder", Short: "Folder where Piper voices are stored and downloaded.", Group: GroupVoice, Order: 2030, Advanced: true}, Null: "the voices folder in the icebath home"})

// How loud Piper speaks
var TtsPiperVolume = Register(&Float{Meta: Meta{Key: "tts.piper.volume", Label: "Piper volume", Short: "How loud Piper speaks.", Help: "1 is unchanged.", Group: GroupVoice, Order: 2040, Advanced: true}, Default: 1})

// Your own speech programs, by the name tts.provider picks them with
var TtsProviders = Register(&Map{Meta: Meta{Key: "tts.providers", Label: "Speech commands", Short: "Your own speech programs, by name.", Help: "Set Speech voice to a name here to use it. A program may also sit directly under tts by its name.", Group: GroupVoice, Order: 2045, Advanced: true},
	Beside: true,
	Value: &Object{Meta: Meta{Key: "tts.providers.*", Label: "Speech command", Short: "One program that turns text into an audio file."}, Fields: []Entry{
		&String{Meta: Meta{Key: "type", Label: "Kind", Short: "Kind of program; only command entries run."}, Null: "command"},
		&String{Meta: Meta{Key: "command", Label: "Command", Short: "Shell command run for each piece of text."}, Default: ""},
		&Enum{Meta: Meta{Key: "output_format", Label: "Output format", Short: "Audio format the command writes."}, Default: "mp3", Values: commandAudioFormats},
		&Enum{Meta: Meta{Key: "format", Label: "Format", Short: "Audio format the command writes, before Output format."}, Null: "Output format", Values: commandAudioFormats},
		&String{Meta: Meta{Key: "voice", Label: "Voice", Short: "Voice name passed to the command."}, Default: ""},
		&String{Meta: Meta{Key: "model", Label: "Model", Short: "Model name passed to the command."}, Default: ""},
		&Float{Meta: Meta{Key: "speed", Label: "Speed", Short: "Speed passed to the command."}, Null: "the general speech speed"},
		&Duration{Meta: Meta{Key: "timeout", Label: "Time limit", Short: "How long the command may stay silent before it is stopped."}, Null: "Time limit in seconds, else two minutes", Min: at(time.Second)},
		&Duration{Meta: Meta{Key: "timeout_seconds", Label: "Time limit in seconds", Short: "The same limit, used when Time limit is not set."}, Null: "two minutes", Min: at(time.Second)},
		&Bool{Meta: Meta{Key: "voice_compatible", Label: "Voice bubbles", Short: "Send the audio as a voice message where chat apps allow."}, Default: false},
		&Int{Meta: Meta{Key: "max_text_length", Label: "Text limit", Short: "Most text the command speaks at once."}, Default: 5000, Min: at(1), Unit: "chars"},
		&List{Meta: Meta{Key: "env_passthrough", Label: "Passed variables", Short: "Environment variables the command may see."}},
		&String{Meta: Meta{Key: "warm_command", Label: "Warm command", Short: "Shell command run when speech is about to be needed."}, Default: ""},
		&String{Meta: Meta{Key: "release_command", Label: "Release command", Short: "Shell command run when speech is no longer needed."}, Default: ""},
	}}})

// How fast voices speak
var TtsSpeed = Register(&Float{Meta: Meta{Key: "tts.speed", Label: "Speech speed", Short: "How fast voices speak.", Group: GroupVoice, Order: 2050, Help: "1 is normal. Services with their own speed setting use that instead.", Advanced: true}, Null: "normal speed"})

// Smallest piece of text spoken at once while streaming speech
var TtsStreamingMinLen = Register(&Int{Meta: Meta{Key: "tts.streaming.min_len", Label: "Live chunk size", Short: "Smallest piece of text spoken at once while streaming speech.", Group: GroupVoice, Order: 2060, Advanced: true}, Default: 20, Min: at(1), Unit: "chars"})

// Service for speech that plays as the text arrives
var TtsStreamingProvider = Register(&String{Meta: Meta{Key: "tts.streaming.provider", Label: "Live speech service", Short: "Service for speech that plays as the text arrives.", Group: GroupVoice, Order: 2070, Advanced: true}, Null: "the Speech voice service"})

// Voice for plugin voice services
var TtsVoice = Register(&String{Meta: Meta{Key: "tts.voice", Label: "Speech voice name", Short: "Voice for plugin voice services.", Group: GroupVoice, Order: 2080, Advanced: true}, Null: "the plugin provider's voice"})

// Add expressive speech tags to text before xAI speaks it
var TtsXaiAutoSpeechTags = Register(&Bool{Meta: Meta{Key: "tts.xai.auto_speech_tags", Label: "xAI auto tags", Short: "Add expressive speech tags to text before xAI speaks it.", Group: GroupVoice, Order: 2090, Advanced: true}, Default: false})

// Custom endpoint for xAI speech
var TtsXaiBaseUrl = Register(&String{Meta: Meta{Key: "tts.xai.base_url", Label: "xAI speech endpoint", Short: "Custom endpoint for xAI speech.", Group: GroupVoice, Order: 2100, Advanced: true}, Null: "the provider endpoint"})

// Audio quality of xAI speech, in bits per second
var TtsXaiBitRate = Register(&Int{Meta: Meta{Key: "tts.xai.bit_rate", Label: "xAI bitrate", Short: "Audio quality of xAI speech, in bits per second.", Group: GroupVoice, Order: 2110, Advanced: true}, Default: 128000, Unit: "bps"})

// Language xAI speaks in, as a language code
var TtsXaiLanguage = Register(&String{Meta: Meta{Key: "tts.xai.language", Label: "xAI language", Short: "Language xAI speaks in, as a language code.", Group: GroupVoice, Order: 2120, Advanced: true}, Default: "en"})

// Most text xAI speaks in one request
var TtsXaiMaxTextLength = Register(maxTextLength("tts.xai.max_text_length", "xAI text limit", "xAI", 2125))

// Trade quality for faster start in xAI speech
var TtsXaiOptimizeStreamingLatency = Register(&Int{Meta: Meta{Key: "tts.xai.optimize_streaming_latency", Label: "xAI latency boost", Short: "Trade quality for faster start in xAI speech.", Help: "Zero turns it off.", Group: GroupVoice, Order: 2130, Advanced: true}, Null: "the general live latency boost", Min: at(0), Max: at(2)})

// Audio sample rate for xAI speech, in Hz
var TtsXaiSampleRate = Register(&Int{Meta: Meta{Key: "tts.xai.sample_rate", Label: "xAI sample rate", Short: "Audio sample rate for xAI speech, in Hz.", Group: GroupVoice, Order: 2140, Advanced: true}, Default: 24000, Unit: "Hz"})

// How fast xAI speaks
var TtsXaiSpeed = Register(&Float{Meta: Meta{Key: "tts.xai.speed", Label: "xAI speed", Short: "How fast xAI speaks.", Help: "1 is normal.", Group: GroupVoice, Order: 2150, Advanced: true}, Null: "the general speech speed"})

// Custom endpoint for xAI speech that plays as it arrives
var TtsXaiStreamingUrl = Register(&String{Meta: Meta{Key: "tts.xai.streaming_url", Label: "xAI live endpoint", Short: "Custom endpoint for xAI speech that plays as it arrives.", Group: GroupVoice, Order: 2160, Advanced: true}, Null: "the provider streaming endpoint"})

// Have xAI spell out numbers and symbols before speaking
var TtsXaiTextNormalization = Register(&Bool{Meta: Meta{Key: "tts.xai.text_normalization", Label: "xAI text cleanup", Short: "Have xAI spell out numbers and symbols before speaking.", Group: GroupVoice, Order: 2170, Advanced: true}, Null: "the service default"})

// Voice used with xAI
var TtsXaiVoiceId = Register(&String{Meta: Meta{Key: "tts.xai.voice_id", Label: "xAI voice", Short: "Voice used with xAI.", Group: GroupVoice, Order: 2180, Advanced: true}, Default: "eve"})

// Stop spoken replies when you start talking over them
var VoiceBargeIn = Register(&Bool{Meta: Meta{Key: "voice.barge_in", Label: "Interrupt by talking", Short: "Stop spoken replies when you start talking over them.", Group: GroupVoice, Order: 2190, Advanced: true}, Default: true})

// Time after a reply starts before talking can interrupt it
var VoiceBargeInGraceSeconds = Register(&Duration{Meta: Meta{Key: "voice.barge_in_grace_seconds", Label: "Interrupt grace", Short: "Time after a reply starts before talking can interrupt it.", Group: GroupVoice, Order: 2200, Advanced: true}, Default: 500 * time.Millisecond})

// How much louder than the room you must be to interrupt
var VoiceBargeInThresholdMultiplier = Register(&Float{Meta: Meta{Key: "voice.barge_in_threshold_multiplier", Label: "Interrupt sensitivity", Short: "How much louder than the room you must be to interrupt.", Group: GroupVoice, Order: 2210, Help: "Zero turns the loudness check off.", Advanced: true}, Default: 3})

// Play a beep when recording starts and stops
var VoiceBeepEnabled = Register(&Bool{Meta: Meta{Key: "voice.beep_enabled", Label: "Recording beep", Short: "Play a beep when recording starts and stops.", Group: GroupVoice, Order: 2220, Advanced: true}, Default: true})

// How loud the recording beep is, from 0 to 1
var VoiceBeepVolume = Register(&Float{Meta: Meta{Key: "voice.beep_volume", Label: "Beep volume", Short: "How loud the recording beep is, from 0 to 1.", Group: GroupVoice, Order: 2230, Advanced: true}, Default: 0.3, Min: at(0.0), Max: at(1.0), DefaultBelow: true, DefaultAbove: true})

// Each step of voice capture and recognition printed to the error output
var VoiceDebug = Register(&Bool{Meta: Meta{Key: "voice.debug", Label: "Voice debug log", Short: "Print each step of voice capture and recognition to the error output.", Group: GroupVoice, Order: 2235, Advanced: true, Env: "ICEBATH_VOICE_DEBUG"}, Default: false})

// Voice input stops listening after this long
var VoiceMaxRecordingSeconds = Register(&Duration{Meta: Meta{Key: "voice.max_recording_seconds", Label: "Longest recording", Short: "Voice input stops listening after this long.", Group: GroupVoice, Order: 2240, Advanced: true}, Default: 2 * time.Minute})

// Seconds of quiet before recording stops on its own
var VoiceSilenceDuration = Register(&Float{Meta: Meta{Key: "voice.silence_duration", Label: "Silence length", Short: "Seconds of quiet before recording stops on its own.", Group: GroupVoice, Order: 2250, Advanced: true}, Default: 3, Unit: "seconds"})

// Loudness below which audio counts as quiet
var VoiceSilenceThreshold = Register(&Int{Meta: Meta{Key: "voice.silence_threshold", Label: "Silence level", Short: "Loudness below which audio counts as quiet.", Help: "Raise it in noisy rooms.", Group: GroupVoice, Order: 2260, Advanced: true}, Default: 200})

// Spoken words that end voice conversation mode
var VoiceStopPhrases = Register(&List{Meta: Meta{Key: "voice.stop_phrases", Label: "Stop phrases", Short: "Spoken words that end voice conversation mode.", Group: GroupVoice, Order: 2270, Advanced: true}, Default: []any{"stop"}})

// Play soft blips while waiting for a reply
var VoiceThinkingSound = Register(&Bool{Meta: Meta{Key: "voice.thinking_sound", Label: "Thinking sound", Short: "Play soft blips while waiting for a reply.", Group: GroupVoice, Order: 2280, Advanced: true}, Default: true})

// Checks in a row that must hear the Wake word before it triggers
var WakeWordConfirmationFrames = Register(&Int{Meta: Meta{Key: "wake_word.confirmation_frames", Label: "Confirmation frames", Short: "Checks in a row that must hear the Wake word before it triggers.", Group: GroupVoice, Order: 2290, Advanced: true}, Default: 3, Min: at(1), Max: at(10)})

// Microphone to listen on, by number or name
var WakeWordInputDevice = Register(&String{Meta: Meta{Key: "wake_word.input_device", Label: "Wake microphone", Short: "Microphone to listen on, by number or name.", Group: GroupVoice, Order: 2300, Advanced: true}, Null: "the default microphone"})

// Path to the model file used for the Wake word
var WakeWordOpenwakewordModel = Register(&String{Meta: Meta{Key: "wake_word.openwakeword.model", Label: "Wake model file", Short: "Path to the model file used for the Wake word.", Group: GroupVoice, Order: 2310, Advanced: true}, Default: ""})

// Phrase that wakes the assistant
var WakeWordPhrase = Register(&String{Meta: Meta{Key: "wake_word.phrase", Label: "Wake phrase", Short: "Phrase that wakes the assistant.", Group: GroupVoice, Order: 2320, Advanced: true}, Default: "hey icebath"})

// Built-in keyword Porcupine listens for
var WakeWordPorcupineKeyword = Register(&String{Meta: Meta{Key: "wake_word.porcupine.keyword", Label: "Porcupine keyword", Short: "Built-in keyword Porcupine listens for.", Group: GroupVoice, Order: 2330, Advanced: true}, Default: "jarvis"})

// Each profile can have its own Wake word and answers to it
var WakeWordProfileRouting = Register(&Bool{Meta: Meta{Key: "wake_word.profile_routing", Label: "Route by profile", Short: "Each profile can have its own Wake word and answers to it.", Group: GroupVoice, Order: 2340, Advanced: true}, Default: true})

// Engine that listens for the Wake word
var WakeWordProvider = Register(&String{Meta: Meta{Key: "wake_word.provider", Label: "Wake engine", Short: "Engine that listens for the Wake word.", Help: "Auto picks one for you.", Group: GroupVoice, Order: 2350, Advanced: true}, Default: "auto"})

// How easily the Wake word triggers, from 0 to 1
var WakeWordSensitivity = Register(&Float{Meta: Meta{Key: "wake_word.sensitivity", Label: "Wake sensitivity", Short: "How easily the Wake word triggers, from 0 to 1.", Group: GroupVoice, Order: 2360, Help: "Higher catches more but triggers by mistake more often.", Advanced: true}, Default: 0.6, Min: at(0.0), Max: at(1.0)})

// Folder holding the Sherpa Wake word model
var WakeWordSherpaModelDir = Register(&String{Meta: Meta{Key: "wake_word.sherpa.model_dir", Label: "Sherpa model folder", Short: "Folder holding the Sherpa Wake word model.", Group: GroupVoice, Order: 2370, Advanced: true}, Default: ""})

// Begin a new session each time the Wake word is heard
var WakeWordStartNewSession = Register(&Bool{Meta: Meta{Key: "wake_word.start_new_session", Label: "Start fresh session", Short: "Begin a new session each time the Wake word is heard.", Group: GroupVoice, Order: 2380, Advanced: true}, Default: true})

// Which screen runs the Wake word listener
var WakeWordSurface = Register(&Enum{Meta: Meta{Key: "wake_word.surface", Label: "Wake listens in", Short: "Which screen runs the Wake word listener.", Group: GroupVoice, Order: 2390, Advanced: true}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "Any screen."}, {Value: "cli", Label: "CLI", Meaning: "Plain terminal only."}, {Value: "tui", Label: "TUI", Meaning: "Terminal UI only."}, {Value: "gui", Label: "GUI", Meaning: "Desktop app only."}}})
