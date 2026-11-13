// sherpa-onnx v1.13.8 sherpa-onnx/c-api/c-api.h with comments removed, Copyright (c) 2023 Xiaomi Corporation, Apache License 2.0

#ifndef SHERPA_ONNX_C_API_C_API_H_
#define SHERPA_ONNX_C_API_C_API_H_
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif
#if defined(__GNUC__)
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wattributes"
#endif
#if defined(_WIN32)
#if defined(SHERPA_ONNX_BUILD_SHARED_LIBS)
#define SHERPA_ONNX_EXPORT __declspec(dllexport)
#define SHERPA_ONNX_IMPORT __declspec(dllimport)
#else
#define SHERPA_ONNX_EXPORT
#define SHERPA_ONNX_IMPORT
#endif
#else  
#define SHERPA_ONNX_EXPORT __attribute__((visibility("default")))
#define SHERPA_ONNX_IMPORT SHERPA_ONNX_EXPORT
#endif  
#if defined(SHERPA_ONNX_BUILD_MAIN_LIB)
#define SHERPA_ONNX_API SHERPA_ONNX_EXPORT
#else
#define SHERPA_ONNX_API SHERPA_ONNX_IMPORT
#endif
#ifndef SHERPA_ONNX_DEPRECATED
#if defined(_MSC_VER)
#define SHERPA_ONNX_DEPRECATED(msg) __declspec(deprecated(msg))
#elif defined(__GNUC__) || defined(__clang__)
#define SHERPA_ONNX_DEPRECATED(msg) __attribute__((deprecated(msg)))
#else
#define SHERPA_ONNX_DEPRECATED(msg)
#endif
#endif
SHERPA_ONNX_API const char *SherpaOnnxGetVersionStr();
SHERPA_ONNX_API const char *SherpaOnnxGetGitSha1();
SHERPA_ONNX_API const char *SherpaOnnxGetGitDate();
SHERPA_ONNX_API const char *SherpaOnnxGetOnnxruntimeVersionStr();
SHERPA_ONNX_API int32_t SherpaOnnxFileExists(const char *filename);
typedef struct SherpaOnnxOnlineTransducerModelConfig {
  const char *encoder;
  const char *decoder;
  const char *joiner;
} SherpaOnnxOnlineTransducerModelConfig;
typedef struct SherpaOnnxOnlineParaformerModelConfig {
  const char *encoder;
  const char *decoder;
} SherpaOnnxOnlineParaformerModelConfig;
typedef struct SherpaOnnxOnlineZipformer2CtcModelConfig {
  const char *model;
} SherpaOnnxOnlineZipformer2CtcModelConfig;
typedef struct SherpaOnnxOnlineNemoCtcModelConfig {
  const char *model;
} SherpaOnnxOnlineNemoCtcModelConfig;
typedef struct SherpaOnnxOnlineToneCtcModelConfig {
  const char *model;
} SherpaOnnxOnlineToneCtcModelConfig;
typedef struct SherpaOnnxOnlineModelConfig {
  SherpaOnnxOnlineTransducerModelConfig transducer;
  SherpaOnnxOnlineParaformerModelConfig paraformer;
  SherpaOnnxOnlineZipformer2CtcModelConfig zipformer2_ctc;
  const char *tokens;
  int32_t num_threads;
  const char *provider;
  int32_t debug;
  const char *model_type;
  const char *modeling_unit;
  const char *bpe_vocab;
  const char *tokens_buf;
  int32_t tokens_buf_size;
  SherpaOnnxOnlineNemoCtcModelConfig nemo_ctc;
  SherpaOnnxOnlineToneCtcModelConfig t_one_ctc;
} SherpaOnnxOnlineModelConfig;
typedef struct SherpaOnnxFeatureConfig {
  int32_t sample_rate;
  int32_t feature_dim;
} SherpaOnnxFeatureConfig;
typedef struct SherpaOnnxOnlineCtcFstDecoderConfig {
  const char *graph;
  int32_t max_active;
} SherpaOnnxOnlineCtcFstDecoderConfig;
typedef struct SherpaOnnxHomophoneReplacerConfig {
  const char *dict_dir;
  const char *lexicon;
  const char *rule_fsts;
} SherpaOnnxHomophoneReplacerConfig;
typedef struct SherpaOnnxOnlineRecognizerConfig {
  SherpaOnnxFeatureConfig feat_config;
  SherpaOnnxOnlineModelConfig model_config;
  const char *decoding_method;
  int32_t max_active_paths;
  int32_t enable_endpoint;
  float rule1_min_trailing_silence;
  float rule2_min_trailing_silence;
  float rule3_min_utterance_length;
  const char *hotwords_file;
  float hotwords_score;
  SherpaOnnxOnlineCtcFstDecoderConfig ctc_fst_decoder_config;
  const char *rule_fsts;
  const char *rule_fars;
  float blank_penalty;
  const char *hotwords_buf;
  int32_t hotwords_buf_size;
  SherpaOnnxHomophoneReplacerConfig hr;
} SherpaOnnxOnlineRecognizerConfig;
typedef struct SherpaOnnxOnlineRecognizerResult {
  const char *text;
  const char *tokens;
  const char *const *tokens_arr;
  float *timestamps;
  int32_t count;
  const char *json;
} SherpaOnnxOnlineRecognizerResult;
typedef struct SherpaOnnxOnlineRecognizer SherpaOnnxOnlineRecognizer;
typedef struct SherpaOnnxOnlineStream SherpaOnnxOnlineStream;
SHERPA_ONNX_API const SherpaOnnxOnlineRecognizer *
SherpaOnnxCreateOnlineRecognizer(
    const SherpaOnnxOnlineRecognizerConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlineRecognizer(
    const SherpaOnnxOnlineRecognizer *recognizer);
SHERPA_ONNX_API const SherpaOnnxOnlineStream *SherpaOnnxCreateOnlineStream(
    const SherpaOnnxOnlineRecognizer *recognizer);
SHERPA_ONNX_API const SherpaOnnxOnlineStream *
SherpaOnnxCreateOnlineStreamWithHotwords(
    const SherpaOnnxOnlineRecognizer *recognizer, const char *hotwords);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlineStream(
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxOnlineStreamAcceptWaveform(
    const SherpaOnnxOnlineStream *stream, int32_t sample_rate,
    const float *samples, int32_t n);
SHERPA_ONNX_API int32_t
SherpaOnnxIsOnlineStreamReady(const SherpaOnnxOnlineRecognizer *recognizer,
                              const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDecodeOnlineStream(
    const SherpaOnnxOnlineRecognizer *recognizer,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDecodeMultipleOnlineStreams(
    const SherpaOnnxOnlineRecognizer *recognizer,
    const SherpaOnnxOnlineStream **streams, int32_t n);
SHERPA_ONNX_API const SherpaOnnxOnlineRecognizerResult *
SherpaOnnxGetOnlineStreamResult(const SherpaOnnxOnlineRecognizer *recognizer,
                                const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlineRecognizerResult(
    const SherpaOnnxOnlineRecognizerResult *r);
SHERPA_ONNX_API const char *SherpaOnnxGetOnlineStreamResultAsJson(
    const SherpaOnnxOnlineRecognizer *recognizer,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlineStreamResultJson(const char *s);
SHERPA_ONNX_API void SherpaOnnxOnlineStreamReset(
    const SherpaOnnxOnlineRecognizer *recognizer,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxOnlineStreamInputFinished(
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxOnlineStreamSetOption(
    const SherpaOnnxOnlineStream *stream, const char *key, const char *value);
SHERPA_ONNX_API const char *SherpaOnnxOnlineStreamGetOption(
    const SherpaOnnxOnlineStream *stream, const char *key);
SHERPA_ONNX_API int32_t SherpaOnnxOnlineStreamHasOption(
    const SherpaOnnxOnlineStream *stream, const char *key);
SHERPA_ONNX_API int32_t
SherpaOnnxOnlineStreamIsEndpoint(const SherpaOnnxOnlineRecognizer *recognizer,
                                 const SherpaOnnxOnlineStream *stream);
typedef struct SherpaOnnxDisplay SherpaOnnxDisplay;
SHERPA_ONNX_API const SherpaOnnxDisplay *SherpaOnnxCreateDisplay(
    int32_t max_word_per_line);
SHERPA_ONNX_API void SherpaOnnxDestroyDisplay(const SherpaOnnxDisplay *display);
SHERPA_ONNX_API void SherpaOnnxPrint(const SherpaOnnxDisplay *display,
                                     int32_t idx, const char *s);
typedef struct SherpaOnnxOfflineTransducerModelConfig {
  const char *encoder;
  const char *decoder;
  const char *joiner;
} SherpaOnnxOfflineTransducerModelConfig;
typedef struct SherpaOnnxOfflineParaformerModelConfig {
  const char *model;
} SherpaOnnxOfflineParaformerModelConfig;
typedef struct SherpaOnnxOfflineNemoEncDecCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineNemoEncDecCtcModelConfig;
typedef struct SherpaOnnxOfflineWhisperModelConfig {
  const char *encoder;
  const char *decoder;
  const char *language;
  const char *task;
  int32_t tail_paddings;
  int32_t enable_token_timestamps;
  int32_t enable_segment_timestamps;
} SherpaOnnxOfflineWhisperModelConfig;
typedef struct SherpaOnnxOfflineCanaryModelConfig {
  const char *encoder;
  const char *decoder;
  const char *src_lang;
  const char *tgt_lang;
  int32_t use_pnc;
} SherpaOnnxOfflineCanaryModelConfig;
typedef struct SherpaOnnxOfflineCohereTranscribeModelConfig {
  const char *encoder;
  const char *decoder;
  const char *language;
  int32_t use_punct;
  int32_t use_itn;
} SherpaOnnxOfflineCohereTranscribeModelConfig;
typedef struct SherpaOnnxOfflineFireRedAsrModelConfig {
  const char *encoder;
  const char *decoder;
} SherpaOnnxOfflineFireRedAsrModelConfig;
typedef struct SherpaOnnxOfflineFireRedAsrCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineFireRedAsrCtcModelConfig;
typedef struct SherpaOnnxOfflineMoonshineModelConfig {
  const char *preprocessor;
  const char *encoder;
  const char *uncached_decoder;
  const char *cached_decoder;
  const char *merged_decoder;
} SherpaOnnxOfflineMoonshineModelConfig;
typedef struct SherpaOnnxOfflineTdnnModelConfig {
  const char *model;
} SherpaOnnxOfflineTdnnModelConfig;
typedef struct SherpaOnnxOfflineLMConfig {
  const char *model;
  float scale;
} SherpaOnnxOfflineLMConfig;
typedef struct SherpaOnnxOfflineSenseVoiceModelConfig {
  const char *model;
  const char *language;
  int32_t use_itn;
} SherpaOnnxOfflineSenseVoiceModelConfig;
typedef struct SherpaOnnxOfflineDolphinModelConfig {
  const char *model;
} SherpaOnnxOfflineDolphinModelConfig;
typedef struct SherpaOnnxOfflineZipformerCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineZipformerCtcModelConfig;
typedef struct SherpaOnnxOfflineWenetCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineWenetCtcModelConfig;
typedef struct SherpaOnnxOfflineOmnilingualAsrCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineOmnilingualAsrCtcModelConfig;
typedef struct SherpaOnnxOfflineFunASRNanoModelConfig {
  const char *encoder_adaptor;
  const char *llm;
  const char *embedding;
  const char *tokenizer;
  const char *system_prompt;
  const char *user_prompt;
  int32_t max_new_tokens;
  float temperature;
  float top_p;
  int32_t seed;
  const char *language;
  int32_t itn;
  const char *hotwords;
} SherpaOnnxOfflineFunASRNanoModelConfig;
typedef struct SherpaOnnxOfflineQwen3ASRModelConfig {
  const char *conv_frontend;
  const char *encoder;
  const char *decoder;
  const char *tokenizer;
  int32_t max_total_len;
  int32_t max_new_tokens;
  float temperature;
  float top_p;
  int32_t seed;
  const char *hotwords;
} SherpaOnnxOfflineQwen3ASRModelConfig;
typedef struct SherpaOnnxOfflineMedAsrCtcModelConfig {
  const char *model;
} SherpaOnnxOfflineMedAsrCtcModelConfig;
typedef struct SherpaOnnxOfflineModelConfig {
  SherpaOnnxOfflineTransducerModelConfig transducer;
  SherpaOnnxOfflineParaformerModelConfig paraformer;
  SherpaOnnxOfflineNemoEncDecCtcModelConfig nemo_ctc;
  SherpaOnnxOfflineWhisperModelConfig whisper;
  SherpaOnnxOfflineTdnnModelConfig tdnn;
  const char *tokens;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
  const char *model_type;
  const char *modeling_unit;
  const char *bpe_vocab;
  const char *telespeech_ctc;
  SherpaOnnxOfflineSenseVoiceModelConfig sense_voice;
  SherpaOnnxOfflineMoonshineModelConfig moonshine;
  SherpaOnnxOfflineFireRedAsrModelConfig fire_red_asr;
  SherpaOnnxOfflineDolphinModelConfig dolphin;
  SherpaOnnxOfflineZipformerCtcModelConfig zipformer_ctc;
  SherpaOnnxOfflineCanaryModelConfig canary;
  SherpaOnnxOfflineWenetCtcModelConfig wenet_ctc;
  SherpaOnnxOfflineOmnilingualAsrCtcModelConfig omnilingual;
  SherpaOnnxOfflineMedAsrCtcModelConfig medasr;
  SherpaOnnxOfflineFunASRNanoModelConfig funasr_nano;
  SherpaOnnxOfflineFireRedAsrCtcModelConfig fire_red_asr_ctc;
  SherpaOnnxOfflineQwen3ASRModelConfig qwen3_asr;
  SherpaOnnxOfflineCohereTranscribeModelConfig cohere_transcribe;
} SherpaOnnxOfflineModelConfig;
typedef struct SherpaOnnxOfflineRecognizerConfig {
  SherpaOnnxFeatureConfig feat_config;
  SherpaOnnxOfflineModelConfig model_config;
  SherpaOnnxOfflineLMConfig lm_config;
  const char *decoding_method;
  int32_t max_active_paths;
  const char *hotwords_file;
  float hotwords_score;
  const char *rule_fsts;
  const char *rule_fars;
  float blank_penalty;
  SherpaOnnxHomophoneReplacerConfig hr;
} SherpaOnnxOfflineRecognizerConfig;
typedef struct SherpaOnnxOfflineRecognizer SherpaOnnxOfflineRecognizer;
typedef struct SherpaOnnxOfflineStream SherpaOnnxOfflineStream;
SHERPA_ONNX_API const SherpaOnnxOfflineRecognizer *
SherpaOnnxCreateOfflineRecognizer(
    const SherpaOnnxOfflineRecognizerConfig *config);
SHERPA_ONNX_API void SherpaOnnxOfflineRecognizerSetConfig(
    const SherpaOnnxOfflineRecognizer *recognizer,
    const SherpaOnnxOfflineRecognizerConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineRecognizer(
    const SherpaOnnxOfflineRecognizer *recognizer);
SHERPA_ONNX_API const SherpaOnnxOfflineStream *SherpaOnnxCreateOfflineStream(
    const SherpaOnnxOfflineRecognizer *recognizer);
SHERPA_ONNX_API const SherpaOnnxOfflineStream *
SherpaOnnxCreateOfflineStreamWithHotwords(
    const SherpaOnnxOfflineRecognizer *recognizer, const char *hotwords);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineStream(
    const SherpaOnnxOfflineStream *stream);
SHERPA_ONNX_API void SherpaOnnxAcceptWaveformOffline(
    const SherpaOnnxOfflineStream *stream, int32_t sample_rate,
    const float *samples, int32_t n);
SHERPA_ONNX_API void SherpaOnnxOfflineStreamSetOption(
    const SherpaOnnxOfflineStream *stream, const char *key, const char *value);
SHERPA_ONNX_API const char *SherpaOnnxOfflineStreamGetOption(
    const SherpaOnnxOfflineStream *stream, const char *key);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineStreamHasOption(
    const SherpaOnnxOfflineStream *stream, const char *key);
SHERPA_ONNX_API void SherpaOnnxDecodeOfflineStream(
    const SherpaOnnxOfflineRecognizer *recognizer,
    const SherpaOnnxOfflineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDecodeMultipleOfflineStreams(
    const SherpaOnnxOfflineRecognizer *recognizer,
    const SherpaOnnxOfflineStream **streams, int32_t n);
typedef struct SherpaOnnxOfflineRecognizerResult {
  const char *text;
  float *timestamps;
  int32_t count;
  const char *tokens;
  const char *const *tokens_arr;
  const char *json;
  const char *lang;
  const char *emotion;
  const char *event;
  float *durations;
  float *ys_log_probs;
  const float *segment_timestamps;
  const float *segment_durations;
  const char *segment_texts;
  const char *const *segment_texts_arr;
  int32_t segment_count;
} SherpaOnnxOfflineRecognizerResult;
SHERPA_ONNX_API const SherpaOnnxOfflineRecognizerResult *
SherpaOnnxGetOfflineStreamResult(const SherpaOnnxOfflineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineRecognizerResult(
    const SherpaOnnxOfflineRecognizerResult *r);
SHERPA_ONNX_API const char *SherpaOnnxGetOfflineStreamResultAsJson(
    const SherpaOnnxOfflineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineStreamResultJson(const char *s);
typedef struct SherpaOnnxKeywordResult {
  const char *keyword;
  const char *tokens;
  const char *const *tokens_arr;
  int32_t count;
  float *timestamps;
  float start_time;
  const char *json;
} SherpaOnnxKeywordResult;
typedef struct SherpaOnnxKeywordSpotterConfig {
  SherpaOnnxFeatureConfig feat_config;
  SherpaOnnxOnlineModelConfig model_config;
  int32_t max_active_paths;
  int32_t num_trailing_blanks;
  float keywords_score;
  float keywords_threshold;
  const char *keywords_file;
  const char *keywords_buf;
  int32_t keywords_buf_size;
} SherpaOnnxKeywordSpotterConfig;
typedef struct SherpaOnnxKeywordSpotter SherpaOnnxKeywordSpotter;
SHERPA_ONNX_API const SherpaOnnxKeywordSpotter *SherpaOnnxCreateKeywordSpotter(
    const SherpaOnnxKeywordSpotterConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyKeywordSpotter(
    const SherpaOnnxKeywordSpotter *spotter);
SHERPA_ONNX_API const SherpaOnnxOnlineStream *SherpaOnnxCreateKeywordStream(
    const SherpaOnnxKeywordSpotter *spotter);
SHERPA_ONNX_API const SherpaOnnxOnlineStream *
SherpaOnnxCreateKeywordStreamWithKeywords(
    const SherpaOnnxKeywordSpotter *spotter, const char *keywords);
SHERPA_ONNX_API int32_t
SherpaOnnxIsKeywordStreamReady(const SherpaOnnxKeywordSpotter *spotter,
                               const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDecodeKeywordStream(
    const SherpaOnnxKeywordSpotter *spotter,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxResetKeywordStream(
    const SherpaOnnxKeywordSpotter *spotter,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDecodeMultipleKeywordStreams(
    const SherpaOnnxKeywordSpotter *spotter,
    const SherpaOnnxOnlineStream **streams, int32_t n);
SHERPA_ONNX_API const SherpaOnnxKeywordResult *SherpaOnnxGetKeywordResult(
    const SherpaOnnxKeywordSpotter *spotter,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxDestroyKeywordResult(
    const SherpaOnnxKeywordResult *r);
SHERPA_ONNX_API const char *SherpaOnnxGetKeywordResultAsJson(
    const SherpaOnnxKeywordSpotter *spotter,
    const SherpaOnnxOnlineStream *stream);
SHERPA_ONNX_API void SherpaOnnxFreeKeywordResultJson(const char *s);
typedef struct SherpaOnnxSileroVadModelConfig {
  const char *model;
  float threshold;
  float min_silence_duration;
  float min_speech_duration;
  int32_t window_size;
  float max_speech_duration;
} SherpaOnnxSileroVadModelConfig;
typedef struct SherpaOnnxTenVadModelConfig {
  const char *model;
  float threshold;
  float min_silence_duration;
  float min_speech_duration;
  int32_t window_size;
  float max_speech_duration;
} SherpaOnnxTenVadModelConfig;
typedef struct SherpaOnnxVadModelConfig {
  SherpaOnnxSileroVadModelConfig silero_vad;
  int32_t sample_rate;
  int32_t num_threads;
  const char *provider;
  int32_t debug;
  SherpaOnnxTenVadModelConfig ten_vad;
} SherpaOnnxVadModelConfig;
typedef struct SherpaOnnxCircularBuffer SherpaOnnxCircularBuffer;
SHERPA_ONNX_API const SherpaOnnxCircularBuffer *SherpaOnnxCreateCircularBuffer(
    int32_t capacity);
SHERPA_ONNX_API void SherpaOnnxDestroyCircularBuffer(
    const SherpaOnnxCircularBuffer *buffer);
SHERPA_ONNX_API void SherpaOnnxCircularBufferPush(
    const SherpaOnnxCircularBuffer *buffer, const float *p, int32_t n);
SHERPA_ONNX_API const float *SherpaOnnxCircularBufferGet(
    const SherpaOnnxCircularBuffer *buffer, int32_t start_index, int32_t n);
SHERPA_ONNX_API void SherpaOnnxCircularBufferFree(const float *p);
SHERPA_ONNX_API void SherpaOnnxCircularBufferPop(
    const SherpaOnnxCircularBuffer *buffer, int32_t n);
SHERPA_ONNX_API int32_t
SherpaOnnxCircularBufferSize(const SherpaOnnxCircularBuffer *buffer);
SHERPA_ONNX_API int32_t
SherpaOnnxCircularBufferHead(const SherpaOnnxCircularBuffer *buffer);
SHERPA_ONNX_API void SherpaOnnxCircularBufferReset(
    const SherpaOnnxCircularBuffer *buffer);
typedef struct SherpaOnnxSpeechSegment {
  int32_t start;
  float *samples;
  int32_t n;
} SherpaOnnxSpeechSegment;
typedef struct SherpaOnnxVoiceActivityDetector SherpaOnnxVoiceActivityDetector;
SHERPA_ONNX_API const SherpaOnnxVoiceActivityDetector *
SherpaOnnxCreateVoiceActivityDetector(const SherpaOnnxVadModelConfig *config,
                                      float buffer_size_in_seconds);
SHERPA_ONNX_API void SherpaOnnxDestroyVoiceActivityDetector(
    const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API void SherpaOnnxVoiceActivityDetectorAcceptWaveform(
    const SherpaOnnxVoiceActivityDetector *p, const float *samples, int32_t n);
SHERPA_ONNX_API int32_t
SherpaOnnxVoiceActivityDetectorEmpty(const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API int32_t SherpaOnnxVoiceActivityDetectorDetected(
    const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API void SherpaOnnxVoiceActivityDetectorPop(
    const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API void SherpaOnnxVoiceActivityDetectorClear(
    const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API const SherpaOnnxSpeechSegment *
SherpaOnnxVoiceActivityDetectorFront(const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API void SherpaOnnxDestroySpeechSegment(
    const SherpaOnnxSpeechSegment *p);
SHERPA_ONNX_API void SherpaOnnxVoiceActivityDetectorReset(
    const SherpaOnnxVoiceActivityDetector *p);
SHERPA_ONNX_API void SherpaOnnxVoiceActivityDetectorFlush(
    const SherpaOnnxVoiceActivityDetector *p);
typedef struct SherpaOnnxOfflineTtsVitsModelConfig {
  const char *model;
  const char *lexicon;
  const char *tokens;
  const char *data_dir;
  float noise_scale;
  float noise_scale_w;
  float length_scale;
  const char *dict_dir;
} SherpaOnnxOfflineTtsVitsModelConfig;
typedef struct SherpaOnnxOfflineTtsMatchaModelConfig {
  const char *acoustic_model;
  const char *vocoder;
  const char *lexicon;
  const char *tokens;
  const char *data_dir;
  float noise_scale;
  float length_scale;
  const char *dict_dir;
} SherpaOnnxOfflineTtsMatchaModelConfig;
typedef struct SherpaOnnxOfflineTtsKokoroModelConfig {
  const char *model;
  const char *voices;
  const char *tokens;
  const char *data_dir;
  float length_scale;
  const char *dict_dir;
  const char *lexicon;
  const char *lang;
} SherpaOnnxOfflineTtsKokoroModelConfig;
typedef struct SherpaOnnxOfflineTtsKittenModelConfig {
  const char *model;
  const char *voices;
  const char *tokens;
  const char *data_dir;
  float length_scale;
} SherpaOnnxOfflineTtsKittenModelConfig;
typedef struct SherpaOnnxOfflineTtsZipvoiceModelConfig {
  const char *tokens;
  const char *encoder;
  const char *decoder;
  const char *vocoder;
  const char *data_dir;
  const char *lexicon;
  float feat_scale;
  float t_shift;
  float target_rms;
  float guidance_scale;
} SherpaOnnxOfflineTtsZipvoiceModelConfig;
typedef struct SherpaOnnxOfflineTtsPocketModelConfig {
  const char *lm_flow;
  const char *lm_main;
  const char *encoder;
  const char *decoder;
  const char *text_conditioner;
  const char *vocab_json;
  const char *token_scores_json;
  int32_t voice_embedding_cache_capacity;
} SherpaOnnxOfflineTtsPocketModelConfig;
typedef struct SherpaOnnxOfflineTtsSupertonicModelConfig {
  const char *duration_predictor;
  const char *text_encoder;
  const char *vector_estimator;
  const char *vocoder;
  const char *tts_json;
  const char *unicode_indexer;
  const char *voice_style;
} SherpaOnnxOfflineTtsSupertonicModelConfig;
typedef struct SherpaOnnxOfflineTtsModelConfig {
  SherpaOnnxOfflineTtsVitsModelConfig vits;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
  SherpaOnnxOfflineTtsMatchaModelConfig matcha;
  SherpaOnnxOfflineTtsKokoroModelConfig kokoro;
  SherpaOnnxOfflineTtsKittenModelConfig kitten;
  SherpaOnnxOfflineTtsZipvoiceModelConfig zipvoice;
  SherpaOnnxOfflineTtsPocketModelConfig pocket;
  SherpaOnnxOfflineTtsSupertonicModelConfig supertonic;
} SherpaOnnxOfflineTtsModelConfig;
typedef struct SherpaOnnxOfflineTtsConfig {
  SherpaOnnxOfflineTtsModelConfig model;
  const char *rule_fsts;
  int32_t max_num_sentences;
  const char *rule_fars;
  float silence_scale;
} SherpaOnnxOfflineTtsConfig;
typedef struct SherpaOnnxGeneratedAudio {
  const float *samples;
  int32_t n;
  int32_t sample_rate;
} SherpaOnnxGeneratedAudio;
typedef int32_t (*SherpaOnnxGeneratedAudioCallback)(const float *samples,
                                                    int32_t n);
typedef int32_t (*SherpaOnnxGeneratedAudioCallbackWithArg)(const float *samples,
                                                           int32_t n,
                                                           void *arg);
typedef int32_t (*SherpaOnnxGeneratedAudioProgressCallback)(
    const float *samples, int32_t n, float p);
typedef int32_t (*SherpaOnnxGeneratedAudioProgressCallbackWithArg)(
    const float *samples, int32_t n, float p, void *arg);
typedef struct SherpaOnnxOfflineTts SherpaOnnxOfflineTts;
SHERPA_ONNX_API const SherpaOnnxOfflineTts *SherpaOnnxCreateOfflineTts(
    const SherpaOnnxOfflineTtsConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineTts(
    const SherpaOnnxOfflineTts *tts);
SHERPA_ONNX_API int32_t
SherpaOnnxOfflineTtsSampleRate(const SherpaOnnxOfflineTts *tts);
SHERPA_ONNX_API int32_t
SherpaOnnxOfflineTtsNumSpeakers(const SherpaOnnxOfflineTts *tts);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerate(
        const SherpaOnnxOfflineTts *tts, const char *text, int32_t sid,
        float speed);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerateWithCallback(
        const SherpaOnnxOfflineTts *tts, const char *text, int32_t sid,
        float speed, SherpaOnnxGeneratedAudioCallback callback);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerateWithProgressCallback(
        const SherpaOnnxOfflineTts *tts, const char *text, int32_t sid,
        float speed, SherpaOnnxGeneratedAudioProgressCallback callback);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio
        *SherpaOnnxOfflineTtsGenerateWithProgressCallbackWithArg(
            const SherpaOnnxOfflineTts *tts, const char *text, int32_t sid,
            float speed,
            SherpaOnnxGeneratedAudioProgressCallbackWithArg callback,
            void *arg);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerateWithCallbackWithArg(
        const SherpaOnnxOfflineTts *tts, const char *text, int32_t sid,
        float speed, SherpaOnnxGeneratedAudioCallbackWithArg callback,
        void *arg);
SHERPA_ONNX_API SHERPA_ONNX_DEPRECATED(
    "Use SherpaOnnxOfflineTtsGenerateWithConfig() instead") const
    SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerateWithZipvoice(
        const SherpaOnnxOfflineTts *tts, const char *text,
        const char *prompt_text, const float *prompt_samples, int32_t n_prompt,
        int32_t prompt_sr, float speed, int32_t num_steps);
typedef struct SherpaOnnxGenerationConfig {
  float silence_scale;
  float speed;
  int32_t sid;
  const float *reference_audio;
  int32_t reference_audio_len;
  int32_t reference_sample_rate;
  const char *reference_text;
  int32_t num_steps;
  const char *extra;
} SherpaOnnxGenerationConfig;
SHERPA_ONNX_API const SherpaOnnxGeneratedAudio *
SherpaOnnxOfflineTtsGenerateWithConfig(
    const SherpaOnnxOfflineTts *tts, const char *text,
    const SherpaOnnxGenerationConfig *config,
    SherpaOnnxGeneratedAudioProgressCallbackWithArg callback, void *arg);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineTtsGeneratedAudio(
    const SherpaOnnxGeneratedAudio *p);
SHERPA_ONNX_API int32_t SherpaOnnxWriteWave(const float *samples, int32_t n,
                                            int32_t sample_rate,
                                            const char *filename);
SHERPA_ONNX_API int64_t SherpaOnnxWaveFileSize(int32_t n_samples);
SHERPA_ONNX_API void SherpaOnnxWriteWaveToBuffer(const float *samples,
                                                 int32_t n, int32_t sample_rate,
                                                 char *buffer);
SHERPA_ONNX_API int32_t SherpaOnnxWriteWaveMultiChannel(
    const float *const *samples, int32_t n, int32_t sample_rate,
    int32_t num_channels, const char *filename);
typedef struct SherpaOnnxWave {
  const float *samples;
  int32_t sample_rate;
  int32_t num_samples;
} SherpaOnnxWave;
SHERPA_ONNX_API const SherpaOnnxWave *SherpaOnnxReadWave(const char *filename);
SHERPA_ONNX_API const SherpaOnnxWave *SherpaOnnxReadWaveFromBinaryData(
    const char *data, int32_t n);
SHERPA_ONNX_API void SherpaOnnxFreeWave(const SherpaOnnxWave *wave);
typedef struct SherpaOnnxMultiChannelWave {
  const float *const *samples;
  int32_t num_channels;
  int32_t num_samples;
  int32_t sample_rate;
} SherpaOnnxMultiChannelWave;
SHERPA_ONNX_API const SherpaOnnxMultiChannelWave *
SherpaOnnxReadWaveMultiChannel(const char *filename);
SHERPA_ONNX_API void SherpaOnnxFreeMultiChannelWave(
    const SherpaOnnxMultiChannelWave *wave);
typedef struct SherpaOnnxSpokenLanguageIdentificationWhisperConfig {
  const char *encoder;
  const char *decoder;
  int32_t tail_paddings;
} SherpaOnnxSpokenLanguageIdentificationWhisperConfig;
typedef struct SherpaOnnxSpokenLanguageIdentificationConfig {
  SherpaOnnxSpokenLanguageIdentificationWhisperConfig whisper;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxSpokenLanguageIdentificationConfig;
typedef struct SherpaOnnxSpokenLanguageIdentification
    SherpaOnnxSpokenLanguageIdentification;
SHERPA_ONNX_API const SherpaOnnxSpokenLanguageIdentification *
SherpaOnnxCreateSpokenLanguageIdentification(
    const SherpaOnnxSpokenLanguageIdentificationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroySpokenLanguageIdentification(
    const SherpaOnnxSpokenLanguageIdentification *slid);
SHERPA_ONNX_API SherpaOnnxOfflineStream *
SherpaOnnxSpokenLanguageIdentificationCreateOfflineStream(
    const SherpaOnnxSpokenLanguageIdentification *slid);
typedef struct SherpaOnnxSpokenLanguageIdentificationResult {
  const char *lang;
} SherpaOnnxSpokenLanguageIdentificationResult;
SHERPA_ONNX_API const SherpaOnnxSpokenLanguageIdentificationResult *
SherpaOnnxSpokenLanguageIdentificationCompute(
    const SherpaOnnxSpokenLanguageIdentification *slid,
    const SherpaOnnxOfflineStream *s);
SHERPA_ONNX_API void SherpaOnnxDestroySpokenLanguageIdentificationResult(
    const SherpaOnnxSpokenLanguageIdentificationResult *r);
typedef struct SherpaOnnxSpeakerEmbeddingExtractorConfig {
  const char *model;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxSpeakerEmbeddingExtractorConfig;
typedef struct SherpaOnnxSpeakerEmbeddingExtractor
    SherpaOnnxSpeakerEmbeddingExtractor;
SHERPA_ONNX_API const SherpaOnnxSpeakerEmbeddingExtractor *
SherpaOnnxCreateSpeakerEmbeddingExtractor(
    const SherpaOnnxSpeakerEmbeddingExtractorConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroySpeakerEmbeddingExtractor(
    const SherpaOnnxSpeakerEmbeddingExtractor *p);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingExtractorDim(
    const SherpaOnnxSpeakerEmbeddingExtractor *p);
SHERPA_ONNX_API const SherpaOnnxOnlineStream *
SherpaOnnxSpeakerEmbeddingExtractorCreateStream(
    const SherpaOnnxSpeakerEmbeddingExtractor *p);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingExtractorIsReady(
    const SherpaOnnxSpeakerEmbeddingExtractor *p,
    const SherpaOnnxOnlineStream *s);
SHERPA_ONNX_API const float *
SherpaOnnxSpeakerEmbeddingExtractorComputeEmbedding(
    const SherpaOnnxSpeakerEmbeddingExtractor *p,
    const SherpaOnnxOnlineStream *s);
SHERPA_ONNX_API void SherpaOnnxSpeakerEmbeddingExtractorDestroyEmbedding(
    const float *v);
typedef struct SherpaOnnxSpeakerEmbeddingManager
    SherpaOnnxSpeakerEmbeddingManager;
SHERPA_ONNX_API const SherpaOnnxSpeakerEmbeddingManager *
SherpaOnnxCreateSpeakerEmbeddingManager(int32_t dim);
SHERPA_ONNX_API void SherpaOnnxDestroySpeakerEmbeddingManager(
    const SherpaOnnxSpeakerEmbeddingManager *p);
SHERPA_ONNX_API int32_t
SherpaOnnxSpeakerEmbeddingManagerAdd(const SherpaOnnxSpeakerEmbeddingManager *p,
                                     const char *name, const float *v);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerAddList(
    const SherpaOnnxSpeakerEmbeddingManager *p, const char *name,
    const float **v);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerAddListFlattened(
    const SherpaOnnxSpeakerEmbeddingManager *p, const char *name,
    const float *v, int32_t n);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerRemove(
    const SherpaOnnxSpeakerEmbeddingManager *p, const char *name);
SHERPA_ONNX_API const char *SherpaOnnxSpeakerEmbeddingManagerSearch(
    const SherpaOnnxSpeakerEmbeddingManager *p, const float *v,
    float threshold);
SHERPA_ONNX_API void SherpaOnnxSpeakerEmbeddingManagerFreeSearch(
    const char *name);
typedef struct SherpaOnnxSpeakerEmbeddingManagerSpeakerMatch {
  float score;
  const char *name;
} SherpaOnnxSpeakerEmbeddingManagerSpeakerMatch;
typedef struct SherpaOnnxSpeakerEmbeddingManagerBestMatchesResult {
  const SherpaOnnxSpeakerEmbeddingManagerSpeakerMatch *matches;
  int32_t count;
} SherpaOnnxSpeakerEmbeddingManagerBestMatchesResult;
SHERPA_ONNX_API const SherpaOnnxSpeakerEmbeddingManagerBestMatchesResult *
SherpaOnnxSpeakerEmbeddingManagerGetBestMatches(
    const SherpaOnnxSpeakerEmbeddingManager *p, const float *v, float threshold,
    int32_t n);
SHERPA_ONNX_API void SherpaOnnxSpeakerEmbeddingManagerFreeBestMatches(
    const SherpaOnnxSpeakerEmbeddingManagerBestMatchesResult *r);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerVerify(
    const SherpaOnnxSpeakerEmbeddingManager *p, const char *name,
    const float *v, float threshold);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerContains(
    const SherpaOnnxSpeakerEmbeddingManager *p, const char *name);
SHERPA_ONNX_API int32_t SherpaOnnxSpeakerEmbeddingManagerNumSpeakers(
    const SherpaOnnxSpeakerEmbeddingManager *p);
SHERPA_ONNX_API const char *const *
SherpaOnnxSpeakerEmbeddingManagerGetAllSpeakers(
    const SherpaOnnxSpeakerEmbeddingManager *p);
SHERPA_ONNX_API void SherpaOnnxSpeakerEmbeddingManagerFreeAllSpeakers(
    const char *const *names);
typedef struct SherpaOnnxOfflineZipformerAudioTaggingModelConfig {
  const char *model;
} SherpaOnnxOfflineZipformerAudioTaggingModelConfig;
typedef struct SherpaOnnxAudioTaggingModelConfig {
  SherpaOnnxOfflineZipformerAudioTaggingModelConfig zipformer;
  const char *ced;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxAudioTaggingModelConfig;
typedef struct SherpaOnnxAudioTaggingConfig {
  SherpaOnnxAudioTaggingModelConfig model;
  const char *labels;
  int32_t top_k;
} SherpaOnnxAudioTaggingConfig;
typedef struct SherpaOnnxAudioEvent {
  const char *name;
  int32_t index;
  float prob;
} SherpaOnnxAudioEvent;
typedef struct SherpaOnnxAudioTagging SherpaOnnxAudioTagging;
SHERPA_ONNX_API const SherpaOnnxAudioTagging *SherpaOnnxCreateAudioTagging(
    const SherpaOnnxAudioTaggingConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyAudioTagging(
    const SherpaOnnxAudioTagging *tagger);
SHERPA_ONNX_API const SherpaOnnxOfflineStream *
SherpaOnnxAudioTaggingCreateOfflineStream(const SherpaOnnxAudioTagging *tagger);
SHERPA_ONNX_API const SherpaOnnxAudioEvent *const *
SherpaOnnxAudioTaggingCompute(const SherpaOnnxAudioTagging *tagger,
                              const SherpaOnnxOfflineStream *s, int32_t top_k);
SHERPA_ONNX_API void SherpaOnnxAudioTaggingFreeResults(
    const SherpaOnnxAudioEvent *const *p);
typedef struct SherpaOnnxOfflinePunctuationModelConfig {
  const char *ct_transformer;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxOfflinePunctuationModelConfig;
typedef struct SherpaOnnxOfflinePunctuationConfig {
  SherpaOnnxOfflinePunctuationModelConfig model;
} SherpaOnnxOfflinePunctuationConfig;
typedef struct SherpaOnnxOfflinePunctuation SherpaOnnxOfflinePunctuation;
SHERPA_ONNX_API const SherpaOnnxOfflinePunctuation *
SherpaOnnxCreateOfflinePunctuation(
    const SherpaOnnxOfflinePunctuationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflinePunctuation(
    const SherpaOnnxOfflinePunctuation *punct);
SHERPA_ONNX_API const char *SherpaOfflinePunctuationAddPunct(
    const SherpaOnnxOfflinePunctuation *punct, const char *text);
SHERPA_ONNX_API void SherpaOfflinePunctuationFreeText(const char *text);
typedef struct SherpaOnnxOnlinePunctuationModelConfig {
  const char *cnn_bilstm;
  const char *bpe_vocab;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxOnlinePunctuationModelConfig;
typedef struct SherpaOnnxOnlinePunctuationConfig {
  SherpaOnnxOnlinePunctuationModelConfig model;
} SherpaOnnxOnlinePunctuationConfig;
typedef struct SherpaOnnxOnlinePunctuation SherpaOnnxOnlinePunctuation;
SHERPA_ONNX_API const SherpaOnnxOnlinePunctuation *
SherpaOnnxCreateOnlinePunctuation(
    const SherpaOnnxOnlinePunctuationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlinePunctuation(
    const SherpaOnnxOnlinePunctuation *punctuation);
SHERPA_ONNX_API const char *SherpaOnnxOnlinePunctuationAddPunct(
    const SherpaOnnxOnlinePunctuation *punctuation, const char *text);
SHERPA_ONNX_API void SherpaOnnxOnlinePunctuationFreeText(const char *text);
typedef struct SherpaOnnxLinearResampler SherpaOnnxLinearResampler;
SHERPA_ONNX_API const SherpaOnnxLinearResampler *
SherpaOnnxCreateLinearResampler(int32_t samp_rate_in_hz,
                                int32_t samp_rate_out_hz,
                                float filter_cutoff_hz, int32_t num_zeros);
SHERPA_ONNX_API void SherpaOnnxDestroyLinearResampler(
    const SherpaOnnxLinearResampler *p);
SHERPA_ONNX_API void SherpaOnnxLinearResamplerReset(
    const SherpaOnnxLinearResampler *p);
typedef struct SherpaOnnxResampleOut {
  const float *samples;
  int32_t n;
} SherpaOnnxResampleOut;
SHERPA_ONNX_API const SherpaOnnxResampleOut *SherpaOnnxLinearResamplerResample(
    const SherpaOnnxLinearResampler *p, const float *input, int32_t input_dim,
    int32_t flush);
SHERPA_ONNX_API void SherpaOnnxLinearResamplerResampleFree(
    const SherpaOnnxResampleOut *p);
SHERPA_ONNX_API int32_t SherpaOnnxLinearResamplerResampleGetInputSampleRate(
    const SherpaOnnxLinearResampler *p);
SHERPA_ONNX_API int32_t SherpaOnnxLinearResamplerResampleGetOutputSampleRate(
    const SherpaOnnxLinearResampler *p);
typedef struct SherpaOnnxOfflineSpeakerSegmentationPyannoteModelConfig {
  const char *model;
  float window_shift_ratio;
} SherpaOnnxOfflineSpeakerSegmentationPyannoteModelConfig;
typedef struct SherpaOnnxOfflineSpeakerSegmentationModelConfig {
  SherpaOnnxOfflineSpeakerSegmentationPyannoteModelConfig pyannote;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxOfflineSpeakerSegmentationModelConfig;
typedef struct SherpaOnnxFastClusteringConfig {
  int32_t num_clusters;
  float threshold;
  int32_t compute_confidence;
} SherpaOnnxFastClusteringConfig;
typedef struct SherpaOnnxOfflineSpeakerDiarizationConfig {
  SherpaOnnxOfflineSpeakerSegmentationModelConfig segmentation;
  SherpaOnnxSpeakerEmbeddingExtractorConfig embedding;
  SherpaOnnxFastClusteringConfig clustering;
  float min_duration_on;
  float min_duration_off;
} SherpaOnnxOfflineSpeakerDiarizationConfig;
typedef struct SherpaOnnxOfflineSpeakerDiarization
    SherpaOnnxOfflineSpeakerDiarization;
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarization *
SherpaOnnxCreateOfflineSpeakerDiarization(
    const SherpaOnnxOfflineSpeakerDiarizationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineSpeakerDiarization(
    const SherpaOnnxOfflineSpeakerDiarization *sd);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSpeakerDiarizationGetSampleRate(
    const SherpaOnnxOfflineSpeakerDiarization *sd);
SHERPA_ONNX_API void SherpaOnnxOfflineSpeakerDiarizationSetConfig(
    const SherpaOnnxOfflineSpeakerDiarization *sd,
    const SherpaOnnxOfflineSpeakerDiarizationConfig *config);
typedef struct SherpaOnnxOfflineSpeakerDiarizationResult
    SherpaOnnxOfflineSpeakerDiarizationResult;
typedef struct SherpaOnnxOfflineSpeakerDiarizationSegment {
  float start;
  float end;
  int32_t speaker;
  float confidence;
} SherpaOnnxOfflineSpeakerDiarizationSegment;
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSpeakerDiarizationResultGetNumSpeakers(
    const SherpaOnnxOfflineSpeakerDiarizationResult *r);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSpeakerDiarizationResultGetNumSegments(
    const SherpaOnnxOfflineSpeakerDiarizationResult *r);
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarizationSegment *
SherpaOnnxOfflineSpeakerDiarizationResultSortByStartTime(
    const SherpaOnnxOfflineSpeakerDiarizationResult *r);
SHERPA_ONNX_API void SherpaOnnxOfflineSpeakerDiarizationDestroySegment(
    const SherpaOnnxOfflineSpeakerDiarizationSegment *s);
typedef int32_t (*SherpaOnnxOfflineSpeakerDiarizationProgressCallback)(
    int32_t num_processed_chunks, int32_t num_total_chunks, void *arg);
typedef int32_t (*SherpaOnnxOfflineSpeakerDiarizationProgressCallbackNoArg)(
    int32_t num_processed_chunks, int32_t num_total_chunks);
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarizationResult *
SherpaOnnxOfflineSpeakerDiarizationProcess(
    const SherpaOnnxOfflineSpeakerDiarization *sd, const float *samples,
    int32_t n);
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarizationResult *
SherpaOnnxOfflineSpeakerDiarizationProcessWithCallback(
    const SherpaOnnxOfflineSpeakerDiarization *sd, const float *samples,
    int32_t n, SherpaOnnxOfflineSpeakerDiarizationProgressCallback callback,
    void *arg);
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarizationResult *
SherpaOnnxOfflineSpeakerDiarizationProcessWithCallbackNoArg(
    const SherpaOnnxOfflineSpeakerDiarization *sd, const float *samples,
    int32_t n,
    SherpaOnnxOfflineSpeakerDiarizationProgressCallbackNoArg callback);
SHERPA_ONNX_API void SherpaOnnxOfflineSpeakerDiarizationDestroyResult(
    const SherpaOnnxOfflineSpeakerDiarizationResult *r);
typedef struct SherpaOnnxOfflineSpeechDenoiserGtcrnModelConfig {
  const char *model;
} SherpaOnnxOfflineSpeechDenoiserGtcrnModelConfig;
typedef struct SherpaOnnxOfflineSpeechDenoiserDpdfNetModelConfig {
  const char *model;
  float attenuation_limit_db;
} SherpaOnnxOfflineSpeechDenoiserDpdfNetModelConfig;
typedef struct SherpaOnnxOfflineSpeechDenoiserModelConfig {
  SherpaOnnxOfflineSpeechDenoiserGtcrnModelConfig gtcrn;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
  SherpaOnnxOfflineSpeechDenoiserDpdfNetModelConfig dpdfnet;
} SherpaOnnxOfflineSpeechDenoiserModelConfig;
typedef struct SherpaOnnxOfflineSpeechDenoiserConfig {
  SherpaOnnxOfflineSpeechDenoiserModelConfig model;
} SherpaOnnxOfflineSpeechDenoiserConfig;
typedef struct SherpaOnnxOfflineSpeechDenoiser SherpaOnnxOfflineSpeechDenoiser;
SHERPA_ONNX_API const SherpaOnnxOfflineSpeechDenoiser *
SherpaOnnxCreateOfflineSpeechDenoiser(
    const SherpaOnnxOfflineSpeechDenoiserConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineSpeechDenoiser(
    const SherpaOnnxOfflineSpeechDenoiser *sd);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSpeechDenoiserGetSampleRate(
    const SherpaOnnxOfflineSpeechDenoiser *sd);
typedef struct SherpaOnnxDenoisedAudio {
  const float *samples;
  int32_t n;
  int32_t sample_rate;
} SherpaOnnxDenoisedAudio;
SHERPA_ONNX_API const SherpaOnnxDenoisedAudio *
SherpaOnnxOfflineSpeechDenoiserRun(const SherpaOnnxOfflineSpeechDenoiser *sd,
                                   const float *samples, int32_t n,
                                   int32_t sample_rate);
SHERPA_ONNX_API void SherpaOnnxDestroyDenoisedAudio(
    const SherpaOnnxDenoisedAudio *p);
typedef struct SherpaOnnxOnlineSpeechDenoiserConfig {
  SherpaOnnxOfflineSpeechDenoiserModelConfig model;
} SherpaOnnxOnlineSpeechDenoiserConfig;
typedef struct SherpaOnnxOnlineSpeechDenoiser SherpaOnnxOnlineSpeechDenoiser;
SHERPA_ONNX_API const SherpaOnnxOnlineSpeechDenoiser *
SherpaOnnxCreateOnlineSpeechDenoiser(
    const SherpaOnnxOnlineSpeechDenoiserConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOnlineSpeechDenoiser(
    const SherpaOnnxOnlineSpeechDenoiser *sd);
SHERPA_ONNX_API int32_t SherpaOnnxOnlineSpeechDenoiserGetSampleRate(
    const SherpaOnnxOnlineSpeechDenoiser *sd);
SHERPA_ONNX_API int32_t SherpaOnnxOnlineSpeechDenoiserGetFrameShiftInSamples(
    const SherpaOnnxOnlineSpeechDenoiser *sd);
SHERPA_ONNX_API const SherpaOnnxDenoisedAudio *
SherpaOnnxOnlineSpeechDenoiserRun(const SherpaOnnxOnlineSpeechDenoiser *sd,
                                  const float *samples, int32_t n,
                                  int32_t sample_rate);
SHERPA_ONNX_API const SherpaOnnxDenoisedAudio *
SherpaOnnxOnlineSpeechDenoiserFlush(const SherpaOnnxOnlineSpeechDenoiser *sd);
SHERPA_ONNX_API void SherpaOnnxOnlineSpeechDenoiserReset(
    const SherpaOnnxOnlineSpeechDenoiser *sd);
typedef struct SherpaOnnxOfflineSourceSeparationSpleeterModelConfig {
  const char *vocals;
  const char *accompaniment;
} SherpaOnnxOfflineSourceSeparationSpleeterModelConfig;
typedef struct SherpaOnnxOfflineSourceSeparationUvrModelConfig {
  const char *model;
} SherpaOnnxOfflineSourceSeparationUvrModelConfig;
typedef struct SherpaOnnxOfflineSourceSeparationModelConfig {
  SherpaOnnxOfflineSourceSeparationSpleeterModelConfig spleeter;
  SherpaOnnxOfflineSourceSeparationUvrModelConfig uvr;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxOfflineSourceSeparationModelConfig;
typedef struct SherpaOnnxOfflineSourceSeparationConfig {
  SherpaOnnxOfflineSourceSeparationModelConfig model;
} SherpaOnnxOfflineSourceSeparationConfig;
typedef struct SherpaOnnxOfflineSourceSeparation
    SherpaOnnxOfflineSourceSeparation;
SHERPA_ONNX_API const SherpaOnnxOfflineSourceSeparation *
SherpaOnnxCreateOfflineSourceSeparation(
    const SherpaOnnxOfflineSourceSeparationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineSourceSeparation(
    const SherpaOnnxOfflineSourceSeparation *ss);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSourceSeparationGetOutputSampleRate(
    const SherpaOnnxOfflineSourceSeparation *ss);
SHERPA_ONNX_API int32_t SherpaOnnxOfflineSourceSeparationGetNumberOfStems(
    const SherpaOnnxOfflineSourceSeparation *ss);
typedef struct SherpaOnnxSourceSeparationStem {
  float **samples;
  int32_t num_channels;
  int32_t n;
} SherpaOnnxSourceSeparationStem;
typedef struct SherpaOnnxSourceSeparationOutput {
  const SherpaOnnxSourceSeparationStem *stems;
  int32_t num_stems;
  int32_t sample_rate;
} SherpaOnnxSourceSeparationOutput;
SHERPA_ONNX_API const SherpaOnnxSourceSeparationOutput *
SherpaOnnxOfflineSourceSeparationProcess(
    const SherpaOnnxOfflineSourceSeparation *ss, const float *const *samples,
    int32_t num_channels, int32_t num_samples, int32_t sample_rate);
SHERPA_ONNX_API void SherpaOnnxDestroySourceSeparationOutput(
    const SherpaOnnxSourceSeparationOutput *p);
#ifdef __OHOS__
typedef struct NativeResourceManager NativeResourceManager;
SHERPA_ONNX_API const SherpaOnnxOfflineSpeechDenoiser *
SherpaOnnxCreateOfflineSpeechDenoiserOHOS(
    const SherpaOnnxOfflineSpeechDenoiserConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOnlineSpeechDenoiser *
SherpaOnnxCreateOnlineSpeechDenoiserOHOS(
    const SherpaOnnxOnlineSpeechDenoiserConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOnlineRecognizer *
SherpaOnnxCreateOnlineRecognizerOHOS(
    const SherpaOnnxOnlineRecognizerConfig *config, NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOfflineRecognizer *
SherpaOnnxCreateOfflineRecognizerOHOS(
    const SherpaOnnxOfflineRecognizerConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxVoiceActivityDetector *
SherpaOnnxCreateVoiceActivityDetectorOHOS(
    const SherpaOnnxVadModelConfig *config, float buffer_size_in_seconds,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOfflineTts *SherpaOnnxCreateOfflineTtsOHOS(
    const SherpaOnnxOfflineTtsConfig *config, NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOfflinePunctuation *
SherpaOnnxCreateOfflinePunctuationOHOS(
    const SherpaOnnxOfflinePunctuationConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOnlinePunctuation *
SherpaOnnxCreateOnlinePunctuationOHOS(
    const SherpaOnnxOnlinePunctuationConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxSpeakerEmbeddingExtractor *
SherpaOnnxCreateSpeakerEmbeddingExtractorOHOS(
    const SherpaOnnxSpeakerEmbeddingExtractorConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxKeywordSpotter *
SherpaOnnxCreateKeywordSpotterOHOS(const SherpaOnnxKeywordSpotterConfig *config,
                                   NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOfflineSpeakerDiarization *
SherpaOnnxCreateOfflineSpeakerDiarizationOHOS(
    const SherpaOnnxOfflineSpeakerDiarizationConfig *config,
    NativeResourceManager *mgr);
SHERPA_ONNX_API const SherpaOnnxOfflineSourceSeparation *
SherpaOnnxCreateOfflineSourceSeparationOHOS(
    const SherpaOnnxOfflineSourceSeparationConfig *config,
    NativeResourceManager *mgr);
#endif
typedef struct SherpaOnnxOfflineDiacritizationModelConfig {
  const char *catt_encoder;
  const char *catt_decoder;
  int32_t num_threads;
  int32_t debug;
  const char *provider;
} SherpaOnnxOfflineDiacritizationModelConfig;
typedef struct SherpaOnnxOfflineDiacritizationConfig {
  SherpaOnnxOfflineDiacritizationModelConfig model;
} SherpaOnnxOfflineDiacritizationConfig;
typedef struct SherpaOnnxOfflineDiacritization SherpaOnnxOfflineDiacritization;
SHERPA_ONNX_API const SherpaOnnxOfflineDiacritization *
SherpaOnnxCreateOfflineDiacritization(
    const SherpaOnnxOfflineDiacritizationConfig *config);
SHERPA_ONNX_API void SherpaOnnxDestroyOfflineDiacritization(
    const SherpaOnnxOfflineDiacritization *diacrt);
SHERPA_ONNX_API const char *SherpaOfflineDiacritizationAddDiacritics(
    const SherpaOnnxOfflineDiacritization *diacrt, const char *text);
SHERPA_ONNX_API void SherpaOfflineDiacritizationFreeText(const char *text);
#if defined(__GNUC__)
#pragma GCC diagnostic pop
#endif
#ifdef __cplusplus
} 
#endif
#endif  
