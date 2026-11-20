package tokenizers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/pkg/tokenizers/sentencepiece"
	"github.com/nickheyer/icebath/pkg/tokenizers/tiktoken"
)

// Encoder gives the token ids of a text
type Encoder interface {
	Encode(text string) ([]int, error)
}

// Source reads the files of one tokenizer snapshot
type Source interface {
	Has(name string) bool
	Read(name string) ([]byte, error)
}

// Dir is a snapshot folder on disk
type Dir string

func (d Dir) Has(name string) bool {
	st, err := os.Stat(filepath.Join(string(d), name))
	return err == nil && !st.IsDir()
}

func (d Dir) Read(name string) ([]byte, error) { return os.ReadFile(filepath.Join(string(d), name)) }

// Formats names the tokenizer files Load reads, in the order it looks for them
const Formats = "tokenizer.json, tiktoken.model, a SentencePiece model (tokenizer.model, spiece.model, sentencepiece.bpe.model, spm.model or sentencepiece.model), vocab.json with merges.txt, or vocab.txt"

var sentencePieceFiles = []string{"tokenizer.model", "spiece.model", "sentencepiece.bpe.model", "spm.model", "sentencepiece.model"}

var roles = []string{"bos", "eos", "unk", "sep", "pad", "cls", "mask"}

// settings are the tokenizer_config.json and special_tokens_map.json facts the loaders read
type settings struct {
	class        string
	role         map[string]string
	unset        map[string]bool
	extra        []string
	added        map[int]AddedToken
	addBOS       *bool
	addEOS       *bool
	prefixSpace  bool
	extraIDs     int
	offset       int
	lowerCase    *bool
	keepAccents  bool
	removeSpace  bool
	chineseChars bool
	stripAccents *bool
}

// The text of a special token entry, a plain string or an object with its content
func tokenContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var token AddedToken
	if json.Unmarshal(raw, &token) == nil {
		return token.Content
	}
	return ""
}

func readSettings(src Source) (*settings, error) {
	cfg := &settings{role: map[string]string{}, unset: map[string]bool{}, added: map[int]AddedToken{}, chineseChars: true, removeSpace: true}
	seen := map[string]bool{}
	offsets := map[string]json.RawMessage{}
	cfg.offset = 103
	extra := func(raw json.RawMessage) {
		var list []json.RawMessage
		var named map[string]json.RawMessage
		if json.Unmarshal(raw, &list) != nil && json.Unmarshal(raw, &named) == nil {
			keys := make([]string, 0, len(named))
			for key := range named {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				list = append(list, named[key])
			}
		}
		for _, item := range list {
			if content := tokenContent(item); content != "" && !seen[content] {
				seen[content] = true
				cfg.extra = append(cfg.extra, content)
			}
		}
	}
	for _, name := range []string{"special_tokens_map.json", "tokenizer_config.json"} {
		if !src.Has(name) {
			continue
		}
		data, err := src.Read(name)
		if err != nil {
			return nil, err
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("tokenizers: %s: %w", name, err)
		}
		for _, role := range roles {
			raw, stated := f[role+"_token"]
			if content := tokenContent(raw); content != "" {
				cfg.role[role] = content
			} else if stated {
				delete(cfg.role, role)
				cfg.unset[role] = true
			}
		}
		extra(f["additional_special_tokens"])
		extra(f["extra_special_tokens"])
		if !isNull(f["offset"]) {
			offsets["offset"] = f["offset"]
		}
		var added map[string]AddedToken
		if err := firstErr(field(f, "tokenizer_class", &cfg.class), field(f, "added_tokens_decoder", &added), field(f, "add_bos_token", &cfg.addBOS),
			field(f, "add_eos_token", &cfg.addEOS), field(f, "add_prefix_space", &cfg.prefixSpace), field(f, "do_lower_case", &cfg.lowerCase), field(f, "extra_ids", &cfg.extraIDs),
			field(f, "tokenize_chinese_chars", &cfg.chineseChars), field(f, "strip_accents", &cfg.stripAccents),
			field(f, "keep_accents", &cfg.keepAccents), field(f, "remove_space", &cfg.removeSpace)); err != nil {
			return nil, fmt.Errorf("%w in %s", err, name)
		}
		for key, token := range added {
			id, err := strconv.Atoi(key)
			if err != nil {
				return nil, fmt.Errorf("tokenizers: %s names the added token id %q", name, key)
			}
			token.ID = id
			cfg.added[id] = token
		}
	}
	if cfg.class == "" && src.Has("config.json") {
		data, err := src.Read("config.json")
		if err != nil {
			return nil, err
		}
		var model struct {
			Class string `json:"tokenizer_class"`
			Type  string `json:"model_type"`
		}
		if err := json.Unmarshal(data, &model); err != nil {
			return nil, fmt.Errorf("tokenizers: config.json: %w", err)
		}
		if cfg.class = model.Class; cfg.class == "" {
			cfg.class = modelClasses[model.Type]
		}
	}
	cfg.class = strings.TrimSuffix(cfg.class, "Fast")
	if family, ok := families[cfg.class]; ok {
		cfg.class = family
	}
	if err := field(offsets, "offset", &cfg.offset); err != nil {
		return nil, err
	}
	if src.Has("added_tokens.json") {
		data, err := src.Read("added_tokens.json")
		if err != nil {
			return nil, err
		}
		var ids map[string]int
		if err := json.Unmarshal(data, &ids); err != nil {
			return nil, fmt.Errorf("tokenizers: added_tokens.json: %w", err)
		}
		for content, id := range ids {
			if _, ok := cfg.added[id]; !ok {
				cfg.added[id] = AddedToken{ID: id, Content: content}
			}
		}
	}
	return cfg, nil
}

