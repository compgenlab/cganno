// Command cgvant is the interactive CLI for variant annotation.
//
// Usage:
//
//	cgvant [-home DIR] [-snapshot NAME] <command> [args]
//
// CGVANT_HOME (the -home flag, else $CGVANT_HOME, else the current directory)
// is the base directory holding config.toml; config values may reference it as
// $CGVANT_HOME (e.g. data_dir = "$CGVANT_HOME/data"). Sources and snapshot manifests
// live under annotations_dir (sources/, snapshots/). A source is a data file, a
// type="builtin" bundle, or a type="tool" external annotator.
//
// Config commands:
//
//	init                        scaffold config.toml + a starter snapshot
//	snapshot add <name> [--from] create a snapshot manifest (optionally copy another)
//	snapshot list               list snapshots
//	source add <snapshot> ...   add a source and reference it from a snapshot
//	annotation add <snapshot> .. add an annotation field to a source
//	download [--source N] [-j N] fetch the snapshot's sources (incl. tool images) + index
//	registry ...                pull pre-made snapshot/source configs from a catalog
//
// Annotation commands:
//
//	annotate [--format tab|vcf|json|text] <vcf|locus...>  annotate loci (default: tab)
//	annotate --format vcf -o <out.vcf> <in.vcf>  write a fully-annotated VCF
//	versions                    show the active snapshot
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	annotatepkg "github.com/compgenlab/cgvant/internal/annotate"
	"github.com/compgenlab/cgvant/internal/annotator"
	"github.com/compgenlab/cgvant/internal/annotator/overlay"
	"github.com/compgenlab/cgvant/internal/config"
	"github.com/compgenlab/cgvant/internal/engine"
	"github.com/compgenlab/cgvant/internal/fetch"
	"github.com/compgenlab/cgvant/internal/model"
	"github.com/compgenlab/cgvant/internal/store"
	"github.com/compgenlab/cgvant/internal/store/sqlite"
	"github.com/compgenlab/cgvant/internal/vcf"
)

