# Source types

Everything cgtag annotates from is a **source**, identified `name:version` and living at
`sources/<name>/<version>/<name>-<version>.toml`. A source is discriminated by its
`type` field:

| `type` | what it is | data on disk? | runs when? |
| --- | --- | --- | --- |
| `""` (default) | a **data file** in `vcf` / `tab` / `bed` / `gtf` format | yes (downloaded/built) | queried by coordinate |
| `builtin` | a **built-in annotator** computed from the record | no | in the VCF pipeline |
| `tool` | an **external annotator** (VEP/ANNOVAR) run per query | no (generates output) | at `annotate` time |

The litmus test between a data source and a tool source: *does producing the annotation
require seeing the query variants?* If no, it can be precomputed into a static file → a
data source (this is why **CADD is a data source** — precomputed scores for all SNVs). If
yes, it must run on your variants → a **tool** (this is why **VEP is a tool**).

Every source's annotations nest under it as `[[sources.annotations]]`.

---

## Data sources (`vcf` / `tab` / `bed` / `gtf`)

A data source is a tabix-indexed reference file (or a set of files). At annotate time each
locus is looked up by genomic coordinate against the source's tabix index, then matched.

### Common fields

```toml
[[sources]]
name      = "clinvar"
version   = "2026-01"
assembly  = "GRCh38"          # verified against the snapshot's assembly
format    = "vcf"             # vcf | bed | tab | gtf
url       = "https://…/clinvar.vcf.gz"        # canonical location (provenance + registry)
url_index = "https://…/clinvar.vcf.gz.tbi"    # a prebuilt index to download (else guessed/built)
localpath = "/data/clinvar.vcf.gz"            # this machine's actual file (used as-is, never downloaded)
checksum       = "md5:https://…/clinvar.vcf.gz.md5"   # optional "<algo>:<hex-or-url>"
checksum_index = "md5:…"
```

- **`url` vs `localpath`:** `url`/`url_index` are the canonical reference kept for
  provenance and the registry; `localpath`/`localpath_index` are this machine's files —
  when `localpath` is set the file is used exactly and never downloaded. Environment
  variables (`${VAR}`, `$CGTAG_HOME`) are expanded in `localpath`. `registry submit`
  strips `localpath`.
- **`checksum`/`checksum_index`** are optional (`md5`|`sha1`|`sha256`); verified while
  downloading when present. The value may be a URL to a checksum file.

### Multi-file data sources

One source can span several files, all queried and merged:

- **Per-chromosome** — a `{chrom}` template in `url`/`localpath` plus a `chroms` list
  (e.g. gnomAD, one file per chromosome). Each locus is routed to its chromosome's file.
- **Explicit union** — `[[sources.files]]`, an explicit list (each with its own
  `url`/`checksum`) for a small union split by something other than chromosome (e.g.
  coding + indels). Every file is queried; ref/alt picks the match.

### Format specifics

- **`vcf`** — copy an INFO field (`field = "CLNSIG"`), the record ID (`field = "@ID"`), or
  presence (`type = "flag"`). `match = "exact"` (REF+ALT, the default) or `"position"`;
  `unique = true` de-duplicates multiple matches.
- **`tab`** (tabix-indexed TSV — REVEL, CADD, …) — `ref_col`/`alt_col` (1-based) are
  **optional**: set both for allele-aware matching, omit for position-only. The
  annotation's `field` is the value column (a number, or a header column name).
- **`bed`** — interval overlap; `field = "name"` (col 4), `"score"` (col 5), or a column
  number.
- **`gtf`** — a GTF gene model read into memory (no tabix index). Its annotations select
  from a fixed vocabulary of derived fields via `field`: `GENE`, `GENEID`, `STRAND`,
  `BIOTYPE`, `REGION`, `CODING`, `NONCODING`. `gtf_tags = [...]` restricts to features
  carrying every listed tag (e.g. `"basic"` for the GENCODE basic set).

