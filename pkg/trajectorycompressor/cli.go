package trajectorycompressor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrandom"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// SamplePercent is a sampling percentage with the Python repr it was given as, an int or a float
type SamplePercent struct {
	Value float64
	Repr  string
}

// Options are the arguments of the trajectory compressor command
type Options struct {
	Input           string
	Output          string
	Config          string
	TargetMaxTokens int
	Tokenizer       string
	SamplePercent   *SamplePercent
	Seed            int64
	DryRun          bool
}

// PurePath name, stem, parent and child join of a path string
func pathName(p string) string {
	s := pypath.PurePathStr(p)
	if s == "." || strings.HasSuffix(s, string(filepath.Separator)) {
		return ""
	}
	return filepath.Base(s)
}

func pathStem(p string) string {
	name := pathName(p)
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}

func pathParent(p string) string { return filepath.Dir(pypath.PurePathStr(p)) }

// Loads the YAML config or the defaults when it is missing, then applies the CLI overrides
func loadCLIConfig(config string, targetMaxTokens int, tokenizer string) (*CompressionConfig, error) {
	var cfg *CompressionConfig
	if _, err := os.Stat(config); err == nil {
		fmt.Printf("📋 Loading config from %s\n", config)
		if cfg, err = ConfigFromYAML(config); err != nil {
			return nil, err
		}
	} else {
		fmt.Printf("⚠️  Config not found at %s, using defaults\n", config)
		cfg = DefaultConfig()
	}
	if targetMaxTokens != 0 {
		cfg.TargetMaxTokens = targetMaxTokens
	}
	if tokenizer != "" {
		cfg.TokenizerName = tokenizer
	}
	return cfg, nil
}

func printDryRun(icon, target, outputPath string) {
	fmt.Println("\n🔍 DRY RUN MODE - analyzing without writing")
	fmt.Printf("%s Would process: %s\n", icon, target)
	fmt.Printf("%s Would output to: %s\n", icon, outputPath)
}

// Sample of about sample_percent of entries, at least one and at most all of them
func sampleEntries(rng *pyrandom.Random, entries []any, percent float64) ([]any, error) {
	k := min(max(1, int(float64(len(entries))*percent/100)), len(entries))
	return pyrandom.Sample(rng, entries, k)
}

// Entries of a JSONL file without their line numbers
func entryValues(entries []jsonlEntry) []any {
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e.entry
	}
	return out
}

// Copies a file with its permission bits as shutil.copy does
func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chmod(dst, st.Mode().Perm())
}

// Temporary directory in the Python tempfile location, removed by the returned cleanup
func tempDir() (string, func(), error) {
	base, err := pypath.Gettempdir()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "tmp")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// Single file mode, sampled entries compressed through a temp directory and merged into one output file