// version is stamped at build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// resolveHome returns CGVANT_HOME: the --home flag, else $CGVANT_HOME, else the
// current directory; resolved to an absolute path. It exports the result back to
// the environment so $CGVANT_HOME references inside config.toml resolve.
func resolveHome(flagHome string) string {
	home := flagHome
	if home == "" {
		home = os.Getenv("CGVANT_HOME")
	}
	if home == "" {
		home = "."
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	os.Setenv("CGVANT_HOME", home)
	return home
}

func run(args []string) error {
	fs := flag.NewFlagSet("cgvant", flag.ContinueOnError)
	home := fs.String("home", "", "cgvant home dir (default: $CGVANT_HOME or CWD); holds config.toml")
	snapshotName := fs.String("snapshot", "", "snapshot to use (default: config default_snapshot)")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfgPathStr := filepath.Join(resolveHome(*home), "config.toml")
	cfgPath := &cfgPathStr
	rest := fs.Args()
	if len(rest) == 0 {
		usage()
		return fmt.Errorf("no command given")
	}
	cmd, cmdArgs := rest[0], rest[1:]
	ctx := context.Background()

	if cmd == "version" {
		fmt.Println("cgvant", version)
		return nil
	}

	// Config-management commands operate on files, not the engine. `annotate`
	// also routes here because its -o (VCF-output) mode uses the streaming
	// pipeline rather than the cache engine.
	switch cmd {
	case "init":
		return cmdInit(*cfgPath, cmdArgs)
	case "snapshot":
		return cmdSnapshot(*cfgPath, cmdArgs)
	case "source":
		return cmdSource(*cfgPath, cmdArgs)
	case "annotation":
		return cmdAnnotation(*cfgPath, cmdArgs)
	case "annotate":
		return cmdAnnotate(ctx, *cfgPath, *snapshotName, cmdArgs)
	case "download":
		return cmdDownload(ctx, *cfgPath, *snapshotName, cmdArgs)
	case "registry":
		return cmdRegistry(ctx, *cfgPath, cmdArgs)
	case "configure", "edit":
		return cmdEdit(*cfgPath, cmdArgs)
	case "bgzip": // hidden: BGZF compress (mimics bgzip) for recipes/tool scripts
		return cmdBgzip(cmdArgs)
	case "tabix": // hidden: write a tabix index (mimics tabix) for recipes/tool scripts
		return cmdTabix(cmdArgs)
	}

	// `versions` needs the engine (config + snapshot + store + annotator).
	if cmd == "versions" {
		eng, closeFn, err := buildEngine(ctx, *cfgPath, *snapshotName)
		if err != nil {
			return err
		}
		defer closeFn()
		return cmdVersions(eng)
	}

	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func usage() {
	fmt.Fprint(os.Stderr, `cgvant — variant annotation CLI

usage: cgvant [-home DIR] [-snapshot NAME] <command> [args]

CGVANT_HOME (-home flag, else $CGVANT_HOME, else CWD) is the base dir holding
config.toml; config values may reference it, e.g. data_dir = "$CGVANT_HOME/data".

config commands:
  init                         scaffold config.toml + a starter snapshot
  configure | edit             interactive editor: snapshots, sources, annotations
  snapshot add <name> [--from S]  create a snapshot (optionally copy from S)
  snapshot list                list snapshots
  source add [flags] [--snapshot S]  add a source (prompts when flags omitted)
  annotation add --source R [flags]  add an annotation to a source
  annotation list <snapshot>   list annotations + the default set
  download [--source N] [--force] [-j N]  fetch the snapshot's sources (incl. tool images) + index
  registry list|pull-snapshot <name>|add-source <name[:version]> [--snapshot S]
  registry submit <name[:version]> [--dry-run]  propose a source to the public registry

annotation commands:
  annotate [--all|-a name,...] [--format tab|vcf|json|text] [-o FILE] <vcf|locus...>
                               annotate loci (default format: tab; -o writes to a file)
  versions                     show the active snapshot
  version                      print the cgvant version
`)
}

// buildEngine loads the config + snapshot and builds the annotate engine.
func buildEngine(ctx context.Context, cfgPath, snapshotName string) (*engine.Engine, func(), error) {
	if err := config.MustExist(cfgPath); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	snap, err := cfg.LoadSnapshot(snapshotName)
	if err != nil {
		return nil, nil, err
	}
	return buildEngineWith(ctx, cfg, snap)
}

// buildEngineWith builds the annotate engine over the store and the overlay (tabix
// source) annotator, for an already-loaded config + snapshot.
func buildEngineWith(ctx context.Context, cfg *config.Config, snap *config.Snapshot) (*engine.Engine, func(), error) {
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	eng, err := newEngineOverStore(ctx, cfg, snap, st, nil)
	if err != nil {
		if st != nil {
			st.Close()
		}
		return nil, nil, err
	}
	return eng, func() {
		if st != nil {
			st.Close()
		}
	}, nil
}

// newEngineOverStore builds the annotate engine over an already-open store: the
// overlay annotator is one tabix source per pinned (non-builtin) source, plus
// `extra` sources (e.g. tool outputs projected via Tool.AsSource, used on the
// cache/locus path) read from their LocalPath.
func newEngineOverStore(ctx context.Context, cfg *config.Config, snap *config.Snapshot, st store.Store, extra []config.Source) (*engine.Engine, error) {
	var srcs []annotator.SourceAnnotator
	for _, s := range snap.Sources {
		if s.IsBuiltinSource() || s.IsTool() {
			continue // tool sources have no static file; their output arrives via `extra`
		}
		srcs = append(srcs, overlay.NewSource(s, cfg.ResolveSourceFiles(s), snap.Annotations))
	}
	for _, s := range extra {
		srcs = append(srcs, overlay.NewSource(s, []config.SourceFile{{Path: s.LocalPath}}, snap.Annotations))
	}
	ann := annotator.NewComposite(srcs, 0)

	eng := engine.New(st, ann, snap.Name, snap.Assembly, snap.DataSources())
	if err := eng.Init(ctx); err != nil {
		return nil, err
	}
	return eng, nil
}

// openStore opens the configured annotation-cache backend.
// openStore opens the configured cache store, or returns (nil, nil) when the cache is
// disabled (no [database]) — callers treat a nil store as "compute, don't persist".
func openStore(cfg *config.Config) (store.Store, error) {
	if !cfg.CacheEnabled() {
		return nil, nil
	}
	switch cfg.Database.Backend {
	case "sqlite":
		return sqlite.Open(cfg.DatabasePathAbs())
	case "postgres":
		return nil, fmt.Errorf("postgres backend not yet implemented")
	default:
		return nil, fmt.Errorf("unsupported backend %q", cfg.Database.Backend)
	}
}

// cmdAnnotate runs either the VCF→annotated-VCF pipeline (with -o) or the
// cache/loci annotate path (printing values). Which annotations are applied is
// selected by --all / -a (else the snapshot's default-marked annotations).
func cmdAnnotate(ctx context.Context, cfgPath, snapshot string, args []string) error {
	fs := flag.NewFlagSet("annotate", flag.ContinueOnError)
	out := fs.String("o", "", "write output to this file (default: stdout)")
	format := fs.String("format", "tab", "output format: tab|vcf|json|text")
	all := fs.Bool("all", false, "apply all annotations (else the default-marked set)")
	var keys stringList
	fs.Var(&keys, "annotation", "annotation name to apply (repeatable, comma-separated)")
	fs.Var(&keys, "a", "shorthand for --annotation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if *all && len(keys) > 0 {
		return fmt.Errorf("use --all or -a, not both")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	snap, err := cfg.LoadSnapshot(snapshot)
	if err != nil {
		return err
	}
	selected, err := snap.SelectAnnotations(keys, *all)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no annotations selected — mark some `default = true`, or pass --all / -a key[,key...]")
	}

	// vcf output uses the streaming pipeline (preserves samples for a VCF input;
	// synthesizes a sites-only VCF for bare loci). Other formats use the engine.
	if *format == "vcf" {
		sub := *snap
		sub.Annotations = selected
		sub.Sources = withSelectedTools(snap, selected)
		if err := requireSources(cfg, snap, selected); err != nil {
			return err
		}
		var st store.Store
		if len(referencedTools(snap, selected)) > 0 && cfg.CacheEnabled() {
			if st, err = openStore(cfg); err != nil {
				return err
			}
			defer st.Close()
			if err := st.Init(ctx); err != nil {
				return err
			}
		}
		inPath, cleanup, err := annotateInputVCF(rest)
		if err != nil {
			return err
		}
		defer cleanup()
		return annotatepkg.AnnotateVCFSnapshot(ctx, cfg, &sub, inPath, *out, st)
	}

	if *format != "tab" && *format != "json" && *format != "text" {
		return fmt.Errorf("unknown --format %q (want tab|vcf|json|text)", *format)
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: annotate [--format tab|vcf|json|text] <vcf|locus...>  (locus = chrom:pos:ref:alt)")
	}
	loci, err := readLoci(rest)
	if err != nil {
		return err
	}

	// The engine annotates all sources (keeping the cache complete), so every
	// source must be present; the selection only filters what's shown.
	if err := requireSources(cfg, snap, snap.Annotations); err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	closeStore := func() {
		if st != nil {
			st.Close()
		}
	}
	cleanup := closeStore
	defer func() { cleanup() }()

	// Run any tool sources (VEP/ANNOVAR) referenced by the selected annotations over
	// the requested loci, projecting each tool's cached output as a source the engine
	// overlays. Selection-aware so we don't launch an expensive tool unless asked.
	var toolSrcs []config.Source
	if tools := referencedTools(snap, selected); len(tools) > 0 {
		if st != nil {
			if err := st.Init(ctx); err != nil {
				return err
			}
		}
		workdir, err := os.MkdirTemp("", "cgvant-loci-tools-")
		if err != nil {
			return err
		}
		cleanup = func() { closeStore(); os.RemoveAll(workdir) }
		toolSrcs, err = annotatepkg.RunToolsForLoci(ctx, cfg, tools, st, loci, workdir, snap.Reference, snap.Assembly)
		if err != nil {
			return err
		}
	}

	eng, err := newEngineOverStore(ctx, cfg, snap, st, toolSrcs)
	if err != nil {
		return err
	}
	res, err := eng.Annotate(ctx, loci)
	if err != nil {
		return err
	}

	w := io.Writer(os.Stdout)
	if *out != "" && *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return formatResults(w, *format, loci, selected, res)
}

// withSelectedTools returns the snapshot's data/builtin sources plus only the tool
// sources referenced by the selected annotations (so unused tools aren't run).
func withSelectedTools(snap *config.Snapshot, selected []config.Annotation) []config.Source {
	var out []config.Source
	for _, s := range snap.Sources {
		if !s.IsTool() {
			out = append(out, s)
		}
	}
	return append(out, referencedTools(snap, selected)...)
}

// readLoci parses loci from CLI args: a single existing file is read as a VCF,
// otherwise each arg is a chrom:pos:ref:alt locus.
func readLoci(rest []string) ([]model.Locus, error) {
	if len(rest) == 1 && fileExists(rest[0]) {
		return vcf.ReadFile(rest[0])
	}
	var loci []model.Locus
	for _, a := range rest {
		l, err := parseLocus(a)
		if err != nil {
			return nil, err
		}
		loci = append(loci, l)
	}
	return loci, nil
}

// annotateInputVCF returns a VCF path to stream through the pipeline: the input VCF
// as-is (samples preserved) when a single VCF file is given, else a temp sites-only
// VCF synthesized from the locus args. The cleanup removes any temp file.
func annotateInputVCF(rest []string) (string, func(), error) {
	noop := func() {}
	if len(rest) == 0 {
		return "", noop, fmt.Errorf("usage: annotate --format vcf <vcf|locus...>")
	}
	if len(rest) == 1 && fileExists(rest[0]) {
		return rest[0], noop, nil
	}
	loci, err := readLoci(rest)
	if err != nil {
		return "", noop, err
	}
	tmp, err := os.CreateTemp("", "cgvant-in-*.vcf")
	if err != nil {
		return "", noop, err
	}
	tmp.Close()
	if err := vcf.WriteLoci(tmp.Name(), loci); err != nil {
		os.Remove(tmp.Name())
		return "", noop, err
	}
	return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
}

// formatResults renders engine results in the chosen format: tab (default; a
// #-commented header + chrom/pos/ref/alt then a column per selected annotation),
// json (per-variant objects), or text (a human report).
func formatResults(w io.Writer, format string, loci []model.Locus, selected []config.Annotation, res engine.AnnotateResult) error {
	names := make([]string, len(selected))
	for i, a := range selected {
		names[i] = a.Name
	}
	valOf := func(l model.Locus, name string) (model.AnnRow, bool) {
		for _, r := range res.ByLocus[l.Key()] {
			if r.Key == name {
				return r, true
			}
		}
		return model.AnnRow{}, false
	}
	switch format {
	case "tab":
		bw := bufio.NewWriter(w)
		fmt.Fprintln(bw, "#"+strings.Join(append([]string{"chrom", "pos", "ref", "alt"}, names...), "\t"))
		for _, l := range loci {
			cols := []string{l.Chrom, strconv.FormatInt(l.Pos, 10), l.Ref, l.Alt}
			for _, n := range names {
				r, ok := valOf(l, n)
				if ok {
					cols = append(cols, r.Value.String())
				} else {
					cols = append(cols, "")
				}
			}
			fmt.Fprintln(bw, strings.Join(cols, "\t"))
		}
		return bw.Flush()
	case "json":
		type variant struct {
			Chrom       string         `json:"chrom"`
			Pos         int64          `json:"pos"`
			Ref         string         `json:"ref"`
			Alt         string         `json:"alt"`
			Annotations map[string]any `json:"annotations"`
		}
		out := make([]variant, 0, len(loci))
		for _, l := range loci {
			v := variant{Chrom: l.Chrom, Pos: l.Pos, Ref: l.Ref, Alt: l.Alt, Annotations: map[string]any{}}
			for _, n := range names {
				if r, ok := valOf(l, n); ok {
					if r.Value.IsNum {
						v.Annotations[n] = r.Value.Num
					} else {
						v.Annotations[n] = r.Value.Str
					}
				}
			}
			out = append(out, v)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "text":
		bw := bufio.NewWriter(w)
		for _, l := range loci {
			fmt.Fprintf(bw, "%s\n", l.Key())
			for _, n := range names {
				if r, ok := valOf(l, n); ok {
					fmt.Fprintf(bw, "  %-24s %-24s = %s\n", r.DataSource, r.Key, r.Value.String())
				}
			}
		}
		fmt.Fprintf(bw, "\nsnapshot %s  (%d newly annotated)\n", res.Version, res.Novel)
		return bw.Flush()
	}
	return fmt.Errorf("unknown format %q", format)
}

// stringList is a repeatable, comma-splittable flag.Value.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// referencedTools returns the snapshot's tool sources whose output is read by a
// selected annotation (so unused tools — e.g. VEP — aren't run).
func referencedTools(snap *config.Snapshot, anns []config.Annotation) []config.Source {
	need := map[string]bool{}
	for _, a := range anns {
		need[a.Source] = true
	}
	var out []config.Source
	for _, s := range snap.ToolSources() {
		if need[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// requireSources errors if any file-based source referenced by anns isn't fully
// present on disk (tool sources are generated at run time, so skipped).
func requireSources(cfg *config.Config, snap *config.Snapshot, anns []config.Annotation) error {
	seen := map[string]bool{}
	var problems []string
	for _, a := range anns {
		src := snap.SourceByName(a.Source)
		if src == nil || src.IsTool() || seen[src.ID()] {
			continue
		}
		seen[src.ID()] = true
		if m := fetch.Missing(cfg, *src); len(m) > 0 {
			problems = append(problems, fmt.Sprintf("%s (missing %s)", src.ID(), m[0]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("sources not downloaded — run `cgvant download`:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func cmdVersions(eng *engine.Engine) error {
	fmt.Printf("snapshot: %s\n", eng.Version())
	return nil
}

func parseLocus(s string) (model.Locus, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return model.Locus{}, fmt.Errorf("bad locus %q: want chrom:pos:ref:alt", s)
	}
	pos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return model.Locus{}, fmt.Errorf("bad locus %q: pos %w", s, err)
	}
	return model.Locus{
		Chrom: parts[0], Pos: pos,
		Ref: strings.ToUpper(parts[2]), Alt: strings.ToUpper(parts[3]),
	}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
