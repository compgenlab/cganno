package annotate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/hts/htsio/tabix"

	"github.com/compgenlab/cgvant/internal/config"
	"github.com/compgenlab/cgvant/internal/model"
	"github.com/compgenlab/cgvant/internal/store/sqlite"
)

// TestAnnotateVCFWithTool exercises the full external-tool path with a fake tool
// (a local-runner step that "produces" a prebuilt indexed tab file). It proves
// the tool's output is consumed as a source and annotates the VCF.
func TestAnnotateVCFWithTool(t *testing.T) {
	dir := t.TempDir()

	// The indexed tab file the fake tool will "produce": chrom pos ref alt score.
	pre := filepath.Join(dir, "pre.tab.gz")
	w := tabix.NewWriter(pre, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, l := range []string{"chr1\t100\tA\tG\t0.42", "chr1\t100\tA\tT\t0.99"} {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rel := &config.Snapshot{
		Name: "r",
		Sources: []config.Source{{
			Type: "tool", Name: "myvep", Version: "1", Format: "tab", RefCol: 3, AltCol: 4,
			Steps: []config.Step{{
				Name: "produce",
				Run:  "cp " + pre + " {output}; cp " + pre + ".tbi {output}.tbi",
			}},
		}},
		Annotations: []config.Annotation{{Name: "VEP_SCORE", Source: "myvep", Field: "5", Type: "numeric"}},
	}

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(
		"##fileformat=VCFv4.2\n"+
			"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
			"chr1\t100\t.\tA\tG\t.\t.\t.\n"+
			"chr1\t100\t.\tA\tT\t.\t.\t.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.vcf")
	if err := AnnotateVCFSnapshot(context.Background(), &config.Config{}, rel, in, out, nil, 1, false); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var g, tt string
	for _, ln := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(ln, "chr1\t100\t.\tA\tG"):
			g = ln
		case strings.HasPrefix(ln, "chr1\t100\t.\tA\tT"):
			tt = ln
		}
	}
	if !strings.Contains(g, "VEP_SCORE=0.42") {
		t.Errorf("A>G missing VEP_SCORE=0.42: %s", g)
	}
	if !strings.Contains(tt, "VEP_SCORE=0.99") {
		t.Errorf("A>T missing VEP_SCORE=0.99: %s", tt)
	}
}

