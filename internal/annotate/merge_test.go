package annotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/hts/vcf"
)

// writePartVCF writes a small uncompressed part VCF (header lines + record lines).
func writePartVCF(t *testing.T, path string, header, records []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("##fileformat=VCFv4.2\n")
	for _, h := range header {
		b.WriteString(h + "\n")
	}
	b.WriteString(strings.Join(records, "\n") + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMergeVCFParts: two same-order parts (disjoint INFO, the base adding a FORMAT
// tag) combine to the union; the sites and samples are preserved.
func TestMergeVCFParts(t *testing.T) {
	dir := t.TempDir()
	// Base part carries the sample + a FORMAT addition (CG_DS); its INFO adds AAA.
	base := filepath.Join(dir, "part.00.vcf")
	writePartVCF(t,
		base,
		[]string{
			"##FORMAT=<ID=GT,Number=1,Type=String,Description=\"gt\">",
			"##FORMAT=<ID=CG_DS,Number=A,Type=Integer,Description=\"dosage\">",
			"##INFO=<ID=AAA,Number=1,Type=Float,Description=\"a\">",
			"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1",
		},
		[]string{
			"chr1\t100\t.\tA\tG\t.\t.\tAAA=0.1\tGT:CG_DS\t0/1:1",
			"chr1\t200\t.\tC\tT\t.\t.\tAAA=0.2\tGT:CG_DS\t1/1:2",
		})
	// Second part: same sites/sample (untouched GT), adds INFO BBB only.
	other := filepath.Join(dir, "part.01.vcf")
	writePartVCF(t,
		other,
		[]string{
			"##FORMAT=<ID=GT,Number=1,Type=String,Description=\"gt\">",
			"##INFO=<ID=BBB,Number=1,Type=Float,Description=\"b\">",
			"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1",
		},
		[]string{
			"chr1\t100\t.\tA\tG\t.\t.\tBBB=9\tGT\t0/1",
			"chr1\t200\t.\tC\tT\t.\t.\tBBB=8\tGT\t1/1",
		})

	out := filepath.Join(dir, "merged.vcf")
	if err := MergeVCFParts([]string{base, other}, out); err != nil {
		t.Fatalf("MergeVCFParts: %v", err)
	}

	r, err := vcf.NewVcfFile(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h, err := r.Header()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.InfoDef("AAA"); !ok {
		t.Error("merged header missing INFO AAA")
	}
	if _, ok := h.InfoDef("BBB"); !ok {
		t.Error("merged header missing INFO BBB (union of the second part)")
	}
	rec, err := r.NextRecord()
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := rec.InfoValue("AAA"); !ok || v.String() != "0.1" {
		t.Errorf("AAA = %q,%v want 0.1", v.String(), ok)
	}
	if v, ok := rec.InfoValue("BBB"); !ok || v.String() != "9" {
		t.Errorf("BBB = %q,%v want 9 (merged from part 2)", v.String(), ok)
	}
	// The base's FORMAT/sample is preserved.
	s, err := rec.Sample(0)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Get("CG_DS"); !ok || v.String() != "1" {
		t.Errorf("sample CG_DS = %q,%v want 1", v.String(), ok)
	}
}

// TestMergeVCFPartsSiteMismatch: parts whose sites diverge are a hard error.
func TestMergeVCFPartsSiteMismatch(t *testing.T) {
	dir := t.TempDir()
	hdr := []string{"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO"}
	a := filepath.Join(dir, "a.vcf")
	b := filepath.Join(dir, "b.vcf")
	writePartVCF(t, a, hdr, []string{"chr1\t100\t.\tA\tG\t.\t.\t.", "chr1\t200\t.\tC\tT\t.\t.\t."})
	writePartVCF(t, b, hdr, []string{"chr1\t100\t.\tA\tG\t.\t.\t.", "chr1\t999\t.\tC\tT\t.\t.\t."})
	err := MergeVCFParts([]string{a, b}, filepath.Join(dir, "out.vcf"))
	if err == nil || !strings.Contains(err.Error(), "site mismatch") {
		t.Fatalf("expected a site-mismatch error, got %v", err)
	}
}
