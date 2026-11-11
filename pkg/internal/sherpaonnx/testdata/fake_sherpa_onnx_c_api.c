#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
  const char *encoder, *decoder, *joiner;
} transducer_config;
typedef struct {
  const char *encoder, *decoder;
} paraformer_config;
typedef struct {
  const char *model;
} single_model_config;
typedef struct {
  transducer_config transducer;
  paraformer_config paraformer;
  single_model_config zipformer2_ctc;
  const char *tokens;
  int32_t num_threads;
  const char *provider;
  int32_t debug;
  const char *model_type;
  const char *modeling_unit;
  const char *bpe_vocab;
  const char *tokens_buf;
  int32_t tokens_buf_size;
  single_model_config nemo_ctc;
  single_model_config t_one_ctc;
} online_model_config;
typedef struct {
  int32_t sample_rate, feature_dim;
} feature_config;
typedef struct {
  feature_config feat_config;
  online_model_config model_config;
  int32_t max_active_paths, num_trailing_blanks;
  float keywords_score, keywords_threshold;
  const char *keywords_file;
  const char *keywords_buf;
  int32_t keywords_buf_size;
} spotter_config;
typedef struct {
  const char *keyword;
  const char *tokens;
  const char *const *tokens_arr;
  int32_t count;
  float *timestamps;
  float start_time;
  const char *json;
} keyword_result;
typedef struct {
  char description[1024];
} spotter;
typedef struct {
  int32_t pending, samples, sample_rate, finished, resets;
  float sum;
  char keywords[128];
} stream;

static int live_spotters, live_streams, live_results;

static const char *text(const char *s) { return s ? s : "<null>"; }

const spotter *SherpaOnnxCreateKeywordSpotter(const spotter_config *c) {
  if (c->keywords_file && strstr(c->keywords_file, "refused")) return NULL;
  spotter *s = calloc(1, sizeof *s);
  const online_model_config *m = &c->model_config;
  snprintf(s->description, sizeof s->description, "%d|%d|%s|%s|%s|%s|%s|%s|%s|%d|%s|%d|%s|%s|%s|%s|%d|%s|%s|%d|%d|%.3f|%.3f|%s|%s|%d",
           c->feat_config.sample_rate, c->feat_config.feature_dim, text(m->transducer.encoder), text(m->transducer.decoder),
           text(m->transducer.joiner), text(m->paraformer.encoder), text(m->paraformer.decoder), text(m->zipformer2_ctc.model), text(m->tokens),
           m->num_threads, text(m->provider), m->debug, text(m->model_type), text(m->modeling_unit), text(m->bpe_vocab), text(m->tokens_buf),
           m->tokens_buf_size, text(m->nemo_ctc.model), text(m->t_one_ctc.model), c->max_active_paths, c->num_trailing_blanks,
           c->keywords_score, c->keywords_threshold, text(c->keywords_file), text(c->keywords_buf), c->keywords_buf_size);
  live_spotters++;
  return s;
}

void SherpaOnnxDestroyKeywordSpotter(const spotter *s) {
  live_spotters--;
  free((void *)s);
}

const stream *SherpaOnnxCreateKeywordStream(const spotter *s) {
  (void)s;
  live_streams++;
  return calloc(1, sizeof(stream));
}

const stream *SherpaOnnxCreateKeywordStreamWithKeywords(const spotter *s, const char *keywords) {
  stream *st = (stream *)SherpaOnnxCreateKeywordStream(s);
  snprintf(st->keywords, sizeof st->keywords, "%s", keywords);
  return st;
}

void SherpaOnnxDestroyOnlineStream(const stream *st) {
  live_streams--;
  free((void *)st);
}

void SherpaOnnxOnlineStreamAcceptWaveform(const stream *cst, int32_t sample_rate, const float *samples, int32_t n) {
  stream *st = (stream *)cst;
  st->sample_rate = sample_rate;
  st->samples += n;
  for (int32_t i = 0; i < n; i++) st->sum += samples[i];
  st->pending = st->samples / 640;
}

void SherpaOnnxOnlineStreamInputFinished(const stream *st) { ((stream *)st)->finished = 1; }
int32_t SherpaOnnxIsKeywordStreamReady(const spotter *s, const stream *st) { return s && st->pending > 0; }
void SherpaOnnxDecodeKeywordStream(const spotter *s, const stream *st) { ((stream *)st)->pending -= s != NULL; }

void SherpaOnnxDecodeMultipleKeywordStreams(const spotter *s, const stream **streams, int32_t n) {
  for (int32_t i = 0; i < n; i++) SherpaOnnxDecodeKeywordStream(s, streams[i]);
}

void SherpaOnnxResetKeywordStream(const spotter *s, const stream *cst) {
  stream *st = (stream *)cst;
  st->resets += s != NULL;
  st->samples = 0, st->pending = 0, st->sum = 0;
}

const keyword_result *SherpaOnnxGetKeywordResult(const spotter *s, const stream *st) {
  keyword_result *r = calloc(1, sizeof *r);
  char *keyword = calloc(1, 2048);
  snprintf(keyword, 2048, " %s#%d#%d#%.2f#%d#%d#%d#%s ", s->description, st->sample_rate, st->samples, st->sum, st->pending, st->finished,
           st->resets, st->keywords);
  r->keyword = keyword;
  if (st->samples > 0) {
    static const char *tokens[2] = {"\xe2\x96\x81HE", "Y"};
    r->tokens_arr = tokens;
    r->count = 2;
    r->timestamps = calloc(2, sizeof(float));
    r->timestamps[0] = 0.04f, r->timestamps[1] = 0.32f;
  }
  live_results++;
  return r;
}

void SherpaOnnxDestroyKeywordResult(const keyword_result *r) {
  live_results--;
  free((void *)r->keyword);
  free(r->timestamps);
  free((void *)r);
}

int32_t fake_live_objects(int32_t kind) { return kind == 0 ? live_spotters : kind == 1 ? live_streams : live_results; }