// Names the special tokens of the tokenizer class for every role the settings neither name nor clear
func (cfg *settings) classDefaults() {
	for _, pair := range strings.Fields(classTokens[cfg.class]) {
		role, content, _ := strings.Cut(pair, "=")
		if cfg.role[role] == "" && !cfg.unset[role] {
			cfg.role[role] = content
		}
	}
}

// Whether the settings ask for lowercase text, the class default when they do not say
func (cfg *settings) lower(byDefault bool) bool {
	if cfg.lowerCase != nil {
		return *cfg.lowerCase
	}
	return byDefault
}

// The added tokens the settings declare in id order, then the role and extra special tokens
func (cfg *settings) declared() []AddedToken {
	ids := make([]int, 0, len(cfg.added))
	for id := range cfg.added {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]AddedToken, 0, len(ids)+len(roles)+len(cfg.extra))
	for _, id := range ids {
		out = append(out, cfg.added[id])
	}
	for _, role := range roles {
		if content := cfg.role[role]; content != "" {
			out = append(out, AddedToken{ID: -1, Content: content, Special: true})
		}
	}
	for _, content := range cfg.extra {
		out = append(out, AddedToken{ID: -1, Content: content, Special: true})
	}
	return out
}

// Adds every declared token the tokenizer lacks, under its vocabulary id or the next free one
func (t *Tokenizer) declare(tokens []AddedToken) error {
	var fresh []AddedToken
	known := map[string]bool{}
	next := -1
	for _, token := range tokens {
		if _, ok := t.byContent[token.Content]; ok || known[token.Content] || token.Content == "" {
			continue
		}
		known[token.Content] = true
		if id, ok := t.model.id(token.Content); ok {
			token.ID = id
		} else {
			if next < 0 {
				next = t.nextID(token.Content)
			}
			token.ID = next
			next++
		}
		fresh = append(fresh, token)
	}
	return t.addTokens(fresh)
}

// The tokenizer class of each model type whose snapshot names none
var modelClasses = map[string]string{
	"bert": "BertTokenizer", "distilbert": "BertTokenizer", "electra": "BertTokenizer", "mobilebert": "BertTokenizer", "roberta": "RobertaTokenizer",
	"bart": "RobertaTokenizer", "longformer": "RobertaTokenizer", "led": "RobertaTokenizer", "mvp": "RobertaTokenizer", "xlm-roberta": "XLMRobertaTokenizer",
	"camembert": "CamembertTokenizer", "albert": "AlbertTokenizer", "deberta": "DebertaTokenizer", "deberta-v2": "DebertaV2Tokenizer",
	"big_bird": "DebertaV2Tokenizer", "clip": "CLIPTokenizer", "whisper": "WhisperTokenizer", "t5": "T5Tokenizer", "mt5": "T5Tokenizer",
	"umt5": "T5Tokenizer", "pegasus": "PegasusTokenizer", "xlnet": "XLNetTokenizer", "llama": "LlamaTokenizer", "mistral": "LlamaTokenizer",
	"gemma": "GemmaTokenizer", "gemma2": "GemmaTokenizer", "gpt2": "GPT2Tokenizer", "openai-gpt": "OpenAIGPTTokenizer", "qwen2": "Qwen2Tokenizer",
}

