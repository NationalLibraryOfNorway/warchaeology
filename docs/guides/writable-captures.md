---
title: Writable capture collections
---

`warc serve` accepts streamed CDXJ on writable Badger databases. New HTTP
collections store capture metadata and replay locations. Configure pywb's
`archive_paths` with the corresponding WARC locations and set its access rules.

```sh
warc serve --collections-dir ./collections --address :9999 --path-prefix /archive
curl -X PUT http://localhost:9999/archive/collections/news
curl --data-binary @captures.cdxj http://localhost:9999/archive/collections/news/cdx
curl 'http://localhost:9999/archive/collections/news/cdx?url=https://example.com/'
```

Names contain 1–64 ASCII letters, digits, underscores or hyphens and start with a
letter or digit. Create a collection with `PUT` before uploading its captures.
`--collections-dir` cannot be combined with WARC inputs, zipnum, or single-database
location flags. Collections are isolated Badger directories. The server opens
existing collections at startup and refuses incompatible databases.

| Method and path | Operation |
| --- | --- |
| `GET /collections` | Names and capabilities |
| `PUT /collections/{name}` | Create an index-only collection; existing names are unchanged |
| `GET /collections/{name}/cdx` | Interactive query |
| `POST /collections/{name}/cdx` | Incremental CDXJ upsert |
| `POST /collections/{name}/delete` | Delete the identities in CDXJ |
| `GET /collections/{name}/export` | Consistent, uncapped streaming CDXJ export |
| `GET /collections/{name}/packages` | Stream package membership and preserved manifests as NDJSON |
| `GET /collections/{name}/stats` | Persisted capture count and capabilities; no index scan |
| `GET /collections/{name}/backup` | Consistent Badger backup |
| `GET /health`, `GET /ready` | Process readiness after databases have opened |

Every route honors `--path-prefix`. Single-database mode uses `/cdx`, `/delete`,
`/export`, `/stats`, `/packages`, `/backup`, and `/rejections/{id}` directly. Existing full
native databases retain their record, ID, file, report and warcserver routes.
New single databases default to index-only. Supplying archive inputs, enabling
`--fulltext`, or enabling `--allow-same-origin-replay` creates a full native
database. The stored capability metadata controls subsequent opens.
`--read-only` rejects uploads, deletions, collection creation and report mutations.
Zipnum remains immutable and index-only.

## Upload contract

