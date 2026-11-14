#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sherpa_onnx_c_api.h"

static char last_config[4096];

static const char *text_or_null(const char *s) { return s ? s : "<null>"; }

const char *FakeLastConfig(void) { return last_config; }

struct SherpaOnnxOfflineRecognizer {
  int unused;
};
struct SherpaOnnxOfflineStream {
  char text[512];
  SherpaOnnxOfflineRecognizerResult result;
};

const SherpaOnnxOfflineRecognizer *SherpaOnnxCreateOfflineRecognizer(const SherpaOnnxOfflineRecognizerConfig *c) {
  const SherpaOnnxOfflineModelConfig *m = &c->model_config;
  snprintf(last_config, sizeof last_config, "%d|%d|%s|%s|%s|%s|%d|%s|%d|%s|%s|%s|%s|%s|%s", c->feat_config.sample_rate, c->feat_config.feature_dim,
           text_or_null(m->whisper.encoder), text_or_null(m->whisper.decoder), text_or_null(m->whisper.language), text_or_null(m->whisper.task),
           m->whisper.tail_paddings, text_or_null(m->tokens), m->num_threads, text_or_null(m->provider), text_or_null(c->decoding_method),
           text_or_null(m->transducer.encoder), text_or_null(m->cohere_transcribe.encoder), text_or_null(c->hr.lexicon), text_or_null(c->lm_config.model));
  if (!m->whisper.encoder || strstr(m->whisper.encoder, "rejected")) return NULL;
  return calloc(1, sizeof(SherpaOnnxOfflineRecognizer));
}
void SherpaOnnxDestroyOfflineRecognizer(const SherpaOnnxOfflineRecognizer *r) { free((void *)r); }
const SherpaOnnxOfflineStream *SherpaOnnxCreateOfflineStream(const SherpaOnnxOfflineRecognizer *r) { return calloc(1, sizeof(SherpaOnnxOfflineStream)); }
void SherpaOnnxDestroyOfflineStream(const SherpaOnnxOfflineStream *s) { free((void *)s); }
void SherpaOnnxAcceptWaveformOffline(const SherpaOnnxOfflineStream *s, int32_t sample_rate, const float *samples, int32_t n) {
  snprintf(((SherpaOnnxOfflineStream *)s)->text, 512, "rate=%d n=%d edge=%.2f", sample_rate, n, samples[0] + samples[n - 1]);
}
void SherpaOnnxDecodeOfflineStream(const SherpaOnnxOfflineRecognizer *r, const SherpaOnnxOfflineStream *s) {
  strcat(((SherpaOnnxOfflineStream *)s)->text, " decoded");
}
const SherpaOnnxOfflineRecognizerResult *SherpaOnnxGetOfflineStreamResult(const SherpaOnnxOfflineStream *s) {
  SherpaOnnxOfflineRecognizerResult *r = calloc(1, sizeof *r);
  r->text = strdup(s->text);
  r->lang = "en";
  return r;
}
void SherpaOnnxDestroyOfflineRecognizerResult(const SherpaOnnxOfflineRecognizerResult *r) {
  free((void *)r->text);
  free((void *)r);
}

struct SherpaOnnxOfflineTts {
  int unused;
};