// Tokenizer classes that behave as another class does
var families = map[string]string{
	"DistilBertTokenizer": "BertTokenizer", "ElectraTokenizer": "BertTokenizer", "MobileBertTokenizer": "BertTokenizer", "BartTokenizer": "RobertaTokenizer",
	"LongformerTokenizer": "RobertaTokenizer", "LEDTokenizer": "RobertaTokenizer", "MvpTokenizer": "RobertaTokenizer", "BigBirdTokenizer": "DebertaV2Tokenizer",
	"MT5Tokenizer": "T5Tokenizer", "CodeLlamaTokenizer": "LlamaTokenizer",
}

// The special tokens of each tokenizer class, used for every role the settings leave unnamed
var classTokens = map[string]string{
	"BertTokenizer":       "unk=[UNK] sep=[SEP] pad=[PAD] cls=[CLS] mask=[MASK]",
	"RobertaTokenizer":    "bos=<s> eos=</s> unk=<unk> sep=</s> pad=<pad> cls=<s> mask=<mask>",
	"XLMRobertaTokenizer": "bos=<s> eos=</s> unk=<unk> sep=</s> pad=<pad> cls=<s> mask=<mask>",
	"CamembertTokenizer":  "bos=<s> eos=</s> unk=<unk> sep=</s> pad=<pad> cls=<s> mask=<mask>",
	"AlbertTokenizer":     "bos=[CLS] eos=[SEP] unk=<unk> sep=[SEP] pad=<pad> cls=[CLS] mask=[MASK]",
	"DebertaTokenizer":    "bos=[CLS] eos=[SEP] unk=[UNK] sep=[SEP] pad=[PAD] cls=[CLS] mask=[MASK]",
	"DebertaV2Tokenizer":  "bos=[CLS] eos=[SEP] unk=[UNK] sep=[SEP] pad=[PAD] cls=[CLS] mask=[MASK]",
	"XLNetTokenizer":      "bos=<s> eos=</s> unk=<unk> sep=<sep> pad=<pad> cls=<cls> mask=<mask>",
	"T5Tokenizer":         "eos=</s> unk=<unk> pad=<pad>",
	"PegasusTokenizer":    "eos=</s> unk=<unk> pad=<pad> mask=<mask_2>",
	"LlamaTokenizer":      "bos=<s> eos=</s> unk=<unk>",
	"GemmaTokenizer":      "bos=<bos> eos=<eos> unk=<unk> pad=<pad>",
	"GPT2Tokenizer":       "bos=<|endoftext|> eos=<|endoftext|> unk=<|endoftext|>",
	"WhisperTokenizer":    "bos=<|endoftext|> eos=<|endoftext|> unk=<|endoftext|>",
	"CLIPTokenizer":       "bos=<|startoftext|> eos=<|endoftext|> unk=<|endoftext|> pad=<|endoftext|>",
	"OpenAIGPTTokenizer":  "unk=<unk>",
}

// framing names the special tokens a tokenizer class puts before and after a sequence, each a role with its usual text
type framing struct {
	before, after [][2]string
	switched      bool
}

var (
	frameCLS  = framing{before: [][2]string{{"cls", "[CLS]"}}, after: [][2]string{{"sep", "[SEP]"}}}
	frameBOS  = framing{before: [][2]string{{"cls", "<s>"}}, after: [][2]string{{"sep", "</s>"}}}
	frameEOS  = framing{after: [][2]string{{"eos", "</s>"}}}
	frameHead = framing{before: [][2]string{{"bos", "<s>"}}, switched: true}
	frames    = map[string]framing{
		"BertTokenizer": frameCLS, "AlbertTokenizer": frameCLS, "DebertaV2Tokenizer": frameCLS, "DebertaTokenizer": frameCLS,
		"RobertaTokenizer": frameBOS, "XLMRobertaTokenizer": frameBOS, "CamembertTokenizer": frameBOS,
		"CLIPTokenizer":    {before: [][2]string{{"bos", "<|startoftext|>"}}, after: [][2]string{{"eos", "<|endoftext|>"}}},
		"WhisperTokenizer": {before: [][2]string{{"", "<|startoftranscript|>"}, {"", "<|notimestamps|>"}}, after: [][2]string{{"eos", "<|endoftext|>"}}},
		"T5Tokenizer":      frameEOS, "BlenderbotTokenizer": frameEOS, "PegasusTokenizer": frameEOS,
		"XLNetTokenizer": {after: [][2]string{{"sep", "<sep>"}, {"cls", "<cls>"}}},
		"LlamaTokenizer": frameHead, "GemmaTokenizer": {before: [][2]string{{"bos", "<bos>"}}, switched: true},
	}
)