Send CDXJ, one `key timestamp {JSON}` per line, using a regular or chunked request.
`Content-Encoding: gzip` is supported. Convert legacy CDX11/CDX14 inputs to CDXJ
before uploading. The server computes the stored key from `url`, `method` and
`requestBody`, using the same construction for deletion and POST-aware queries.
The canonicalization profile and its known reference differences are documented
in [capture indexes](capture-indexes.md#compatibility-and-verification).

Required fields are a 14-digit valid UTC timestamp, an absolute `url`, nonempty
`filename`, and nonnegative signed 64-bit `offset` and `length`. Numeric fields
accept decimal strings or JSON integers. `status`, when present, is 0–999.
Canonical URL keys over 60,000 bytes use overflow storage. Upload request and row size limits still apply.
Extension objects, arrays, and numbers retain JSON numeric precision. Typed
fields are stored separately from extension properties. POST/PUT request bodies
must be supplied as the indexer's extracted query string.

Identity is the computed key, timestamp, filename and offset within a collection.
Different locations at the same timestamp remain distinct. Repeated uploads
upsert the same identity. Conflicting values for one identity in a request reject
the request. An upsert supersedes matching native replay locations. Record-ID lookup and
local source mappings come from native WARC indexing. Deletion of a missing
identity succeeds. Uploads update individual capture identities; native WARC
reindexing replaces the complete source generation.

Strict mode is the default: an invalid line rejects the entire request. Add
`?badLines=skip` to publish valid lines and receive accepted/rejected line counts.
Counts describe input lines, so exact duplicate lines can be counted more than
once without producing extra captures. Responses include at most 100 rejection
diagnostics and a `report` URL for the complete streamed NDJSON report. Reports
are available for 24 hours; expired files are removed when another rejection
report is created. Reports are operational artifacts and are not included in
Badger backups. Treat a non-2xx upload response as an unsuccessful or ambiguous
acknowledgment; identical retries are safe.

Defaults are 128 MiB **decoded** request bytes, 1 MiB per line and five minutes.
Configure `--upload-max-bytes`, `--upload-max-line`, and `--upload-timeout`.
Uploads stage in temporary disk-backed databases under the target directory.
Publication writes bounded durable batches and one durable commit marker.
Readers see the complete old or complete new set. Unpublished uploads remain
invisible. Restart replays the cleanup journal, preserving committed records and
removing unpublished versions. Successful acknowledgment means durable publication;
cleanup can finish afterward without changing visibility.

## Queries and caps

Queries return CDXJ by default or NDJSON with `output=json`.
Use `urlkey=` for an already canonical key, or `url=...&method=POST&requestBody=...`
for POST-aware lookup. `fields=` and `fl=` select fields; `original`, `mimetype`
and `statuscode` alias `url`, `mime` and `status`.

`collapse=digest`, `collapse=urlkey`, or `collapse=timestamp:4` collapse adjacent
matching values in result order. `collapseTo=first` is the default;
`collapseTo=last` retains the final member. Filters run before collapse.
Exact-URL reverse/closest queries use ordered seeks. Other orderings retain the
external-sort fallback.

Interactive queries default to independently configured 10,000 scanned captures
and 10,000 returned records. Set `--query-scan-limit` and `--query-result-limit`
to change these ceilings. A client's `limit` can reduce the result ceiling.
Inspect HTTP trailers `X-Cdx-Truncated`, `X-Cdx-Scanned`, `X-Cdx-Returned` and
`X-Cdx-Error`; a capped response is not a complete export. Proxies and clients
must preserve/read trailers. Administrative export bypasses these caps and is
separate from zipnum block pagination. Export and backup report stream failures
through `X-Export-Error` and `X-Backup-Error` trailers. Check them before accepting
an artifact. Long exports hold a publication read lock and can delay writers.

## Backup, restore and compaction

Back up a running server through HTTP:

```sh
curl --fail http://localhost:9999/archive/collections/news/backup -o news.backup
```

Or stop the server and back up its database directory:

```sh
warc index backup ./collections/news news.backup
```

The HTTP endpoint returns a native Badger backup and excludes full text.
The offline command creates a ZIP archive containing `badger.backup` and a
Bleve snapshot under `fulltext/` when that subdirectory exists. It uses Badger's
backup API and Bleve's snapshot-copy API. Both indexes must be closed before
running the command; it refuses databases in use. No extra flags are needed.

The offline command requires a new backup filename and publishes it only after
the backup is complete. Allow temporary disk space for the Bleve snapshot as well
as the backup archive.

Restore either backup format into a new directory:

```sh
warc index restore news.backup ./restored-news
warc serve --index-dir ./restored-news
```

Stop any server using the restore destination. Restore requires a nonexistent
directory, verifies the current format, and performs recovery before returning.
The backup includes capture data, secondary references, source checkpoints,
capability metadata and publication state. Offline backups also restore the
included Bleve index, so text search can resume without rereading archive text.
After restoring an HTTP backup, use `warc serve --fulltext` to rebuild text search.
Neither backup format includes the original WARC or WACZ files.
Copying open Badger files is not a supported backup procedure.

After stopping the server, run `warc index compact ./restored-news` to compact
obsolete versions and perform value-log garbage collection. Maintenance holds
an exclusive database handle. Reclamation can require temporary disk space.

## Storage and workload boundaries

The shared codec uses versioned protobuf envelopes, binary algorithm-tagged
digests, compact UUIDs with a lossless URI fallback, nanosecond protobuf timestamps
and 64-bit lengths/offsets. Ordered SURTs remain in capture keys. Record-ID
indexes reference captures instead of duplicating them. Native source cleanup
uses source-specific references. Index storage uses Badger v4.3.1.

A donor is a record that stores the payload referenced by a revisit record.
Deduplication syncs output records before registering them
as provisional donors. A run remains provisional until its WARC writers close
successfully and publish outputs.
Unfinished output runs refuse donor-index reuse after restart; recover by
reprocessing with fresh index/output directories, retaining previous evidence
for inspection. When using external hooks, verify their outputs before retrying
an interrupted run.

Redupe stores rebuildable inventory/planning state separately from its small,
synced publication ledger. Occurrence indexes reference one stored capture.
Fingerprint, exact URI/date, ambiguity, chain and year-scope checks remain.
The scratch directory is removed as a unit on close. Complete diagnostic findings
stream to the report's
`findingsArtifact`; inline lists are limited to 100 entries per year and 100
run-level entries. Preserve the artifact with the audit report.
