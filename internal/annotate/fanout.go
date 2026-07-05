package annotate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"

	"github.com/compgenlab/cgvant/internal/config"
)

// annJob is one fan-out unit: an annotation pass over the whole input, run in
// parallel with the others into its own temp part. Every builtin annotation is
// grouped into a single job (so vardist's look-ahead sees the whole file in order,
// and the sample-derived builtins' FORMAT additions land in one part). A data
// source becomes one job per resolved file — so a per-chromosome (or multi-file)
// source is annotated file-by-file in parallel, each file matching only its own
// records; the merge unions them. `resolve` fixes the file(s) this job sees.
type annJob struct {
	label   string
	anns    []config.Annotation
	resolve func(config.Source) []config.SourceFile
}

// splitAnnotationJobs partitions a snapshot's annotations into fan-out jobs, in
// first-appearance order. Builtins collapse into one job; each data source expands
// to one job per resolved file (per-chrom / Files union / per-alt), so multi-file
// sources parallelize file-by-file.
func splitAnnotationJobs(cfg *config.Config, snap *config.Snapshot) []annJob {
	const builtinKey = "\x00builtins" // sentinel that can't collide with a source name
	order := []string{}
	grouped := map[string][]config.Annotation{}
	for _, a := range snap.Annotations {
		key := a.Source
		if config.IsBuiltin(a.Source) {
			key = builtinKey
		}
		if _, seen := grouped[key]; !seen {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], a)
	}

	var jobs []annJob
	for _, key := range order {
		anns := grouped[key]
		if key == builtinKey {
			jobs = append(jobs, annJob{label: "builtins", anns: anns, resolve: cfg.ResolveSourceFiles})
			continue
		}
		src := snap.SourceByName(key)
		files := cfg.ResolveSourceFiles(*src)
		if len(files) <= 1 {
			jobs = append(jobs, annJob{label: key, anns: anns, resolve: cfg.ResolveSourceFiles})
			continue
		}
		// One job per file. Each job's resolve returns just that file, so
		// AnnotatorFor builds a single-file annotator matching only its records.
		for i, f := range files {
			f := f
			jobs = append(jobs, annJob{
				label:   key + "#" + fileTag(f, i),
				anns:    anns,
				resolve: func(config.Source) []config.SourceFile { return []config.SourceFile{f} },
			})
		}
	}
	return jobs
}

// fileTag labels a per-file job by its chromosome or alt (else its index).
func fileTag(f config.SourceFile, i int) string {
	switch {
	case f.Chrom != "":
		return f.Chrom
	case f.Alt != "":
		return f.Alt
	default:
		return fmt.Sprintf("%d", i)
	}
}

// subSnapshot returns a shallow copy of snap restricted to one job's annotations
// (and the data sources they reference), so BuildPipeline builds only that job's
// annotators. Builtins need no source entry (BuildPipeline dispatches them by name).
func subSnapshot(snap *config.Snapshot, job annJob) *config.Snapshot {
	sub := *snap
	sub.Annotations = job.anns
	seen := map[string]bool{}
	var srcs []config.Source
	for _, a := range job.anns {
		if config.IsBuiltin(a.Source) || seen[a.Source] {
			continue
		}
		seen[a.Source] = true
		if s := snap.SourceByName(a.Source); s != nil {
			srcs = append(srcs, *s)
		}
	}
	sub.Sources = srcs
	return &sub
}

// annotateVCFFanOut annotates inPath → outPath by running each job over the whole
// input concurrently (up to `threads` at once), each writing a temp part VCF, then
// merging the parts positionally. Falls back to a single sequential pass when there
// is nothing to parallelize (≤1 job). Temp parts are removed unless keepTemp.
func annotateVCFFanOut(cfg *config.Config, snap *config.Snapshot, inPath, outPath string, threads int, keepTemp bool) error {
	jobs := splitAnnotationJobs(cfg, snap)
	if len(jobs) <= 1 {
		p, err := BuildPipeline(snap, cfg.ResolveSourceFiles)
		if err != nil {
			return err
		}
		return AnnotateVCF(p, inPath, outPath)
	}

	tmpDir, err := os.MkdirTemp("", "cgvant-annotate-")
	if err != nil {
		return err
	}
	if keepTemp {
		fmt.Fprintf(os.Stderr, "cgvant: keeping annotate temp dir %s\n", tmpDir)
	} else {
		defer os.RemoveAll(tmpDir)
	}

	parts := make([]string, len(jobs))
	g, _ := errgroup.WithContext(context.Background())
	if threads > 0 {
		g.SetLimit(threads)
	}
	for i, job := range jobs {
		i, job := i, job
		part := filepath.Join(tmpDir, fmt.Sprintf("part.%02d.vcf.gz", i))
		parts[i] = part
		g.Go(func() error {
			p, err := BuildPipeline(subSnapshot(snap, job), job.resolve)
			if err != nil {
				return fmt.Errorf("annotate job %q: %w", job.label, err)
			}
			if err := AnnotateVCF(p, inPath, part); err != nil {
				return fmt.Errorf("annotate job %q: %w", job.label, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return mergeAnnotatedParts(inPath, parts, outPath)
}
