package provider

import "testing"

func TestOpenAIClientOptionsIsTheAuxOptionsType(t *testing.T) {
	var opts auxOpenAIOptions = OpenAIClientOptions{Timeout: 3}
	if opts.Timeout != 3 {
		t.Fatalf("options %+v", opts)
	}
}
