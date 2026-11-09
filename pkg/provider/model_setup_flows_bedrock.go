package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// BedrockGeoPrefixes are the cross region inference profile prefixes that route only from their own geography
var BedrockGeoPrefixes = []string{"us.", "eu.", "ap.", "apac.", "jp.", "ca.", "sa.", "me.", "af."}

// Region name prefixes to the inference profile geo prefix
var bedrockRegionGeo = []struct {
	geo      string
	prefixes []string
}{
	{"us.", []string{"us-", "us_gov"}}, {"eu.", []string{"eu-"}}, {"ap.", []string{"ap-"}}, {"ca.", []string{"ca-"}},
	{"sa.", []string{"sa-"}}, {"me.", []string{"me-"}}, {"af.", []string{"af-"}},
}

var bedrockExcludePrefixes = []string{"stability.", "cohere.embed", "twelvelabs.", "us.stability.", "us.cohere.embed",
	"us.twelvelabs.", "global.cohere.embed", "global.twelvelabs."}

var bedrockExcludeSubstrings = []string{"safeguard", "voxtral", "palmyra-vision"}

var bedrockPickerProfilePrefixes = append(append([]string{}, BedrockGeoPrefixes...), "global.")

// Recommended models matched without their geo prefix so each region pins its own profile
var bedrockRecommendedBases = []string{
	"anthropic.claude-sonnet-4-6", "anthropic.claude-opus-4-6", "anthropic.claude-haiku-4-5", "amazon.nova-pro",
	"amazon.nova-lite", "amazon.nova-micro", "deepseek.v3", "meta.llama4-maverick", "meta.llama4-scout"}

// Bedrock flow seams, swappable so tests script the discovery and the credential chain probe
var (
	bedrockDiscoverModelsFn = DiscoverBedrockModels
	bedrockHasCredentialsFn = func() bool { return hasAWSCredentials(true) }
)

// BedrockRegionGeoPrefix maps an AWS region name to its inference profile geo prefix, empty when unknown
func BedrockRegionGeoPrefix(regionName string) string {
	r := strings.ToLower(regionName)
	for _, row := range bedrockRegionGeo {
		for _, p := range row.prefixes {
			if strings.HasPrefix(r, p) {
				return row.geo
			}
		}
	}
	return ""
}

func matchedPrefix(s string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return p, true
		}
	}
	return "", false
}

// BedrockModelRoutableFromRegion is true when the model id can be invoked from the region's endpoint
func BedrockModelRoutableFromRegion(modelID, regionName string) bool {
	mid := strings.ToLower(modelID)
	matchedGeo, ok := matchedPrefix(mid, BedrockGeoPrefixes)
	if !ok || strings.HasPrefix(mid, "global.") {
		return true
	}
	geo := BedrockRegionGeoPrefix(regionName)
	if geo == "" {
		return true
	}
	if geo == "ap." {
		return matchedGeo == "ap." || matchedGeo == "apac." || matchedGeo == "jp."
	}
	return matchedGeo == geo
}

// Bedrock API Key mode on the OpenAI compatible bedrock-mantle endpoint saved as a named custom provider
func modelFlowBedrockAPIKey(r *ModelFlowRun, region string) error {
	mantleBaseURL := fmt.Sprintf("https://bedrock-mantle.%s.api.aws/v1", region)
	bedrockPconfig := Profile{Name: "bedrock", DisplayName: "Bedrock", AuthType: AuthTypeAPIKey, KeyEnv: []string{"AWS_BEARER_TOKEN_BEDROCK"}}
	existingKey, existingSource := existingAPIKeyForModelFlow(r.Env, "bedrock", bedrockPconfig)
	if existingKey != "" {
		sourceVar := existingSource
		if sourceVar == "" {
			sourceVar = "AWS_BEARER_TOKEN_BEDROCK"
		}
		fmt.Fprintf(r.Out, "  Bedrock API Key: %s... ✓%s\n", prefixRunes(existingKey, 12), FormatSecretSourceSuffix(r.home(), sourceVar))
	} else {
		flowSay(r.Out, fmt.Sprintf("  Endpoint: %s", mantleBaseURL), "")
		apiKey, ok := flowAskFn(r.Out, "  Bedrock API Key: ", flowAskOptions{Secret: true, CancelMsg: optStr("")})
		if !ok {
			return nil
		}
		if apiKey == "" {
			fmt.Fprintln(r.Out, "  Cancelled.")
			return nil
		}
		if err := r.saveEnv("AWS_BEARER_TOKEN_BEDROCK", apiKey); err != nil {
			return err
		}
		existingKey = apiKey
		fmt.Fprintln(r.Out, "  ✓ API key saved.")
	}
	fmt.Fprintln(r.Out)
	modelList := providerModels["bedrock"]
	fmt.Fprintf(r.Out, "  Showing %d curated models\n", len(modelList))
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, modelList, "  Model ID: ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: "custom", ConfirmBaseURL: mantleBaseURL, ConfirmAPIKey: existingKey})
	finish := func(cfg config.Tree, _ map[string]any) {
		providers := ensureDictSection(cfg, "providers")
		mantleEntry, ok := providers["bedrock-mantle"].(map[string]any)
		if !ok {
			mantleEntry = map[string]any{}
		}
		mantleEntry["base_url"] = mantleBaseURL
		mantleEntry["key_env"] = "AWS_BEARER_TOKEN_BEDROCK"
		providers["bedrock-mantle"] = mantleEntry
		ensureDictSection(cfg, "bedrock")["region"] = region
	}
	model, err := finishModelFn(r.Env, r.Out, selected, "custom:bedrock-mantle", fmt.Sprintf("  Default model set to: %s (via Bedrock API Key, %s)", selected, region),
		"  No change.", persistModelOptions{DropBaseURL: true, DropAPIMode: true, Finish: finish})
	if err != nil {
		return err
	}
	if model != nil {
		fmt.Fprintf(r.Out, "  Endpoint: %s\n", mantleBaseURL)
	}
	return nil
}