// TestRunToolsForLoci exercises the cache/locus-path entry point: it materializes
// loci as a VCF, runs the (fake) tool through the per-locus cache, and returns the
// tool's indexed output projected as a Source — what the engine then overlays.
func TestRunToolsForLoci(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	pre := filepath.Join(dir, "pre.tab.gz")
	w := tabix.NewWriter(pre, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, l := range []string{"chr1\t100\tA\tG\t0.42", "chr1\t100\tA\tT\t0.99"} {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tools := []config.Source{{
		Type: "tool", Name: "myvep", Version: "1", Format: "tab", RefCol: 3, AltCol: 4,
		Steps: []config.Step{{Name: "produce",
			Run: "cp " + pre + " {output}; cp " + pre + ".tbi {output}.tbi"}},
	}}
	loci := []model.Locus{
		{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"},
		{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"},
	}

	st, err := sqlite.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}

	workdir := filepath.Join(dir, "wd")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}

	srcs, err := RunToolsForLoci(ctx, &config.Config{}, tools, st, loci, workdir, "", "GRCh38")
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Name != "myvep" || srcs[0].LocalPath == "" {
		t.Fatalf("got sources %+v, want one myvep with a LocalPath", srcs)
	}
	out := srcs[0].LocalPath
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("tool output %s missing: %v", out, err)
	}
	_, tbi := os.Stat(out + ".tbi")
	_, csi := os.Stat(out + ".csi")
	if tbi != nil && csi != nil {
		t.Errorf("tool output not tabix-indexed: %s", out)
	}
}

// TestAnnotateVCFToolCached proves the tool-output cache: a tool runs only on
// loci it hasn't seen. The fake tool logs each invocation and copies a prebuilt
// output covering both alleles; we assert run counts across three annotations.
func TestAnnotateVCFToolCached(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	pre := filepath.Join(dir, "pre.tab.gz")
	w := tabix.NewWriter(pre, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, l := range []string{"chr1\t100\tA\tG\t0.42", "chr1\t100\tA\tT\t0.99"} {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	counter := filepath.Join(dir, "runs.log")
	rel := &config.Snapshot{
		Name: "r",
		Sources: []config.Source{{
			Type: "tool", Name: "myvep", Version: "1", Format: "tab", RefCol: 3, AltCol: 4,
			Steps: []config.Step{{
				Name: "produce",
				Run:  "echo run >> " + counter + "; cp " + pre + " {output}; cp " + pre + ".tbi {output}.tbi",
			}},
		}},
		Annotations: []config.Annotation{{Name: "VEP_SCORE", Source: "myvep", Field: "5", Type: "numeric"}},
	}

	st, err := sqlite.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}

	runs := func() int {
		b, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		return strings.Count(string(b), "run\n")
	}

	const gRec = "chr1\t100\t.\tA\tG\t.\t.\t.\n"
	const tRec = "chr1\t100\t.\tA\tT\t.\t.\t.\n"

	annotate := func(name, body string, want map[string]string) {
		t.Helper()
		in := filepath.Join(dir, name+".in.vcf")
		if err := os.WriteFile(in, []byte(
			"##fileformat=VCFv4.2\n#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, name+".out.vcf")
		if err := AnnotateVCFSnapshot(ctx, &config.Config{}, rel, in, out, st, 1, false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for prefix, score := range want {
			var line string
			for _, ln := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(ln, prefix) {
					line = ln
				}
			}
			if !strings.Contains(line, "VEP_SCORE="+score) {
				t.Errorf("%s: %q missing VEP_SCORE=%s: %q", name, prefix, score, line)
			}
		}
	}

	// Run 1: only A>G is novel → the tool runs once.
	annotate("r1", gRec, map[string]string{"chr1\t100\t.\tA\tG": "0.42"})
	if got := runs(); got != 1 {
		t.Fatalf("run1: tool ran %d times, want 1", got)
	}

	// Run 2: A>G is cached, A>T is novel → the tool runs once more (for A>T).
	annotate("r2", gRec+tRec, map[string]string{
		"chr1\t100\t.\tA\tG": "0.42",
		"chr1\t100\t.\tA\tT": "0.99",
	})
	if got := runs(); got != 2 {
		t.Fatalf("run2: tool ran %d times total, want 2", got)
	}

	// Run 3: both alleles cached → the tool does NOT run again.
	annotate("r3", gRec+tRec, map[string]string{
		"chr1\t100\t.\tA\tG": "0.42",
		"chr1\t100\t.\tA\tT": "0.99",
	})
	if got := runs(); got != 2 {
		t.Fatalf("run3: tool ran %d times total, want 2 (cache hit)", got)
	}
}

func writeIndexedVCF(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "clinvar.vcf.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, line := range []string{
		"chr1\t100\t.\tA\tG\t.\t.\tCLNSIG=Pathogenic",
		"chr1\t250\t.\tC\tT\t.\t.\tCLNSIG=Benign",
	} {
		if err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnnotateVCFSourcesAndTools(t *testing.T) {
	dir := t.TempDir()
	srcPath := writeIndexedVCF(t, dir)

	rel := &config.Snapshot{
		Name:    "2026-06",
		Sources: []config.Source{{Name: "clinvar", Version: "2026-01", Format: "vcf", LocalPath: srcPath}},
		Annotations: []config.Annotation{
			{Name: "clinvar_sig", Source: "clinvar", Field: "CLNSIG", Type: "categorical"},
			{Source: "auto_id"},
			{Source: "indel"},
			{Source: "tags", Args: "PANEL:v1"},
		},
	}

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(
		"##fileformat=VCFv4.2\n"+
			"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
			"chr1\t100\t.\tA\tG\t.\t.\t.\n"+ // in source: Pathogenic
			"chr1\t150\t.\tAT\tA\t.\t.\t.\n", // deletion, not in source
	), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPipeline(rel, func(s config.Source) []config.SourceFile {
		return []config.SourceFile{{Path: s.LocalPath}}
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.vcf")
	if err := AnnotateVCF(p, in, out); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rec1, rec2 string
	for _, ln := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(ln, "chr1\t100"):
			rec1 = ln
		case strings.HasPrefix(ln, "chr1\t150"):
			rec2 = ln
		}
	}
	if rec1 == "" || rec2 == "" {
		t.Fatalf("missing output records:\n%s", data)
	}
	// record 1: source annotation + tag + auto-id
	for _, want := range []string{"clinvar_sig=Pathogenic", "PANEL=v1", "chr1_100_A_G"} {
		if !strings.Contains(rec1, want) {
			t.Errorf("rec1 missing %q: %s", want, rec1)
		}
	}
	// record 2: deletion flagged by --indel; no source match
	if !strings.Contains(rec2, "CG_DELETE") {
		t.Errorf("rec2 missing CG_DELETE: %s", rec2)
	}
	if strings.Contains(rec2, "clinvar_sig") {
		t.Errorf("rec2 should have no clinvar_sig: %s", rec2)
	}
}