const SherpaOnnxOfflineTts *SherpaOnnxCreateOfflineTts(const SherpaOnnxOfflineTtsConfig *c) {
  const SherpaOnnxOfflineTtsModelConfig *m = &c->model;
  snprintf(last_config, sizeof last_config, "%s|%s|%s|%.3f|%.3f|%.3f|%d|%s|%s|%s|%s|%s|%.3f|%d|%.2f|%s|%s", text_or_null(m->vits.model), text_or_null(m->vits.tokens),
           text_or_null(m->vits.data_dir), m->vits.noise_scale, m->vits.noise_scale_w, m->vits.length_scale, m->num_threads, text_or_null(m->provider),
           text_or_null(m->kitten.model), text_or_null(m->kitten.voices), text_or_null(m->kitten.tokens), text_or_null(m->kitten.data_dir),
           m->kitten.length_scale, c->max_num_sentences, c->silence_scale, text_or_null(m->supertonic.voice_style), text_or_null(c->rule_fars));
  if (!m->vits.model && !m->kitten.model) return NULL;
  return calloc(1, sizeof(SherpaOnnxOfflineTts));
}
void SherpaOnnxDestroyOfflineTts(const SherpaOnnxOfflineTts *t) { free((void *)t); }
int32_t SherpaOnnxOfflineTtsSampleRate(const SherpaOnnxOfflineTts *t) { return 22050; }
int32_t SherpaOnnxOfflineTtsNumSpeakers(const SherpaOnnxOfflineTts *t) { return 3; }
const SherpaOnnxGeneratedAudio *SherpaOnnxOfflineTtsGenerateWithConfig(const SherpaOnnxOfflineTts *t, const char *text, const SherpaOnnxGenerationConfig *c,
                                                                       SherpaOnnxGeneratedAudioProgressCallbackWithArg callback, void *arg) {
  SherpaOnnxGeneratedAudio *a = calloc(1, sizeof *a);
  int32_t n = (int32_t)strlen(text);
  float *samples = calloc(n ? n : 1, sizeof(float));
  for (int32_t i = 0; i < n; i++) samples[i] = c->speed + c->sid + c->silence_scale + (float)i;
  a->samples = samples;
  a->n = n;
  a->sample_rate = 22050;
  return a;
}
void SherpaOnnxDestroyOfflineTtsGeneratedAudio(const SherpaOnnxGeneratedAudio *a) {
  free((void *)a->samples);
  free((void *)a);
}

struct SherpaOnnxVoiceActivityDetector {
  int32_t windows, pending, emitted;
  float first[64];
};

const SherpaOnnxVoiceActivityDetector *SherpaOnnxCreateVoiceActivityDetector(const SherpaOnnxVadModelConfig *c, float buffer_size_in_seconds) {
  snprintf(last_config, sizeof last_config, "%s|%.2f|%.2f|%.2f|%d|%.1f|%d|%d|%s|%s|%.1f", text_or_null(c->silero_vad.model), c->silero_vad.threshold,
           c->silero_vad.min_silence_duration, c->silero_vad.min_speech_duration, c->silero_vad.window_size, c->silero_vad.max_speech_duration,
           c->sample_rate, c->num_threads, text_or_null(c->provider), text_or_null(c->ten_vad.model), buffer_size_in_seconds);
  return calloc(1, sizeof(SherpaOnnxVoiceActivityDetector));
}
void SherpaOnnxDestroyVoiceActivityDetector(const SherpaOnnxVoiceActivityDetector *p) { free((void *)p); }
void SherpaOnnxVoiceActivityDetectorAcceptWaveform(const SherpaOnnxVoiceActivityDetector *p, const float *samples, int32_t n) {
  SherpaOnnxVoiceActivityDetector *v = (SherpaOnnxVoiceActivityDetector *)p;
  if (v->windows % 2 == 0 && v->windows / 2 < 64) v->first[v->windows / 2] = samples[0];
  v->windows++;
  if (v->windows % 2 == 0) v->pending++;
}
int32_t SherpaOnnxVoiceActivityDetectorEmpty(const SherpaOnnxVoiceActivityDetector *p) { return p->pending == 0; }
const SherpaOnnxSpeechSegment *SherpaOnnxVoiceActivityDetectorFront(const SherpaOnnxVoiceActivityDetector *p) {
  SherpaOnnxSpeechSegment *s = calloc(1, sizeof *s);
  s->start = p->emitted * 1024;
  s->n = 2;
  s->samples = calloc(2, sizeof(float));
  s->samples[0] = p->first[p->emitted];
  s->samples[1] = (float)p->emitted;
  return s;
}
void SherpaOnnxVoiceActivityDetectorPop(const SherpaOnnxVoiceActivityDetector *p) {
  SherpaOnnxVoiceActivityDetector *v = (SherpaOnnxVoiceActivityDetector *)p;
  v->pending--;
  v->emitted++;
}
void SherpaOnnxVoiceActivityDetectorFlush(const SherpaOnnxVoiceActivityDetector *p) {
  SherpaOnnxVoiceActivityDetector *v = (SherpaOnnxVoiceActivityDetector *)p;
  if (v->windows % 2 == 1) {
    v->windows++;
    v->pending++;
  }
}
void SherpaOnnxDestroySpeechSegment(const SherpaOnnxSpeechSegment *s) {
  free(s->samples);
  free((void *)s);
}