func runFileMode(ctx context.Context, inputPath, output string, cfg *CompressionConfig, sample *SamplePercent, seed int64, dryRun bool) error {
	fmt.Println("📄 Input mode: Single JSONL file")
	outputPath := joinPath(pathParent(inputPath), pathStem(inputPath)+cfg.OutputSuffix+".jsonl")
	if output != "" {
		outputPath = pypath.PurePathStr(output)
	}
	loaded, err := loadJSONL(inputPath, func(n int, e error) { fmt.Printf("⚠️  Skipping invalid JSON at line %d: %s\n", n, e) }, 1)
	if err != nil {
		return err
	}
	entries := entryValues(loaded)
	total := len(entries)
	fmt.Printf("   Loaded %s trajectories from %s\n", pystr.GroupThousands(int64(total)), pathName(inputPath))
	if sample != nil {
		rng := pyrandom.New(seed)
		if entries, err = pyrandom.Sample(rng, entries, max(1, int(float64(total)*sample.Value/100))); err != nil {
			return err
		}
		fmt.Printf("   Sampled %s trajectories (%s%% of %s)\n", pystr.GroupThousands(int64(len(entries))), sample.Repr, pystr.GroupThousands(int64(total)))
	}
	if dryRun {
		printDryRun("📄", pystr.GroupThousands(int64(len(entries)))+" trajectories", outputPath)
		return nil
	}
	tmp, cleanup, err := tempDir()
	if err != nil {
		return err
	}
	defer cleanup()
	tmpIn, tmpOut := filepath.Join(tmp, "input"), filepath.Join(tmp, "output")
	if err := os.Mkdir(tmpIn, 0o777); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(tmpIn, "trajectories.jsonl"), entries); err != nil {
		return err
	}
	comp, err := NewCompressor(ctx, cfg)
	if err != nil {
		return err
	}
	if err := comp.ProcessDirectory(ctx, tmpIn, tmpOut); err != nil {
		return err
	}
	if err := os.MkdirAll(pathParent(outputPath), 0o777); err != nil {
		return err
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	for _, f := range globJSONL(tmpOut) {
		in, err := os.Open(f)
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	metricsFile := filepath.Join(tmpOut, cfg.MetricsOutputFile)
	if _, err := os.Stat(metricsFile); err == nil {
		metricsOutput := joinPath(pathParent(outputPath), pathStem(outputPath)+"_metrics.json")
		if err := copyFile(metricsFile, metricsOutput); err != nil {
			return err
		}
		fmt.Printf("💾 Metrics saved to %s\n", metricsOutput)
	}
	fmt.Println("\n✅ Compression complete!")
	fmt.Printf("📄 Output: %s\n", outputPath)
	return nil
}

// Directory mode, compressed in place or sampled per file into a temp directory first
func runDirMode(ctx context.Context, inputPath, output string, cfg *CompressionConfig, sample *SamplePercent, seed int64, dryRun bool) error {
	fmt.Println("📁 Input mode: Directory of JSONL files")
	outputPath := joinPath(pathParent(inputPath), pathName(inputPath)+cfg.OutputSuffix)
	if output != "" {
		outputPath = pypath.PurePathStr(output)
	}
	if sample == nil {
		if dryRun {
			printDryRun("📁", pypath.PurePathStr(inputPath), outputPath)
			return nil
		}
		comp, err := NewCompressor(ctx, cfg)
		if err != nil {
			return err
		}
		if err := comp.ProcessDirectory(ctx, inputPath, outputPath); err != nil {
			return err
		}
		fmt.Println("\n✅ Compression complete!")
		return nil
	}
	fmt.Printf("\n⚠️  Sampling from directory: will sample %s%% from each file\n", sample.Repr)
	tmp, cleanup, err := tempDir()
	if err != nil {
		return err
	}
	defer cleanup()
	tmpIn := filepath.Join(tmp, "input")
	if err := os.Mkdir(tmpIn, 0o777); err != nil {
		return err
	}
	rng := pyrandom.New(seed)
	totalOriginal, totalSampled := 0, 0
	for _, f := range globJSONL(pypath.PurePathStr(inputPath)) {
		loaded, err := loadJSONL(f, nil, 0)
		if err != nil {
			return err
		}
		entries := entryValues(loaded)
		sampled, err := sampleEntries(rng, entries, sample.Value)
		if err != nil {
			return err
		}
		totalOriginal += len(entries)
		totalSampled += len(sampled)
		if err := writeJSONL(filepath.Join(tmpIn, filepath.Base(f)), sampled); err != nil {
			return err
		}
	}
	fmt.Printf("   Sampled %s from %s total trajectories\n", pystr.GroupThousands(int64(totalSampled)), pystr.GroupThousands(int64(totalOriginal)))
	if dryRun {
		printDryRun("📁", tmpIn, outputPath)
		return nil
	}
	comp, err := NewCompressor(ctx, cfg)
	if err != nil {
		return err
	}
	if err := comp.ProcessDirectory(ctx, tmpIn, outputPath); err != nil {
		return err
	}
	fmt.Println("\n✅ Compression complete!")
	return nil
}

// Compresses a JSONL file or a directory of them into the target token budget, optionally sampling first
func Run(ctx context.Context, o Options) error {
	fmt.Println("🗜️  Trajectory Compressor")
	fmt.Println(strings.Repeat("=", 60))
	cfg, err := loadCLIConfig(o.Config, o.TargetMaxTokens, o.Tokenizer)
	if err != nil {
		return err
	}
	if o.SamplePercent != nil {
		if o.SamplePercent.Value <= 0 || o.SamplePercent.Value > 100 {
			fmt.Printf("❌ sample_percent must be between 1 and 100, got %s\n", o.SamplePercent.Repr)
			return nil
		}
		fmt.Printf("🎲 Will sample %s%% of trajectories (seed=%d)\n", o.SamplePercent.Repr, o.Seed)
	}
	st, err := os.Stat(o.Input)
	if err != nil {
		fmt.Printf("❌ Input not found: %s\n", o.Input)
		return nil
	}
	if st.Mode().IsRegular() {
		return runFileMode(ctx, o.Input, o.Output, cfg, o.SamplePercent, o.Seed, o.DryRun)
	}
	return runDirMode(ctx, o.Input, o.Output, cfg, o.SamplePercent, o.Seed, o.DryRun)
}

// Python literal value of a numeric argument, int or float with its repr
func parseSamplePercent(s string) (*SamplePercent, error) {
	if n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64); err == nil {
		return &SamplePercent{Value: float64(n), Repr: strconv.FormatInt(n, 10)}, nil
	}
	if f, err := pyint.ParseFloat(s); err == nil && !strings.ContainsAny(strings.ToLower(s), "in") {
		return &SamplePercent{Value: f, Repr: pyrepr.Float(f)}, nil
	}
	return nil, &pyint.Error{Kind: "TypeError", Msg: "'<=' not supported between instances of 'str' and 'int'"}
}

// Truthiness of the literal fire parses from a flag value, bool and number literals by value and other text as a string
func fireTruthy(v string) bool {
	switch v {
	case "true", "True":
		return true
	case "false", "False", "None", "":
		return false
	}
	if sp, err := parseSamplePercent(v); err == nil {
		return sp.Value != 0
	}
	return true
}

