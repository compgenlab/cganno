package annotate

import (
	"fmt"
	"strings"

	"github.com/compgenlab/hts/bed"
	"github.com/compgenlab/hts/gtf"
	"github.com/compgenlab/hts/vcf"

	"github.com/compgenlab/cgvant/internal/config"
)

// gtfField selects one GTF-derived value (key, one of config.GTFFields) and the
// INFO tag to write it under.
type gtfField struct {
	name string // output INFO key (the annotation's Name)
	key  string // GTF field, upper-cased (GENE/GENEID/STRAND/BIOTYPE/REGION/CODING/NONCODING)
}

// gtfAnnotator overlays gene annotations from a GTF onto VCF records, emitting
// only the selected fields under their configured names. It loads the GTF gene
// model once (in-memory, via the hts gtf package) and queries it per variant.
// One annotator serves every selected field of a single gtf source, so the GTF
// is parsed once regardless of how many fields are requested. Implements
// htsann.Annotator.
type gtfAnnotator struct {
	src      *gtf.AnnotationSource
	fields   []gtfField
	conv     *vcf.ContigConverter // cross-scheme contig matching (always on, like the other annotators)
	filename string
}

// newGTFAnnotator loads the GTF and returns an annotator for the given fields.
func newGTFAnnotator(path string, requiredTags []string, fields []gtfField) (*gtfAnnotator, error) {
	src, err := gtf.NewAnnotationSource(path, requiredTags)
	if err != nil {
		return nil, err
	}
	return &gtfAnnotator{
		src:      src,
		fields:   fields,
		conv:     vcf.NewContigConverter(src.RefNames()),
		filename: path,
	}, nil
}

func (a *gtfAnnotator) SetupHeader(h *vcf.VcfHeader) error {
	for _, f := range a.fields {
		h.AddInfo(&vcf.AnnotationDef{
			IsInfo: true, ID: f.name, Number: ".", Type: "String",
			Description: gtfFieldDesc(f.key), Source: a.filename,
		})
	}
	return nil
}

func (a *gtfAnnotator) Annotate(rec *vcf.VcfRecord) error {
	chrom, ok := a.conv.Resolve(rec.Chrom)
	if !ok {
		return nil // no contig in the GTF shares this record's identity
	}
	// Query position: the variant base for an SNV, the next base for a deletion
	// (mirrors the hts annotate base coordinate resolution).
	pos := rec.Pos
	if len(rec.Ref) != 1 {
		pos = rec.Pos + 1
	}
	pos0 := pos - 1

	genes := a.src.FindGenes(chrom, pos0, pos0+1)
	if len(genes) == 0 {
		return nil
	}
	for _, f := range a.fields {
		if v := gtfValue(a.src, chrom, pos0, genes, f.key); v != "" {
			rec.AddInfo(f.name, v)
		}
	}
	return nil
}

// Close is a no-op: the gene model lives in memory.
func (a *gtfAnnotator) Close() error { return nil }

// gtfValue computes one field's comma-joined value across the overlapping genes.
// GENE/GENEID/STRAND/REGION yield one value per gene (parallel order); BIOTYPE
// fills "." for genes lacking one; CODING/NONCODING are the gene names filtered
// by coding status (empty → the field is omitted for this record). Variants are
// unstranded, so REGION is always a sense code.
func gtfValue(src *gtf.AnnotationSource, ref string, pos0 int, genes []*gtf.Gene, key string) string {
	var vals []string
	for _, g := range genes {
		switch key {
		case "GENE":
			vals = append(vals, g.GeneName)
		case "GENEID":
			vals = append(vals, g.GeneID)
		case "STRAND":
			vals = append(vals, string(g.Strand))
		case "BIOTYPE":
			if g.BioType != "" {
				vals = append(vals, g.BioType)
			} else {
				vals = append(vals, ".")
			}
		case "REGION":
			vals = append(vals, src.FindGenicRegionForPos(ref, pos0, bed.StrandNone, g.GeneID).Code)
		case "CODING":
			if g.IsCoding() {
				vals = append(vals, g.GeneName)
			}
		case "NONCODING":
			if !g.IsCoding() {
				vals = append(vals, g.GeneName)
			}
		}
	}
	return strings.Join(vals, ",")
}

func gtfFieldDesc(key string) string {
	switch key {
	case "GENE":
		return "Gene name"
	case "GENEID":
		return "Gene ID"
	case "STRAND":
		return "Gene strand"
	case "BIOTYPE":
		return "Gene biotype"
	case "REGION":
		return "Genic region"
	case "CODING":
		return "Coding gene name"
	case "NONCODING":
		return "Non-coding gene name"
	}
	return "GTF annotation"
}

// buildGTF constructs one grouped annotator for a gtf source over its selected
// annotations. A gtf source must resolve to a single file.
func buildGTF(src config.Source, annos []config.Annotation, files []config.SourceFile) (*gtfAnnotator, error) {
	if len(files) != 1 {
		return nil, fmt.Errorf("gtf source %q must be a single file", src.ID())
	}
	fields := make([]gtfField, len(annos))
	for i, a := range annos {
		fields[i] = gtfField{name: a.Name, key: strings.ToUpper(a.FieldName())}
	}
	return newGTFAnnotator(files[0].Path, src.GTFTags, fields)
}
