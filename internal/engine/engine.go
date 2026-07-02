// Package engine wires the store and annotator together to annotate loci. The
// annotation cache (store) memoizes the annotator: a locus is computed once,
// then served from the cache (the "DB-as-cache" pattern).
package engine

import (
	"context"

	"github.com/compgenlab/cgvant/internal/annotator"
	"github.com/compgenlab/cgvant/internal/model"
	"github.com/compgenlab/cgvant/internal/store"
)

// Engine is the core service.
type Engine struct {
	store    store.Store
	ann      annotator.Annotator
	snapshot string // active snapshot name (the version stamped on results)
	assembly string // active assembly (scopes cached annotations)
	sources  []model.DataSource
}

// New builds an Engine over a store, an annotator, the active snapshot name, that
// snapshot's assembly (which scopes the cache), and its pinned sources.
func New(s store.Store, a annotator.Annotator, snapshot, assembly string, sources []model.DataSource) *Engine {
	return &Engine{store: s, ann: a, snapshot: snapshot, assembly: assembly, sources: sources}
}

// Init prepares the schema and registers the pinned sources. A nil store (cache
// disabled) is a no-op.
func (e *Engine) Init(ctx context.Context) error {
	if e.store == nil {
		return nil
	}
	if err := e.store.Init(ctx); err != nil {
		return err
	}
	return e.store.RegisterSources(ctx, e.sources)
}

// Version returns the active snapshot name (the version stamped on results).
func (e *Engine) Version() string { return e.snapshot }

// AnnotateResult groups annotation rows by locus key, with a version stamp.
type AnnotateResult struct {
	ByLocus map[string][]model.AnnRow
	Version string
	Novel   int // loci that were cache misses and freshly computed
}

// Annotate returns annotations for the given loci, computing (and caching) any
// that are not already cached.
func (e *Engine) Annotate(ctx context.Context, loci []model.Locus) (AnnotateResult, error) {
	byLocus, novel, err := e.ensureAnnotated(ctx, loci)
	if err != nil {
		return AnnotateResult{}, err
	}
	return AnnotateResult{ByLocus: byLocus, Version: e.Version(), Novel: novel}, nil
}

// ensureAnnotated returns annotations for all loci, running the annotator only
// on cache misses and persisting the results. With no store (cache disabled) it
// computes every locus fresh and persists nothing.
func (e *Engine) ensureAnnotated(ctx context.Context, loci []model.Locus) (map[string][]model.AnnRow, int, error) {
	if e.store == nil {
		rows, err := e.ann.Annotate(ctx, loci)
		if err != nil {
			return nil, 0, err
		}
		out := map[string][]model.AnnRow{}
		for _, r := range rows {
			k := r.Locus.Key()
			out[k] = append(out[k], r)
		}
		return out, len(loci), nil
	}
	cached, err := e.store.Annotations(ctx, e.assembly, loci)
	if err != nil {
		return nil, 0, err
	}
	var missing []model.Locus
	for _, l := range loci {
		if _, ok := cached[l.Key()]; !ok {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		rows, err := e.ann.Annotate(ctx, missing)
		if err != nil {
			return nil, 0, err
		}
		if err := e.store.PutAnnotations(ctx, e.assembly, rows); err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			k := r.Locus.Key()
			cached[k] = append(cached[k], r)
		}
	}
	return cached, len(missing), nil
}
