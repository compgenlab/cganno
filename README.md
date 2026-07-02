# cgtag

**Fast, no-fuss variant annotation from the command line.**

`cgtag` annotates VCF files (and bare loci) against a *versioned* bundle of reference
sources — gene model, ClinVar significance, gnomAD allele frequencies, CADD/REVEL
scores, your own BED/VCF/TSV tracks, or external tools like VEP — and caches the
results so repeat work is instant. It's a single static Go binary: no Perl, no
cache-install dance, no database server to stand up.

> The name: `cg` for [compgenlab](https://github.com/compgenlab), and the tool writes
> its self-contained annotations as `CG_*` INFO tags.

## Highlights

- **One binary.** Pure Go (`CGO_ENABLED=0`), cross-compiles to Linux/macOS,
  amd64/arm64. No interpreter or system libraries to manage.
- **Versioned bundles.** A *snapshot* pins a set of `name:version` sources + their
  annotation schema, so output is reproducible and the config is git-friendly.
- **Memoizing cache.** Each locus is annotated once and served from SQLite thereafter.
- **Config-driven.** Sources and the fields they expose are declared in TOML — no code
  to add a new annotation. Sources can be static files, built-in annotators, or
  external tools (VEP/ANNOVAR).

## Install

Requires Go 1.25+.

```sh
go install github.com/compgenlab/cgtag/cmd/cgtag@latest
```

## Quick start

```sh
export CGTAG_HOME=~/cgtag                 # base dir for config, data, cache, and the DB
cgtag init                                # scaffold config.toml + a starter snapshot

# add a source, reference it from the snapshot, then fetch the data:
cgtag source add --name gnomad --version 4.1 --url https://… --format vcf --snapshot 2026-07
cgtag annotation add --source gnomad:4.1 --name gnomad_af --field AF --type numeric
cgtag download -j 4

# annotate (default output is TSV; --format vcf|json|text, -o writes to a file):
cgtag annotate chr1:115256529:T:C
cgtag annotate --all --format vcf -o out.vcf in.vcf
```

See the **[Quick start guide](docs/quickstart.md)** for a fuller walkthrough.

## Documentation

Full documentation lives in **[`docs/`](docs/README.md)**:

- **[Overview](docs/overview.md)** — the config model, snapshots, and the cache.
- **[Source types](docs/sources.md)** — builtin / vcf / tabix / bed / gtf / tool.
- **[Source & tool lifecycle](docs/lifecycle.md)** — download, build recipes, tool
  image-acquire + setup, and per-run pre/post-processing steps.
- **[Input & output formats](docs/io-formats.md)** — how variants go in and how results
  come out.
- **[Registry](docs/registry.md)** — pulling pre-made sources, and submitting your own.
- **[REST API](docs/rest-api.md)** — planned (placeholder).

## Status

Early but working: an interactive CLI on a SQLite backend. A Postgres backend and an
asynchronous REST annotate endpoint are planned. Cohort-style filtering ("which loci are
pathogenic *and* rare") is intentionally out of scope — cgtag produces the annotations; a
consumer filters them.

## Development

```sh
make build      # bin/cgtag
make test       # go test -race ./...
make vet
make cross      # release tarballs (linux,darwin × amd64,arm64)
```

Supported platforms: Linux and macOS on `amd64` and `arm64`. Because cgtag is pure Go,
`make cross` static-cross-compiles all four from any host with no C toolchain.

## License

Not yet chosen. (TODO: add a `LICENSE` file before distributing.)