const usageLine = "Usage: trajectory_compressor INPUT <flags>\n" +
	"  optional flags:        --output | --config | --target_max_tokens |\n" +
	"                         --tokenizer | --sample_percent | --seed | --dry_run\n\n" +
	"For detailed information on this command, run:\n" +
	"  trajectory_compressor --help"

// Exit status 2 error printed in the fire usage layout
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// Command line of the compressor with fire style flags and positional arguments bound in parameter order
func NewCommand() *cobra.Command {
	var o Options
	var targetMax, seed, sample string
	cmd := &cobra.Command{
		Use:   "trajectory_compressor INPUT [OUTPUT] [CONFIG] [TARGET_MAX_TOKENS] [TOKENIZER] [SAMPLE_PERCENT] [SEED] [DRY_RUN]",
		Short: "Compress agent trajectories to fit within a target token budget.",
		Long: "Compress agent trajectories to fit within a target token budget.\n\n" +
			"Supports both single JSONL files and directories containing multiple JSONL files.\n" +
			"Optionally sample a percentage of trajectories before compression.",
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := []string{"input", "output", "config", "target_max_tokens", "tokenizer", "sample_percent", "seed", "dry_run"}
			given := map[string]string{}
			for _, p := range params {
				if f := cmd.Flags().Lookup(p); f != nil && f.Changed {
					given[p] = f.Value.String()
				}
			}
			for _, p := range params {
				if _, ok := given[p]; ok || len(args) == 0 {
					continue
				}
				given[p], args = args[0], args[1:]
			}
			if len(args) > 0 {
				return &usageError{"ERROR: Could not consume arg: " + args[0] + "\n" + usageLine}
			}
			input, ok := given["input"]
			if !ok {
				return &usageError{"ERROR: The function received no value for the required argument: input\n" + usageLine}
			}
			o.Input, o.Output, o.Tokenizer = input, given["output"], given["tokenizer"]
			o.Config = "configs/trajectory_compression.yaml"
			if v, ok := given["config"]; ok {
				o.Config = v
			}
			var err error
			if v, ok := given["target_max_tokens"]; ok {
				if o.TargetMaxTokens, err = strconv.Atoi(strings.ReplaceAll(v, "_", "")); err != nil {
					return &pyint.Error{Kind: "TypeError", Msg: "'" + v + "' is not an integer target_max_tokens"}
				}
			}
			o.Seed = 42
			if v, ok := given["seed"]; ok {
				if o.Seed, err = strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 10, 64); err != nil {
					return &pyint.Error{Kind: "TypeError", Msg: "'" + v + "' is not an integer seed"}
				}
			}
			if v, ok := given["sample_percent"]; ok {
				if o.SamplePercent, err = parseSamplePercent(v); err != nil {
					return err
				}
			}
			if v, ok := given["dry_run"]; ok {
				o.DryRun = fireTruthy(v)
			}
			return Run(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Input, "input", "", "Path to JSONL file or directory containing JSONL files")
	f.StringVar(&o.Output, "output", "", "Output path (file for file input, directory for dir input), default adds the _compressed suffix")
	f.StringVar(&o.Config, "config", "configs/trajectory_compression.yaml", "Path to YAML configuration file")
	f.StringVar(&targetMax, "target_max_tokens", "", "Override target token count from config")
	f.StringVar(&o.Tokenizer, "tokenizer", "", "Override tokenizer name from config")
	f.StringVar(&sample, "sample_percent", "", "Sample this percentage of trajectories (1-100) before compression")
	f.StringVar(&seed, "seed", "42", "Random seed for sampling reproducibility")
	f.BoolVar(&o.DryRun, "dry_run", false, "Analyze without compressing (just show what would happen)")
	f.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "-", "_"))
	})
	return cmd
}

// Process entry of the compressor binary, the .env files loaded first and a Python style error line on failure
func Main(ctx context.Context, args []string) int {
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: home.Dir(ctx), ProjectEnv: filepath.Join(pm.RepoRoot(), ".env")}); err != nil {
		fmt.Fprintln(os.Stderr, pyErrorLine(err))
		return 1
	}
	cmd := NewCommand()
	cmd.SetArgs(args)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		arg := err.Error()
		if i := strings.LastIndex(arg, " "); i >= 0 {
			arg = arg[i+1:]
		}
		return &usageError{"ERROR: Could not consume arg: " + arg + "\n" + usageLine}
	})
	if err := cmd.ExecuteContext(ctx); err != nil {
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, ue.msg)
			return 2
		}
		fmt.Fprintln(os.Stderr, pyErrorLine(err))
		return 1
	}
	return 0
}

// Last traceback line of an uncaught error, the exception class before its message
func pyErrorLine(err error) string {
	var pe *pyint.Error
	if errors.As(err, &pe) {
		return pe.Kind + ": " + pe.Msg
	}
	return err.Error()
}
