# REST API

> **Status: planned — not yet implemented.** This page is a placeholder for the annotation
> service API. cgvant is a CLI today; the REST endpoint below describes the intended shape so
> the design is on record.

## Planned model

A cgvant annotation server will expose the same annotation engine over HTTP, backed by the
same snapshot config and cache. Annotation can be expensive (external tools, large
lookups), so the endpoint is designed to be **asynchronous**:

- A request submits a locus or a set of variants and returns a **job ID** immediately,
  rather than blocking until every annotation is computed.
- The server annotates asynchronously on a **fixed worker pool** (bounded concurrency,
  parallel across loci).
- The caller **polls** by job ID and fetches the results when the job is done.

Results would honor the same [output formats](io-formats.md) as the CLI (`tab` / `json` /
`vcf`) and the same [annotation selection](io-formats.md#annotate-output)
(`--all` / a named set / the snapshot defaults).

## Sketch (subject to change)

```
POST /v1/annotate            # body: { snapshot, variants:[…], annotations:[…]|"all", format }
  → 202 { job_id }

GET  /v1/jobs/{job_id}       # → { status: queued|running|done|error, … }
GET  /v1/jobs/{job_id}/results?format=tab|json|vcf   # → the annotated results
```

Nothing here is stable yet — endpoints, payloads, and auth are undecided. Track progress in
the project roadmap.

← Back to the **[docs index](README.md)**.
