#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum { KIND_MEL = 1, KIND_EMB = 2, KIND_WW = 3 };

typedef struct {
  float *data;
  size_t bytes;
  int32_t dims[4];
  int32_t num_dims;
} tensor;

typedef struct {
  int kind;
} model;

typedef struct {
  int kind;
  tensor input;
  tensor output;
} interpreter;

static int deleted_models, deleted_interpreters;

static int ends_with(const char *s, const char *suffix) {
  size_t n = strlen(s), m = strlen(suffix);
  return n >= m && strcmp(s + n - m, suffix) == 0;
}

model *TfLiteModelCreateFromFile(const char *path) {
  if (ends_with(path, "absent.tflite")) return NULL;
  model *m = calloc(1, sizeof *m);
  m->kind = ends_with(path, "melspectrogram.tflite") ? KIND_MEL : ends_with(path, "embedding_model.tflite") ? KIND_EMB : KIND_WW;
  return m;
}

interpreter *TfLiteInterpreterCreate(model *m, void *options) {
  if (m == NULL || options != NULL) return NULL;
  interpreter *it = calloc(1, sizeof *it);
  it->kind = m->kind;
  if (it->kind == KIND_WW) {
    it->input.num_dims = 3;
    it->input.dims[0] = 1, it->input.dims[1] = 16, it->input.dims[2] = 96;
  }
  return it;
}

int TfLiteInterpreterResizeInputTensor(interpreter *it, int32_t index, const int32_t *dims, int32_t num_dims) {
  if (index != 0 || num_dims > 4) return 1;
  it->input.num_dims = num_dims;
  memcpy(it->input.dims, dims, sizeof(int32_t) * num_dims);
  return 0;
}

int TfLiteInterpreterAllocateTensors(interpreter *it) {
  size_t n = 1;
  for (int i = 0; i < it->input.num_dims; i++) n *= it->input.dims[i];
  it->input.bytes = n * sizeof(float);
  it->input.data = calloc(n, sizeof(float));
  size_t out = it->kind == KIND_MEL ? 8 * 32 : it->kind == KIND_EMB ? 96 : 1;
  it->output.bytes = out * sizeof(float);
  it->output.data = calloc(out, sizeof(float));
  return 0;
}

int TfLiteInterpreterInvoke(interpreter *it) {
  const float *x = it->input.data;
  float *y = it->output.data;
  if (it->kind == KIND_MEL) {
    for (int i = 0; i < 8 * 32; i++) {
      float scaled = x[(i * 7) % 1760] * 0.001f;
      y[i] = scaled + (float)i;
    }
  } else if (it->kind == KIND_EMB) {
    for (int j = 0; j < 96; j++) {
      float half = x[(j * 25) % 2432] * 0.5f;
      y[j] = half + x[(j * 25 + 1000) % 2432];
    }
  } else {
    float a = x[0] * 0.25f;
    float b = x[16 * 96 - 1] * 0.125f;
    float sum = a + b;
    y[0] = sum + x[777];
  }
  return 0;
}

tensor *TfLiteInterpreterGetInputTensor(interpreter *it, int32_t index) { return index == 0 ? &it->input : NULL; }
tensor *TfLiteInterpreterGetOutputTensor(interpreter *it, int32_t index) { return index == 0 ? &it->output : NULL; }
size_t TfLiteTensorByteSize(tensor *t) { return t->bytes; }
int32_t TfLiteTensorNumDims(tensor *t) { return t->num_dims; }
int32_t TfLiteTensorDim(tensor *t, int32_t index) { return t->dims[index]; }
int32_t TfLiteTensorType(tensor *t) { return t ? 1 : 0; }

typedef struct {
  float scale;
  int32_t zero_point;
} quantization;

quantization TfLiteTensorQuantizationParams(tensor *t) {
  quantization q = {t ? 0.0f : 1.0f, 0};
  return q;
}

int TfLiteTensorCopyFromBuffer(tensor *t, const void *data, size_t size) {
  if (size != t->bytes) return 1;
  memcpy(t->data, data, size);
  return 0;
}

int TfLiteTensorCopyToBuffer(tensor *t, void *data, size_t size) {
  if (size != t->bytes) return 1;
  memcpy(data, t->data, size);
  return 0;
}

void TfLiteInterpreterDelete(interpreter *it) {
  deleted_interpreters++;
  free(it->input.data);
  free(it->output.data);
  free(it);
}

void TfLiteModelDelete(model *m) {
  deleted_models++;
  free(m);
}

int fake_deleted_models(void) { return deleted_models; }
int fake_deleted_interpreters(void) { return deleted_interpreters; }