// Special ids before and after a sequence by tokenizer class, the BOS and EOS switches deciding where the class reads them
func (cfg *settings) frame(fallback framing, id func(string) (int, bool)) ([]int, []int, error) {
	f, ok := frames[cfg.class]
	if !ok {
		f = fallback
		f.switched = true
	}
	if f.switched {
		if cfg.addBOS != nil {
			f.before = nil
			if *cfg.addBOS {
				f.before = [][2]string{{"bos", "<s>"}}
			}
		}
		if cfg.addEOS != nil {
			f.after = nil
			if *cfg.addEOS {
				f.after = [][2]string{{"eos", "</s>"}}
			}
		}
	}
	resolve := func(parts [][2]string) ([]int, error) {
		var ids []int
		for _, part := range parts {
			content := cfg.role[part[0]]
			if content == "" {
				content = part[1]
			}
			got, ok := id(content)
			if !ok {
				return nil, fmt.Errorf("tokenizers: the special token %q has no id in the vocabulary", content)
			}
			ids = append(ids, got)
		}
		return ids, nil
	}
	before, err := resolve(f.before)
	if err != nil {
		return nil, nil, err
	}
	after, err := resolve(f.after)
	return before, after, err
}

// Load reads the tokenizer of a snapshot from whichever tokenizer file it ships
func Load(src Source) (Encoder, error) {
	cfg, err := readSettings(src)
	if err != nil {
		return nil, err
	}
	if src.Has("tokenizer.json") {
		return loadJSON(src, cfg)
	}
	cfg.classDefaults()
	switch {
	case src.Has("tiktoken.model"):
		return loadTiktoken(src, "tiktoken.model", cfg)
	}
	for _, name := range sentencePieceFiles {
		if src.Has(name) {
			return loadSentencePiece(src, name, cfg)
		}
	}
	switch {
	case src.Has("vocab.json") && src.Has("merges.txt"):
		return loadMerges(src, cfg)
	case src.Has("vocab.txt"):
		return loadWordPiece(src, cfg)
	}
	return nil, fmt.Errorf("tokenizers: the repository ships none of the tokenizer files icebath loads, which are %s", Formats)
}

// The tokenizer.json pipeline with the tokens the settings declare added when the file lacks them
func loadJSON(src Source, cfg *settings) (*Tokenizer, error) {
	data, err := src.Read("tokenizer.json")
	if err != nil {
		return nil, err
	}
	t, err := FromJSON(data)
	if err != nil {
		return nil, err
	}
	return t, t.declare(cfg.declared())
}

// A tokenizer assembled from pipeline components written the way tokenizer.json writes them
func assemble(cfg *settings, fallback framing, vocab map[string]int, parts map[string]any) (*Tokenizer, error) {
	data, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	t, err := FromJSON(data)
	if err != nil {
		return nil, err
	}
	var present []AddedToken
	for _, token := range cfg.declared() {
		if _, ok := vocab[token.Content]; ok || token.ID >= 0 {
			present = append(present, token)
		}
	}
	if err := t.declare(present); err != nil {
		return nil, err
	}
	before, after, err := cfg.frame(fallback, t.tokenID)
	if err != nil {
		return nil, err
	}
	t.post = frame(before, after)
	return t, nil
}

type part = map[string]any

