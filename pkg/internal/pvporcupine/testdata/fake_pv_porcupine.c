#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
  int32_t num_keywords;
} pv_porcupine_t;

static char sdk[32];
static char recorded[1024];
static int freed_stacks;
static const char *stack_messages[2] = {"first frame", "second frame"};
static const char *devices[2] = {"best", "cpu:2"};

void pv_set_sdk(const char *name) { snprintf(sdk, sizeof sdk, "%s", name); }

int pv_get_error_stack(char ***stack, int *depth) {
  *stack = (char **)stack_messages;
  *depth = 2;
  return 0;
}

void pv_free_error_stack(char **stack) { freed_stacks += stack == (char **)stack_messages; }

int pv_porcupine_init(const char *access_key, const char *model_path, const char *device, int32_t num_keywords,
                      const char *const *keyword_paths, const float *sensitivities, pv_porcupine_t **object) {
  if (strcmp(access_key, "refused") == 0) return 11;
  if (strcmp(sdk, "python") != 0) return 7;
  int used = snprintf(recorded, sizeof recorded, "%s|%s|%s|%d", access_key, model_path, device, num_keywords);
  for (int32_t i = 0; i < num_keywords; i++) {
    used += snprintf(recorded + used, sizeof recorded - used, "|%s|%.2f", keyword_paths[i], sensitivities[i]);
  }
  pv_porcupine_t *handle = calloc(1, sizeof *handle);
  handle->num_keywords = num_keywords;
  *object = handle;
  return 0;
}

void pv_porcupine_delete(pv_porcupine_t *object) {
  snprintf(recorded, sizeof recorded, "deleted %d freed %d", object->num_keywords, freed_stacks);
  free(object);
}

int pv_porcupine_process(pv_porcupine_t *object, const int16_t *pcm, int32_t *result) {
  if (pcm[0] == -1) return 6;
  long sum = 0;
  for (int i = 0; i < 512; i++) sum += pcm[i];
  *result = (int32_t)(sum % (object->num_keywords + 1)) - 1;
  return 0;
}

const char *pv_porcupine_version(void) { return recorded; }
int pv_porcupine_frame_length(void) { return 512; }
int pv_sample_rate(void) { return 16000; }

int pv_porcupine_list_hardware_devices(char ***hardware_devices, int32_t *num_hardware_devices) {
  *hardware_devices = (char **)devices;
  *num_hardware_devices = 2;
  return 0;
}

void pv_porcupine_free_hardware_devices(char **hardware_devices, int32_t num_hardware_devices) {
  (void)hardware_devices;
  (void)num_hardware_devices;
}
