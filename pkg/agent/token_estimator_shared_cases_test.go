package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// test_token_estimator_shared.py test_byte_and_ratio_consumers_share_one_constant
func TestByteAndRatioConsumersShareOneConstant(t *testing.T) {
	if compressorCharsPerToken != provider.CharsPerToken || tools.ModelCharsPerToken != provider.CharsPerToken {
		t.Fatalf("compressor %d budget %d model %d", compressorCharsPerToken, tools.ModelCharsPerToken, provider.CharsPerToken)
	}
	size := int64(provider.CharsPerToken * 10)
	if got := bytesToTokens(&size); got == nil || *got != 10 {
		t.Fatalf("bytes to tokens %v", got)
	}
	if got := bytesToTokens(nil); got != nil {
		t.Fatalf("bytes to tokens of None %v", *got)
	}
}