**Chromosome naming is auto-converted:** cgtag builds a converter from the source file's
own reference names, so input/source naming (Ensembl `1` / UCSC `chr1` / NCBI
`NC_000001.11`) is matched automatically.

Data sources that need preprocessing before they can be indexed use a **build recipe** —
see **[lifecycle](lifecycle.md#build-sources)**.

---

## Builtin sources (`type = "builtin"`)

A builtin is a self-contained "built-in tool call" — an annotator computed directly from
the record, with no data file. Builtins live in one `type = "builtin"` source whose nested
annotations name the builtin:

```toml
[[sources]]
name = "builtins"
version = "1"
type = "builtin"
  [[sources.annotations]]
  builtin = "tstv"
  [[sources.annotations]]
  builtin = "tags"
  args = "PANEL:v1"          # parameterized builtins carry their argument in `args`
```

Builtins emit `CG_*` INFO tags. They are **only applied in the VCF pipeline** — i.e.
`cgtag annotate --format vcf` (or `-o out.vcf`). The engine/overlay path used by
`--format tab|json|text` does not run builtins.

| builtin | tag | needs samples? | notes |
| --- | --- | --- | --- |
| `auto_id` | (sets ID) | no | synthesize a variant ID |
| `indel` | | no | flag indels |
| `tstv` | `CG_TSTV` | no | transition/transversion |
| `vardist` | `CG_VARDIST` | no | distance to the nearest variant (streaming look-ahead) |
| `tags` | (constant) | no | a constant tag/flag; `args = "KEY:VALUE"` or `"FLAG"` |
| `dosage` | `CG_DS` | yes (GT) | allele dosage |
| `vaf` | `CG_VAF` | yes (SAC) | variant allele fraction |
| `minor_strand` | `CG_SBPCT` | yes | minor-strand percent |
| `fisher_sb` | `CG_FSB` | yes | Fisher strand bias |
| `copy_logratio` | `CG_CNLR` | yes (AD) | copy-number log-ratio; `args = "SOMATIC:GERMLINE[:st:gt]"` |

*Variant-only* builtins work on any VCF; *sample-derived* ones read per-sample `FORMAT`
(GT/SAC/AD) and need a VCF with samples (so they are meaningful only with a VCF input, not
bare loci).

---

## Tool sources (`type = "tool"`)

A tool source is an external, often containerized annotator (VEP, ANNOVAR, a custom
script) that runs **per query**: cgtag hands it the query variants, it produces an output
file, and that output is consumed exactly like a data source of its `format`.

```toml
[[sources]]
type     = "tool"
name     = "vep"
version  = "113"
image    = "docker://ensemblorg/ensembl-vep:release_113.3"  # docker://|oras://|shub:// ref, or a .sif URL
engine   = "apptainer"                         # or "singularity"
format   = "vcf"                               # how the OUTPUT is read (vcf | tab)
# input_format = "vcf"                          # how variants are handed IN (see io-formats)
output   = "vep.vcf.gz"
runner   = "local"                             # "local" (subprocess) | "batch"
requires = ["python3"]                         # host executables that must be on PATH
assets   = ["expand_vep_vcf.py", "worst.py"]   # co-located helper scripts

  [[sources.setup]]  …    # one-time install (see lifecycle)
  [[sources.steps]]  …    # per-run steps (see lifecycle)

  [[sources.annotations]]
  name  = "vep_consequence"
  field = "Consequence"                        # an INFO id in the VEP output VCF
  type  = "categorical"
```

Because a tool is expensive to run, its output is **cached per locus** (keyed by
`name:version` + the snapshot's assembly), so on later runs it executes only on loci it
hasn't seen. Bump `version` whenever the image, setup, or steps change — that invalidates
the cache.

A tool source runs only when a **selected annotation** references it, so an unused tool
(e.g. VEP when you only asked for gnomAD) never runs.

For how a tool receives variants and how its output is read, see
**[Input & output formats](io-formats.md)**. For the run/setup mechanics, see
**[lifecycle](lifecycle.md)**.
