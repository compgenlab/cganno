package annotate

import (
	"fmt"
	"io"
	"os"

	"github.com/compgenlab/cghts/vcf"
)

// MergeVCFParts combines same-order VCF parts positionally into outPath. Every part
// must hold the identical sites in the identical order — as produced by annotating
// one input VCF with a different source per part (the `annotate -t` fan-out, or a
// hand-run per-source pass). At each position it reads one record from every part,
// verifies they are the same site, and unions onto the base part (part 0) the
// INFO/FORMAT fields each other part *added* — i.e. the keys a part has beyond the
// set common to all parts (which is the shared input's own fields). A site mismatch
// or a differing record count is a hard error. outPath "" or "-" writes stdout;
// a ".gz"/".bgz" suffix writes BGZF.
//
// This is a same-order column combine, NOT a bcftools-style site merge: it never
// reconciles differing variant sets. An annotation that overwrites an input INFO
// key of the same name is not represented (that key is common to all parts, so it
// is treated as input and left as the base's value) — use the sequential path
// (`-t 1`) if you need overwrite semantics.
func MergeVCFParts(partPaths []string, outPath string) error {
	return mergeParts(partPaths, outPath, nil, nil)
}

// mergeAnnotatedParts is the internal `annotate -t` merge: it takes the original
// input's INFO/FORMAT keys as the reference, so a key a part *added* is exactly
// what that part has beyond the input. Unlike MergeVCFParts's intersection, this is
// correct when several parts share an added key — e.g. the per-chromosome files of
// one multi-file source, each contributing the same INFO tag on its own records.
func mergeAnnotatedParts(inPath string, partPaths []string, outPath string) error {
	r, err := vcf.NewVcfFile(inPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", inPath, err)
	}
	h, err := r.Header()
	if err != nil {
		r.Close()
		return fmt.Errorf("read header %s: %w", inPath, err)
	}
	refInfo, refFormat := idSet(h.InfoIDs()), idSet(h.FormatIDs())
	r.Close()
	return mergeParts(partPaths, outPath, refInfo, refFormat)
}

// mergeParts is the shared lockstep combine. refInfo/refFormat name the keys that
// are NOT part-contributed (the input's own fields); a nil ref falls back to the
// intersection of the parts' headers (the standalone `vcf-merge` heuristic).
func mergeParts(partPaths []string, outPath string, refInfo, refFormat map[string]bool) error {
	n := len(partPaths)
	if n == 0 {
		return fmt.Errorf("vcf-merge: no input parts")
	}

	readers := make([]*vcf.VcfReader, n)
	headers := make([]*vcf.VcfHeader, n)
	defer func() {
		for _, r := range readers {
			if r != nil {
				r.Close()
			}
		}
	}()
	for i, p := range partPaths {
		r, err := vcf.NewVcfFile(p)
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		readers[i] = r
		h, err := r.Header()
		if err != nil {
			return fmt.Errorf("read header %s: %w", p, err)
		}
		headers[i] = h
	}

	// With no explicit reference, the fields common to ALL parts are the shared
	// input's own INFO/FORMAT (each source adds only its own distinct keys), so
	// whatever a part has beyond that intersection is what it contributed.
	if refInfo == nil {
		refInfo = intersectIDs(headers, (*vcf.VcfHeader).InfoIDs)
	}
	if refFormat == nil {
		refFormat = intersectIDs(headers, (*vcf.VcfHeader).FormatIDs)
	}
	commonInfo, commonFormat := refInfo, refFormat

	addInfo := make([][]string, n)
	addFormat := make([][]string, n)
	for i := 1; i < n; i++ {
		for _, id := range headers[i].InfoIDs() {
			if !commonInfo[id] {
				addInfo[i] = append(addInfo[i], id)
			}
		}
		for _, id := range headers[i].FormatIDs() {
			if !commonFormat[id] {
				addFormat[i] = append(addFormat[i], id)
			}
		}
	}

	// Output header = base header, plus each non-base part's added defs in order.
	out := headers[0]
	for i := 1; i < n; i++ {
		for _, id := range addInfo[i] {
			if _, ok := out.InfoDef(id); !ok {
				if d, ok := headers[i].InfoDef(id); ok {
					out.AddInfo(d)
				}
			}
		}
		for _, id := range addFormat[i] {
			if _, ok := out.FormatDef(id); !ok {
				if d, ok := headers[i].FormatDef(id); ok {
					out.AddFormat(d)
				}
			}
		}
	}

	var w *vcf.VcfWriter
	var closeFile func() error
	if outPath == "" || outPath == "-" {
		w = vcf.NewVcfWriter(os.Stdout)
	} else {
		f, err := vcf.OpenVcfWriter(outPath)
		if err != nil {
			return err
		}
		w, closeFile = f, f.Close
	}
	if err := w.WriteHeader(out); err != nil {
		return err
	}

	nSamples := len(headers[0].Samples())
	for {
		recs := make([]*vcf.VcfRecord, n)
		eofCount := 0
		for i, r := range readers {
			rec, err := r.NextRecord()
			if err == io.EOF {
				eofCount++
				continue
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", partPaths[i], err)
			}
			recs[i] = rec
		}
		if eofCount == n {
			break
		}
		if eofCount != 0 {
			return fmt.Errorf("vcf-merge: parts have differing record counts")
		}

		base := recs[0]
		for i := 1; i < n; i++ {
			if err := sameSite(base, recs[i]); err != nil {
				return fmt.Errorf("vcf-merge: %w (part %s)", err, partPaths[i])
			}
			copyInfo(base, recs[i], addInfo[i])
			if nSamples > 0 {
				copyFormat(base, recs[i], addFormat[i], nSamples)
			}
		}
		if err := w.WriteRecord(base); err != nil {
			return err
		}
	}
	if closeFile != nil {
		return closeFile()
	}
	return w.Close()
}