// The WordPiece tokenizer of a vocab.txt, one token per line, with the BERT text cleaning the settings select
func loadWordPiece(src Source, cfg *settings) (*Tokenizer, error) {
	data, err := src.Read("vocab.txt")
	if err != nil {
		return nil, err
	}
	vocab := map[string]int{}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		vocab[strings.TrimSuffix(line, "\r")] = i
	}
	for _, role := range [][2]string{{"unk", "[UNK]"}, {"sep", "[SEP]"}, {"pad", "[PAD]"}, {"cls", "[CLS]"}, {"mask", "[MASK]"}} {
		if cfg.role[role[0]] == "" {
			cfg.role[role[0]] = role[1]
		}
	}
	return assemble(cfg, frameCLS, vocab, part{
		"normalizer":    part{"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": cfg.chineseChars, "strip_accents": cfg.stripAccents, "lowercase": cfg.lower(true)},
		"pre_tokenizer": part{"type": "BertPreTokenizer"},
		"model":         part{"type": "WordPiece", "vocab": vocab, "unk_token": cfg.role["unk"], "continuing_subword_prefix": "##", "max_input_chars_per_word": 100},
		"decoder":       part{"type": "WordPiece", "prefix": "##", "cleanup": true},
	})
}

const clipSplit = `<\|startoftext\|>|<\|endoftext\|>|'s|'t|'re|'ve|'m|'ll|'d|[\p{L}]+|[\p{N}]|[^\s\p{L}\p{N}]+`

const qwenSplit = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// The byte pair tokenizer of a vocab.json and merges.txt, byte level unless the vocabulary marks word ends
func loadMerges(src Source, cfg *settings) (*Tokenizer, error) {
	data, err := src.Read("vocab.json")
	if err != nil {
		return nil, err
	}
	vocab := map[string]int{}
	if err := json.Unmarshal(data, &vocab); err != nil {
		return nil, fmt.Errorf("tokenizers: vocab.json: %w", err)
	}
	if data, err = src.Read("merges.txt"); err != nil {
		return nil, err
	}
	var merges [][2]string
	wordEnds := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#version") {
			continue
		}
		left, right, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("tokenizers: merges.txt line %q is not two tokens", line)
		}
		wordEnds = wordEnds || strings.HasSuffix(right, "</w>")
		merges = append(merges, [2]string{left, strings.TrimSpace(right)})
	}
	model := part{"type": "BPE", "vocab": vocab, "merges": merges}
	byteLevel := part{"type": "ByteLevel", "add_prefix_space": cfg.prefixSpace, "use_regex": true}
	switch {
	case cfg.class == "CLIPTokenizer":
		if cfg.role["unk"] == "" {
			cfg.role["unk"] = "<|endoftext|>"
		}
		model["unk_token"], model["end_of_word_suffix"] = cfg.role["unk"], "</w>"
		return assemble(cfg, framing{}, vocab, part{
			"normalizer": part{"type": "Sequence", "normalizers": []part{{"type": "NFC"}, {"type": "Replace", "pattern": part{"Regex": `\s+`}, "content": " "}, {"type": "Lowercase"}}},
			"pre_tokenizer": part{"type": "Sequence", "pretokenizers": []part{
				{"type": "Split", "pattern": part{"Regex": clipSplit}, "behavior": "Removed", "invert": true}, {"type": "ByteLevel", "add_prefix_space": false, "use_regex": true}}},
			"model": model, "decoder": part{"type": "ByteLevel"},
		})
	case cfg.class == "Qwen2Tokenizer":
		return assemble(cfg, framing{}, vocab, part{
			"normalizer": part{"type": "NFC"},
			"pre_tokenizer": part{"type": "Sequence", "pretokenizers": []part{
				{"type": "Split", "pattern": part{"Regex": qwenSplit}, "behavior": "Isolated", "invert": false}, {"type": "ByteLevel", "add_prefix_space": cfg.prefixSpace, "use_regex": false}}},
			"model": model, "decoder": part{"type": "ByteLevel"},
		})
	case wordEnds:
		if cfg.role["unk"] == "" {
			cfg.role["unk"] = "<unk>"
		}
		model["unk_token"], model["end_of_word_suffix"] = cfg.role["unk"], "</w>"
		return assemble(cfg, framing{}, vocab, part{
			"normalizer":    part{"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "lowercase": true},
			"pre_tokenizer": part{"type": "BertPreTokenizer"},
			"model":         model, "decoder": part{"type": "BPEDecoder", "suffix": "</w>"},
		})
	}
	return assemble(cfg, framing{}, vocab, part{"pre_tokenizer": byteLevel, "model": model, "decoder": part{"type": "ByteLevel"}})
}

