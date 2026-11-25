package sherpaonnx

type cOfflineTransducerModelConfig struct {
	encoder *byte
	decoder *byte
	joiner  *byte
}

type cOfflineParaformerModelConfig struct {
	model *byte
}

type cOfflineNemoEncDecCtcModelConfig struct {
	model *byte
}

type cOfflineWhisperModelConfig struct {
	encoder                 *byte
	decoder                 *byte
	language                *byte
	task                    *byte
	tailPaddings            int32
	enableTokenTimestamps   int32
	enableSegmentTimestamps int32
}

type cOfflineTdnnModelConfig struct {
	model *byte
}

type cOfflineSenseVoiceModelConfig struct {
	model    *byte
	language *byte
	useItn   int32
}

type cOfflineMoonshineModelConfig struct {
	preprocessor    *byte
	encoder         *byte
	uncachedDecoder *byte
	cachedDecoder   *byte
	mergedDecoder   *byte
}

type cOfflineFireRedAsrModelConfig struct {
	encoder *byte
	decoder *byte
}

type cOfflineDolphinModelConfig struct {
	model *byte
}

type cOfflineZipformerCtcModelConfig struct {
	model *byte
}

type cOfflineCanaryModelConfig struct {
	encoder *byte
	decoder *byte
	srcLang *byte
	tgtLang *byte
	usePnc  int32
}

type cOfflineWenetCtcModelConfig struct {
	model *byte
}

type cOfflineOmnilingualAsrCtcModelConfig struct {
	model *byte
}

type cOfflineMedAsrCtcModelConfig struct {
	model *byte
}

type cOfflineFunASRNanoModelConfig struct {
	encoderAdaptor *byte
	llm            *byte
	embedding      *byte
	tokenizer      *byte
	systemPrompt   *byte
	userPrompt     *byte
	maxNewTokens   int32
	temperature    float32
	topP           float32
	seed           int32
	language       *byte
	itn            int32
	hotwords       *byte
}

type cOfflineFireRedAsrCtcModelConfig struct {
	model *byte
}

type cOfflineQwen3ASRModelConfig struct {
	convFrontend *byte
	encoder      *byte
	decoder      *byte
	tokenizer    *byte
	maxTotalLen  int32
	maxNewTokens int32
	temperature  float32
	topP         float32
	seed         int32
	hotwords     *byte
}

type cOfflineCohereTranscribeModelConfig struct {
	encoder  *byte
	decoder  *byte
	language *byte
	usePunct int32
	useItn   int32
}

type cOfflineModelConfig struct {
	transducer       cOfflineTransducerModelConfig
	paraformer       cOfflineParaformerModelConfig
	nemoCtc          cOfflineNemoEncDecCtcModelConfig
	whisper          cOfflineWhisperModelConfig
	tdnn             cOfflineTdnnModelConfig
	tokens           *byte
	numThreads       int32
	debug            int32
	provider         *byte
	modelType        *byte
	modelingUnit     *byte
	bpeVocab         *byte
	telespeechCtc    *byte
	senseVoice       cOfflineSenseVoiceModelConfig
	moonshine        cOfflineMoonshineModelConfig
	fireRedAsr       cOfflineFireRedAsrModelConfig
	dolphin          cOfflineDolphinModelConfig
	zipformerCtc     cOfflineZipformerCtcModelConfig
	canary           cOfflineCanaryModelConfig
	wenetCtc         cOfflineWenetCtcModelConfig
	omnilingual      cOfflineOmnilingualAsrCtcModelConfig
	medasr           cOfflineMedAsrCtcModelConfig
	funasrNano       cOfflineFunASRNanoModelConfig
	fireRedAsrCtc    cOfflineFireRedAsrCtcModelConfig
	qwen3Asr         cOfflineQwen3ASRModelConfig
	cohereTranscribe cOfflineCohereTranscribeModelConfig
}

type cOfflineLMConfig struct {
	model *byte
	scale float32
}

type cHomophoneReplacerConfig struct {
	dictDir  *byte
	lexicon  *byte
	ruleFsts *byte
}

