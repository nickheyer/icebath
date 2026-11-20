//go:build amd64 || arm64 || riscv64 || ppc64le || s390x || loong64

package sherpaonnx

import (
	"testing"
	"unsafe"
)

func TestStructLayoutMatchesCAPIHeader(t *testing.T) {
	var c cKeywordSpotterConfig
	var r cKeywordResult
	model := unsafe.Offsetof(c.modelConfig)
	for _, f := range []struct {
		name      string
		got, want uintptr
	}{
		{"feat_config.sample_rate", unsafe.Offsetof(c.featConfig) + unsafe.Offsetof(c.featConfig.sampleRate), 0},
		{"feat_config.feature_dim", unsafe.Offsetof(c.featConfig) + unsafe.Offsetof(c.featConfig.featureDim), 4},
		{"model_config.transducer.encoder", model + unsafe.Offsetof(c.modelConfig.transducerEncoder), 8},
		{"model_config.transducer.decoder", model + unsafe.Offsetof(c.modelConfig.transducerDecoder), 16},
		{"model_config.transducer.joiner", model + unsafe.Offsetof(c.modelConfig.transducerJoiner), 24},
		{"model_config.paraformer.encoder", model + unsafe.Offsetof(c.modelConfig.paraformerEncoder), 32},
		{"model_config.paraformer.decoder", model + unsafe.Offsetof(c.modelConfig.paraformerDecoder), 40},
		{"model_config.zipformer2_ctc.model", model + unsafe.Offsetof(c.modelConfig.zipformer2CtcModel), 48},
		{"model_config.tokens", model + unsafe.Offsetof(c.modelConfig.tokens), 56},
		{"model_config.num_threads", model + unsafe.Offsetof(c.modelConfig.numThreads), 64},
		{"model_config.provider", model + unsafe.Offsetof(c.modelConfig.provider), 72},
		{"model_config.debug", model + unsafe.Offsetof(c.modelConfig.debug), 80},
		{"model_config.model_type", model + unsafe.Offsetof(c.modelConfig.modelType), 88},
		{"model_config.modeling_unit", model + unsafe.Offsetof(c.modelConfig.modelingUnit), 96},
		{"model_config.bpe_vocab", model + unsafe.Offsetof(c.modelConfig.bpeVocab), 104},
		{"model_config.tokens_buf", model + unsafe.Offsetof(c.modelConfig.tokensBuf), 112},
		{"model_config.tokens_buf_size", model + unsafe.Offsetof(c.modelConfig.tokensBufSize), 120},
		{"model_config.nemo_ctc.model", model + unsafe.Offsetof(c.modelConfig.nemoCtcModel), 128},
		{"model_config.t_one_ctc.model", model + unsafe.Offsetof(c.modelConfig.tOneCtcModel), 136},
		{"max_active_paths", unsafe.Offsetof(c.maxActivePaths), 144},
		{"num_trailing_blanks", unsafe.Offsetof(c.numTrailingBlanks), 148},
		{"keywords_score", unsafe.Offsetof(c.keywordsScore), 152},
		{"keywords_threshold", unsafe.Offsetof(c.keywordsThreshold), 156},
		{"keywords_file", unsafe.Offsetof(c.keywordsFile), 160},
		{"keywords_buf", unsafe.Offsetof(c.keywordsBuf), 168},
		{"keywords_buf_size", unsafe.Offsetof(c.keywordsBufSize), 176},
		{"sizeof config", unsafe.Sizeof(c), 184},
		{"result.keyword", unsafe.Offsetof(r.keyword), 0},
		{"result.tokens", unsafe.Offsetof(r.tokens), 8},
		{"result.tokens_arr", unsafe.Offsetof(r.tokensArr), 16},
		{"result.count", unsafe.Offsetof(r.count), 24},
		{"result.timestamps", unsafe.Offsetof(r.timestamps), 32},
		{"result.start_time", unsafe.Offsetof(r.startTime), 40},
		{"result.json", unsafe.Offsetof(r.json), 48},
		{"sizeof result", unsafe.Sizeof(r), 56},
	} {
		if f.got != f.want {
			t.Errorf("%s at %d, c-api.h of v1.13.8 has it at %d", f.name, f.got, f.want)
		}
	}
}