// idSet collects ids into a set.
func idSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

// intersectIDs returns the set of IDs present in every header (via ids).
func intersectIDs(headers []*vcf.VcfHeader, ids func(*vcf.VcfHeader) []string) map[string]bool {
	if len(headers) == 0 {
		return nil
	}
	common := map[string]bool{}
	for _, id := range ids(headers[0]) {
		common[id] = true
	}
	for _, h := range headers[1:] {
		have := map[string]bool{}
		for _, id := range ids(h) {
			have[id] = true
		}
		for id := range common {
			if !have[id] {
				delete(common, id)
			}
		}
	}
	return common
}

// sameSite verifies two records are the same variant (CHROM/POS/REF/ALT).
func sameSite(a, b *vcf.VcfRecord) error {
	if a.Chrom != b.Chrom || a.Pos != b.Pos || a.Ref != b.Ref {
		return fmt.Errorf("site mismatch: %s:%d%s vs %s:%d%s", a.Chrom, a.Pos, a.Ref, b.Chrom, b.Pos, b.Ref)
	}
	aa, ba := a.Alt(), b.Alt()
	if len(aa) != len(ba) {
		return fmt.Errorf("alt mismatch at %s:%d", a.Chrom, a.Pos)
	}
	for i := range aa {
		if aa[i] != ba[i] {
			return fmt.Errorf("alt mismatch at %s:%d", a.Chrom, a.Pos)
		}
	}
	return nil
}

// copyInfo copies the given INFO keys from src onto base (overwriting).
func copyInfo(base, src *vcf.VcfRecord, keys []string) {
	for _, k := range keys {
		v, ok := src.InfoValue(k)
		if !ok {
			continue
		}
		if v.IsEmpty() {
			base.AddInfoFlag(k)
		} else {
			base.AddInfo(k, v.String())
		}
	}
}

// copyFormat copies the given FORMAT keys from src onto base for every sample.
func copyFormat(base, src *vcf.VcfRecord, keys []string, nSamples int) {
	if len(keys) == 0 {
		return
	}
	for s := 0; s < nSamples; s++ {
		attr, err := src.Sample(s)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if v, ok := attr.Get(k); ok {
				_ = base.AddFormat(s, k, v.String())
			}
		}
	}
}