// pieceTokenizer encodes with a SentencePiece model between the special tokens it cuts out of the text
type pieceTokenizer struct {
	model   *sentencepiece.Processor
	special *matcher
	before  []int
	after   []int
	offset  int
	unk     int
	clean   func(string) string
}

// The text cleaning ALBERT and XLNet apply before the model, spaces collapsed, quote pairs joined, accents dropped, case folded
func pieceCleaner(cfg *settings, lower bool) func(string) string {
	return func(s string) string {
		if cfg.removeSpace {
			s = strings.Join(strings.Fields(s), " ")
		}
		s = strings.ReplaceAll(strings.ReplaceAll(s, "``", `"`), "''", `"`)
		if !cfg.keepAccents {
			s = strings.Map(func(r rune) rune {
				if norm.NFD.PropertiesString(string(r)).CCC() != 0 {
					return -1
				}
				return r
			}, norm.NFKD.String(s))
		}
		if lower {
			s = cases.Lower(language.Und).String(s)
		}
		return s
	}
}

// Id of a model piece moved by the class offset, the unknown piece taking its reserved id
func (t *pieceTokenizer) shifted(id int) int {
	if id == 0 && t.unk >= 0 {
		return t.unk
	}
	return id + t.offset
}

func (t *pieceTokenizer) Encode(s string) ([]int, error) {
	ids := append([]int(nil), t.before...)
	for _, p := range t.special.cut(newText(strings.ToValidUTF8(s, "\uFFFD"))) {
		if p.fixed {
			ids = append(ids, p.id)
			continue
		}
		if t.clean != nil {
			p.s = t.clean(p.s)
		}
		got, err := t.model.EncodeAsIDs(p.s)
		if err != nil {
			return nil, err
		}
		for _, id := range got {
			ids = append(ids, t.shifted(id))
		}
	}
	return append(ids, t.after...), nil
}

// The SentencePiece model of a snapshot, a file under that name in the tiktoken rank layout read as tiktoken
func loadSentencePiece(src Source, name string, cfg *settings) (Encoder, error) {
	data, err := src.Read(name)
	if err != nil {
		return nil, err
	}
	model, err := sentencepiece.New(data)
	if err != nil {
		if _, rankErr := tiktoken.ParseRanks(data); rankErr == nil {
			return loadTiktoken(src, name, cfg)
		}
		return nil, fmt.Errorf("tokenizers: %s: %w", name, err)
	}
	t := &pieceTokenizer{model: model, special: newMatcher(), unk: -1}
	fixed := map[string]int{}
	switch cfg.class {
	case "AlbertTokenizer":
		t.clean = pieceCleaner(cfg, cfg.lower(true))
	case "XLNetTokenizer":
		t.clean = pieceCleaner(cfg, cfg.lower(false))
	case "DebertaV2Tokenizer":
		if cfg.lower(false) {
			t.clean = cases.Lower(language.Und).String
		}
	case "XLMRobertaTokenizer":
		t.offset, t.unk = 1, 3
		fixed = map[string]int{"<s>": 0, "<pad>": 1, "</s>": 2, "<unk>": 3, "<mask>": model.Size() + 1}
	case "CamembertTokenizer":
		t.offset, t.unk = 4, 3
		fixed = map[string]int{"<s>NOTUSED": 0, "<pad>": 1, "</s>NOTUSED": 2, "<unk>": 3, "<unk>NOTUSED": 4, "<mask>": model.Size() + 4}
		cfg.extra = append(cfg.extra, "<s>NOTUSED", "</s>NOTUSED", "<unk>NOTUSED")
	case "PegasusTokenizer":
		t.offset = cfg.offset
		fixed = map[string]int{"<pad>": 0, "</s>": 1, "<mask_1>": 2, "<mask_2>": 3}
		cfg.extra = append(cfg.extra, "<mask_1>")
		for i := 2; i < cfg.offset; i++ {
			content := fmt.Sprintf("<unk_%d>", i)
			fixed[content] = i + 2
			cfg.extra = append(cfg.extra, content)
		}
	}
	for i := 0; i < cfg.extraIDs; i++ {
		content := fmt.Sprintf("<extra_id_%d>", i)
		fixed[content] = model.Size() + cfg.extraIDs - 1 - i
		cfg.extra = append(cfg.extra, content)
	}
	id := func(content string) (int, bool) {
		if id, ok := fixed[content]; ok {
			return id, true
		}
		if id, ok := model.PieceID(content); ok {
			return t.shifted(id), true
		}
		for _, token := range cfg.added {
			if token.Content == content {
				return token.ID, true
			}
		}
		return 0, false
	}
	for _, token := range cfg.declared() {
		if got, ok := id(token.Content); ok {
			token.ID = got
			t.special.add(token.Content, token)
		}
	}
	t.before, t.after, err = cfg.frame(framing{}, id)
	return t, err
}