type cOfflineRecognizerConfig struct {
	featConfig     cFeatureConfig
	modelConfig    cOfflineModelConfig
	lmConfig       cOfflineLMConfig
	decodingMethod *byte
	maxActivePaths int32
	hotwordsFile   *byte
	hotwordsScore  float32
	ruleFsts       *byte
	ruleFars       *byte
	blankPenalty   float32
	hr             cHomophoneReplacerConfig
}

type cOfflineRecognizerResult struct {
	text              *byte
	timestamps        uintptr
	count             int32
	tokens            *byte
	tokensArr         uintptr
	json              *byte
	lang              *byte
	emotion           *byte
	event             *byte
	durations         uintptr
	ysLogProbs        uintptr
	segmentTimestamps uintptr
	segmentDurations  uintptr
	segmentTexts      *byte
	segmentTextsArr   uintptr
	segmentCount      int32
}

type cOfflineTtsVitsModelConfig struct {
	model       *byte
	lexicon     *byte
	tokens      *byte
	dataDir     *byte
	noiseScale  float32
	noiseScaleW float32
	lengthScale float32
	dictDir     *byte
}

type cOfflineTtsMatchaModelConfig struct {
	acousticModel *byte
	vocoder       *byte
	lexicon       *byte
	tokens        *byte
	dataDir       *byte
	noiseScale    float32
	lengthScale   float32
	dictDir       *byte
}

type cOfflineTtsKokoroModelConfig struct {
	model       *byte
	voices      *byte
	tokens      *byte
	dataDir     *byte
	lengthScale float32
	dictDir     *byte
	lexicon     *byte
	lang        *byte
}

type cOfflineTtsKittenModelConfig struct {
	model       *byte
	voices      *byte
	tokens      *byte
	dataDir     *byte
	lengthScale float32
}

type cOfflineTtsZipvoiceModelConfig struct {
	tokens        *byte
	encoder       *byte
	decoder       *byte
	vocoder       *byte
	dataDir       *byte
	lexicon       *byte
	featScale     float32
	tShift        float32
	targetRms     float32
	guidanceScale float32
}

type cOfflineTtsPocketModelConfig struct {
	lmFlow                      *byte
	lmMain                      *byte
	encoder                     *byte
	decoder                     *byte
	textConditioner             *byte
	vocabJson                   *byte
	tokenScoresJson             *byte
	voiceEmbeddingCacheCapacity int32
}

type cOfflineTtsSupertonicModelConfig struct {
	durationPredictor *byte
	textEncoder       *byte
	vectorEstimator   *byte
	vocoder           *byte
	ttsJson           *byte
	unicodeIndexer    *byte
	voiceStyle        *byte
}

type cOfflineTtsModelConfig struct {
	vits       cOfflineTtsVitsModelConfig
	numThreads int32
	debug      int32
	provider   *byte
	matcha     cOfflineTtsMatchaModelConfig
	kokoro     cOfflineTtsKokoroModelConfig
	kitten     cOfflineTtsKittenModelConfig
	zipvoice   cOfflineTtsZipvoiceModelConfig
	pocket     cOfflineTtsPocketModelConfig
	supertonic cOfflineTtsSupertonicModelConfig
}

type cOfflineTtsConfig struct {
	model           cOfflineTtsModelConfig
	ruleFsts        *byte
	maxNumSentences int32
	ruleFars        *byte
	silenceScale    float32
}

type cGenerationConfig struct {
	silenceScale        float32
	speed               float32
	sid                 int32
	referenceAudio      uintptr
	referenceAudioLen   int32
	referenceSampleRate int32
	referenceText       *byte
	numSteps            int32
	extra               *byte
}

type cGeneratedAudio struct {
	samples    uintptr
	n          int32
	sampleRate int32
}

type cSileroVadModelConfig struct {
	model              *byte
	threshold          float32
	minSilenceDuration float32
	minSpeechDuration  float32
	windowSize         int32
	maxSpeechDuration  float32
}

type cTenVadModelConfig struct {
	model              *byte
	threshold          float32
	minSilenceDuration float32
	minSpeechDuration  float32
	windowSize         int32
	maxSpeechDuration  float32
}

type cVadModelConfig struct {
	sileroVad  cSileroVadModelConfig
	sampleRate int32
	numThreads int32
	provider   *byte
	debug      int32
	tenVad     cTenVadModelConfig
}

type cSpeechSegment struct {
	start   int32
	samples uintptr
	n       int32
}
