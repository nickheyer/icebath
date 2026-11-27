package sherpa

// C structs of the sherpa-onnx C API this binding fills and reads, field for field in declaration order

type cFeatureConfig struct {
	sampleRate int32
	featureDim int32
}

// SherpaOnnxOnlineModelConfig with its nested model structs flattened
type cOnlineModelConfig struct {
	transducerEncoder  *byte
	transducerDecoder  *byte
	transducerJoiner   *byte
	paraformerEncoder  *byte
	paraformerDecoder  *byte
	zipformer2CtcModel *byte
	tokens             *byte
	numThreads         int32
	provider           *byte
	debug              int32
	modelType          *byte
	modelingUnit       *byte
	bpeVocab           *byte
	tokensBuf          *byte
	tokensBufSize      int32
	nemoCtcModel       *byte
	tOneCtcModel       *byte
}

type cKeywordSpotterConfig struct {
	featConfig        cFeatureConfig
	modelConfig       cOnlineModelConfig
	maxActivePaths    int32
	numTrailingBlanks int32
	keywordsScore     float32
	keywordsThreshold float32
	keywordsFile      *byte
	keywordsBuf       *byte
	keywordsBufSize   int32
}

type cKeywordResult struct {
	keyword    uintptr
	tokens     uintptr
	tokensArr  uintptr
	count      int32
	timestamps uintptr
	startTime  float32
	json       uintptr
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
	vocabJSON                   *byte
	tokenScoresJSON             *byte
	voiceEmbeddingCacheCapacity int32
}

type cOfflineTtsSupertonicModelConfig struct {
	durationPredictor *byte
	textEncoder       *byte
	vectorEstimator   *byte
	vocoder           *byte
	ttsJSON           *byte
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
