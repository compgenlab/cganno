# Input & output formats

There are two "input/output" boundaries in cgtag, and it helps to keep them separate:

1. **The `cgtag annotate` command** — what variants you give it, and how it renders the
   results to you.
2. **A tool source** — how cgtag hands the query variants *into* an external tool
   (`input_format`), and how it reads the tool's produced file back (`format`).

---

## Annotate input

You annotate either **bare loci** or a **VCF file**:

```sh
cgtag annotate chr1:100:A:G chr2:200:C:T      # one or more chrom:pos:ref:alt loci
cgtag annotate variants.vcf                   # every site in a VCF (multi-allelic ALTs are split)
```

Both drive the same per-source lookup — a coordinate query into each source's tabix index,
then ref/alt matching. A single variant is just a one-element list.

Two internal paths back this:

- **Locus / cache path** (`--format tab|json|text`) — results come from the engine and are
  memoized in the cache. This path is **sample-blind**: it reads only chrom/pos/ref/alt, so
  builtins that need sample `FORMAT` fields don't apply here.
- **Pipeline path** (`--format vcf`) — the input streams record-by-record through the hts
  annotation pipeline, preserving the input's header and samples. This is the path where
  **builtins** apply (they can read per-sample fields). For bare loci, a sites-only VCF is
  synthesized first.

## Annotate output

```sh
cgtag annotate [--all | -a name,…] [--format tab|vcf|json|text] [-o FILE] <vcf|locus…>
```

- **`--format`** selects the rendering; the default is **`tab`**.
- **`-o FILE`** writes the output to a file (stdout if omitted) — for *any* format.
- **`--all`** applies every annotation; **`-a name[,name…]`** applies the named ones;
  with neither, the snapshot's `default_annotations` are applied.

| format | shape |
| --- | --- |
| **`tab`** (default) | a `#`-commented header, then one row per variant: `chrom pos ref alt` then a column per selected annotation (missing = empty) |
| **`json`** | an array of per-variant objects `{chrom,pos,ref,alt,annotations:{name:value,…}}` (numeric annotations stay JSON numbers) |
| **`text`** | a human-readable report (the pre-1.0 default) |
| **`vcf`** | a fully-annotated VCF via the streaming pipeline (samples preserved for a VCF input; a sites-only VCF is synthesized for bare loci) |

```sh
cgtag annotate chr1:100:A:G                       # → TSV (default)
cgtag annotate --format json chr1:100:A:G         # → JSON array
cgtag annotate --format vcf -o out.vcf in.vcf     # → annotated VCF (samples preserved)
cgtag annotate -a clinvar_sig -o hits.tsv in.vcf  # → TSV of one annotation, to a file
```

The `tab` output columns are fixed (`chrom pos ref alt` + one per annotation) and are
unrelated to a `tab` *source's* `ref_col`/`alt_col` (which describe how that source file is
*read*).

---

## Tool source I/O

A tool source is defined by how cgtag writes the query variants for it and how it reads the
result back. The variants always reach the tool as a **file** — a single-variant query is
just a one-line/one-record file; on the cache path only the **novel** loci are written.

### Tool input — `input_format`

Controls how `{input}` is written for the tool:

- **`"vcf"`** (default) — cgtag materializes a sites-only VCF (or, on the `--format vcf`
  path, passes the input VCF with its samples). Use this for VCF tools like VEP.
- **a per-variant line template** — cgtag writes one line per variant. For a variant-list
  tool (e.g. ANNOVAR's avinput):

  ```toml
  input_format = "{chrom}\t{pos}\t{ref}\t{alt}"
  # or: input_format = "{chrom}_{pos}:{ref}>{alt}"
  ```

  Placeholders: `{chrom}` `{pos}` `{pos0}` (0-based) `{ref}` `{alt}` `{end}`
  (`pos + len(ref) - 1`). No header line is written. A template must contain at least
  `{chrom}` and `{pos}`.

### Tool output — `format`

The tool's produced file is read back *exactly like a static source of that format*:

- **`"vcf"`** — each annotation's `field` is an INFO id (or `@ID`, or `type = "flag"`).
- **`"tab"`** — chrom in column 1, pos in column 2; ref/alt via `ref_col`/`alt_col`
  (omit → position match); each annotation's `field` is a 1-based column number or a
  header column name. Header lines are `#`-prefixed.

The output must carry coordinates so cgtag can key each record back to a locus — which is
why `vcf` and `tab` are the two supported tool output formats. (A bare `text`/`json`
stream isn't keyable without coordinates; JSON output support may come later when a concrete
tool needs it.)

Next: **[Registry](registry.md)**.