const (
	// The split pattern of the Moonshot tiktoken tokenizer, Han runs first, in regexp2 syntax
	moonshotSplit = `[\p{Han}]+` +
		`|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}-[\p{Han}]]*[\p{Ll}\p{Lm}\p{Lo}\p{M}-[\p{Han}]]+(?:'[sS\x{17F}]|'[tT]|'[rR][eE]|'[vV][eE]|'[mM]|'[lL][lL]|'[dD])?` +
		`|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}-[\p{Han}]]+[\p{Ll}\p{Lm}\p{Lo}\p{M}-[\p{Han}]]*(?:'[sS\x{17F}]|'[tT]|'[rR][eE]|'[vV][eE]|'[mM]|'[lL][lL]|'[dD])?` +
		`|\p{N}{1,3}` +
		`| ?[^\s\p{L}\p{N}]+[\r\n]*` +
		`|\s*[\r\n]+` +
		`|\s+(?!\S)` +
		`|\s+`
	// The split pattern of every other tiktoken rank file
	rankSplit = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

	moonshotReserved = 256
	moonshotChunk    = 400_000
	moonshotRun      = 25_000
)

// rankTokenizer encodes with a tiktoken rank file, cutting the text first when its class bounds chunk and run lengths
type rankTokenizer struct {
	enc     *tiktoken.Encoding
	bounded bool
}

func rankSpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1C && r <= 0x1F }

// Pieces of s holding no run of white space or of anything else longer than limit runes
func boundRuns(s []rune, limit int) [][]rune {
	var out [][]rune
	run, start := 0, 0
	space := len(s) > 0 && rankSpace(s[0])
	for i, r := range s {
		if now := rankSpace(r); now != space {
			run, space = 1, now
			continue
		}
		run++
		if run > limit {
			out = append(out, s[start:i])
			start, run = i, 1
		}
	}
	return append(out, s[start:])
}

func (t *rankTokenizer) Encode(s string) ([]int, error) {
	if !t.bounded {
		return t.enc.Encode(s), nil
	}
	runes := []rune(s)
	var ids []int
	for i := 0; i < len(runes); i += moonshotChunk {
		for _, run := range boundRuns(runes[i:min(i+moonshotChunk, len(runes))], moonshotRun) {
			ids = append(ids, t.enc.Encode(string(run))...)
		}
	}
	return ids, nil
}

// The tiktoken rank file of a snapshot with the special tokens the settings place after its ranks
func loadTiktoken(src Source, name string, cfg *settings) (Encoder, error) {
	data, err := src.Read(name)
	if err != nil {
		return nil, err
	}
	ranks, err := tiktoken.ParseRanks(data)
	if err != nil {
		return nil, fmt.Errorf("%w in %s", err, name)
	}
	moonshot := cfg.class == "TikTokenTokenizer"
	last := len(ranks) - 1
	pattern := rankSplit
	if moonshot {
		last, pattern = len(ranks)+moonshotReserved-1, moonshotSplit
	} else {
		for id := range cfg.added {
			last = max(last, id)
		}
	}
	special := map[string]int{}
	for id := len(ranks); id <= last; id++ {
		content := fmt.Sprintf("<|reserved_token_%d|>", id)
		if token, ok := cfg.added[id]; ok {
			content = token.Content
		}
		special[content] = id
	}
	for _, token := range cfg.declared() {
		if _, ok := special[token.Content]; ok || token.ID >= 0 || moonshot {
			continue
		}
		last++
		special[token.Content] = last
	}
	enc, err := tiktoken.New(name, pattern, ranks, special)
	if err != nil {
		return nil, err
	}
	return &rankTokenizer{enc: enc, bounded: moonshot}, nil
}