// Bare model id of an inference profile id
func bedrockBaseID(mid string) string {
	if p, ok := matchedPrefix(mid, bedrockPickerProfilePrefixes); ok {
		return mid[len(p):]
	}
	return mid
}

// Routable text model ids deduped against their profiles, recommended first, then global profiles, then the rest
func bedrockTextModelIDs(liveModels []BedrockModel, region string) []string {
	filtered := []BedrockModel{}
	for _, m := range liveModels {
		if _, excluded := matchedPrefix(m.ID, bedrockExcludePrefixes); excluded {
			continue
		}
		lower := strings.ToLower(m.ID)
		skip := false
		for _, s := range bedrockExcludeSubstrings {
			if strings.Contains(lower, s) {
				skip = true
				break
			}
		}
		if skip || !BedrockModelRoutableFromRegion(m.ID, region) {
			continue
		}
		filtered = append(filtered, m)
	}
	profileBaseIDs := map[string]bool{}
	for _, m := range filtered {
		if _, isProfile := matchedPrefix(m.ID, bedrockPickerProfilePrefixes); isProfile {
			profileBaseIDs[bedrockBaseID(m.ID)] = true
		}
	}
	deduped := []string{}
	for _, m := range filtered {
		if _, isProfile := matchedPrefix(m.ID, bedrockPickerProfilePrefixes); isProfile || !profileBaseIDs[m.ID] {
			deduped = append(deduped, m.ID)
		}
	}
	type sortKey struct {
		tier, rec, global int
		mid               string
	}
	keyOf := func(mid string) sortKey {
		base := bedrockBaseID(mid)
		for i, rec := range bedrockRecommendedBases {
			if strings.HasPrefix(base, rec) {
				global := 0
				if strings.HasPrefix(mid, "global.") {
					global = 1
				}
				return sortKey{0, i, global, mid}
			}
		}
		if strings.HasPrefix(mid, "global.") {
			return sortKey{1, 0, 0, mid}
		}
		return sortKey{2, 0, 0, mid}
	}
	sort.SliceStable(deduped, func(i, j int) bool {
		a, b := keyOf(deduped[i]), keyOf(deduped[j])
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.rec != b.rec {
			return a.rec < b.rec
		}
		if a.global != b.global {
			return a.global < b.global
		}
		return a.mid < b.mid
	})
	return deduped
}

// AWS Bedrock wizard over the IAM credential chain, region pick, live model discovery and persist
func modelFlowBedrock(r *ModelFlowRun) error {
	if !bedrockHasCredentialsFn() {
		flowSay(r.Out, "  ⚠ No AWS credentials detected via environment variables.",
			"  Bedrock will use boto3's default credential chain (IMDS, SSO, etc.)", "")
	}
	authVar := resolveAWSAuthEnvVar()
	credLine := "  AWS credentials: boto3 default chain (instance role / SSO)"
	if authVar != "" {
		credLine = fmt.Sprintf("  AWS credentials: %s ✓", authVar)
	}
	flowSay(r.Out, credLine, "")
	currentRegion := ResolveBedrockRegion()
	regionInput, ok := flowAskFn(r.Out, fmt.Sprintf("  AWS Region [%s]: ", currentRegion), flowAskOptions{CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	region := regionInput
	if region == "" {
		region = currentRegion
	}
	flowSay(r.Out, "  Choose authentication method:", "", "    1. IAM credential chain (recommended)",
		"       Works with EC2 instance roles, SSO, env vars, aws configure", "    2. Bedrock API Key",
		"       Enter your Bedrock API Key directly — also supports",
		"       team scenarios where an admin distributes keys", "")
	authChoice, ok := flowAskFn(r.Out, "  Choice [1]: ", flowAskOptions{Raw: true, CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	if authChoice == "2" {
		return modelFlowBedrockAPIKey(r, region)
	}
	fmt.Fprintf(r.Out, "  Discovering models in %s...\n", region)
	liveModels := bedrockDiscoverModelsFn(r.ctx(), region, nil)
	var modelList []string
	if len(liveModels) > 0 {
		modelList = bedrockTextModelIDs(liveModels, region)
		fmt.Fprintf(r.Out, "  Found %d text model(s) (filtered from %d total)\n", len(modelList), len(liveModels))
	} else {
		modelList = providerModels["bedrock"]
		if len(modelList) == 0 {
			fmt.Fprintln(r.Out, "  No models found. Check IAM permissions for bedrock:ListFoundationModels.")
			noteSetupFailure("no_models")
			return nil
		}
		fmt.Fprintf(r.Out, "  Using %d curated models (live discovery unavailable)\n", len(modelList))
	}
	runtimeURL := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	selected, _ := pickModelOrPromptFn(r.Env, r.Out, modelList, "  Model ID: ",
		ModelSelectionPrompt{CurrentModel: r.CurrentModel, ConfirmProvider: "bedrock", ConfirmBaseURL: runtimeURL})
	finish := func(cfg config.Tree, _ map[string]any) { ensureDictSection(cfg, "bedrock")["region"] = region }
	_, err := finishModelFn(r.Env, r.Out, selected, "bedrock", fmt.Sprintf("  Default model set to: %s (via AWS Bedrock, %s)", selected, region),
		"  No change.", persistModelOptions{BaseURL: optStr(runtimeURL), DropAPIMode: true, Finish: finish})
	return err
}
