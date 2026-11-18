---
title: Capture indexing, serving and zipnum
---

`warc index` creates replay capture indexes separately from the processing and
deduplication indexes. It accepts WARC and WARC.gz files and directories, plus
warchaeology's archive mounts. ARC indexing, Kafka,
standalone CDXJ serving and AMF decoding are not implemented.

## Export captures

```sh
warc index capture.warc.gz                         # unsorted CDXJ on stdout
warc index --sort capture.warc.gz > captures.cdxj  # sorted, exact-line deduplication
warc index -r --index-out captures.cdxj.gz warcs/  # sorted gzip output
warc index --post-append --dir-root warcs --index-out captures.cdxj warcs/a.warc.gz
warc index --index-format cdxpb --index-out captures.pb capture.warc.gz
```

Named CDXJ outputs are sorted using 8 MiB memory runs and at most 32 merge inputs.
They are published from a temporary file after successful ingestion; a failed
command leaves an existing target intact. `--index-tmp-dir` selects scratch
storage. Standard output is never closed by the exporter. Sorting removes only
identical serialized lines. Zipnum operations themselves retain duplicates.

The default record selection matches the pinned cdxj-indexer fixtures: responses,
revisits, resources and metadata, excluding `application/warc-fields` resources
and metadata. `--record-type` overrides this selection. Existing filters,
concurrency, error policies, traversal options and input hooks apply. Processing,
output, flush and close errors cause command failure.

Exported filenames default to basenames. `--dir-root` exports relative paths.
Database retrieval uses separate structured source locators containing literal
archive mount paths and member names. Basename
collisions produce an ambiguity error instead of choosing a file. Configure
pywb's archive paths separately when using exported indexes.

`cdxpb` is a stream of unsigned-varint message lengths followed by serialized
`schema.Cdx` protobuf messages. There are no textual prefixes or newline
separators. The schema is in `internal/cdx/proto/cdx.proto` and includes original
URI, canonical key, source, filename, offset, stored length and request metadata.
Use a length-delimited protobuf decoder, such as Go's `protodelim.UnmarshalFrom`.

## Databases

```sh
warc index --index-format badger --badger-dir ./captures warcs/a.warc.gz
warc index reset --index-format badger --badger-dir ./captures
```

Badger **v4** is the supported database backend and is used throughout
warchaeology, including processing and deduplication indexes. Capture databases have a format/canonicalization marker;
unversioned or incompatible stores are rejected with reindexing guidance.
Rebuild existing capture stores in a new directory. Existing
Badger v3 processing/deduplication stores may also need rebuilding; retain the
old directories until the new indexes have been verified.

Capture databases store captures, source identity, metadata and reports.
Identity includes source and offset, so equal URL/timestamp
captures from different files remain distinct. Each successful ingestion publishes
a durable file-generation checkpoint. Interrupted generations stay hidden; a
successful retry removes obsolete captures and record IDs from that source.

## Database serving

```sh
warc serve --badger-dir ./captures
warc serve --badger-dir ./captures --read-only
warc serve --badger-dir ./captures warcs/a.warc.gz
```

The defaults are Badger and `:9999`. Inputs trigger one background ingestion pass;
the server remains running afterward. Ingestion and handlers share one database
handle. Read-only mode rejects inputs and omits report mutation routes.
`--path-prefix /archive` prefixes every route. HTTP requests have bounded timeouts;
SIGINT/SIGTERM cancel ingestion, drain handlers and close resources.

Database routes include `/cdx`, `/file`, `/file/:filename`, `/id`, `/id/:urn`,
`/record/:urn`, `/report`, `/report/:id`, `/debug` and the imported `/warcserver`
CDX/resource routes. CDX supports exact, prefix, host and domain matching, date
bounds, substring/exact/regex filters, `fields`, positive limits, reverse and
closest ordering, CDXJ and newline-delimited JSON. Closest and reverse queries
use external sorting rather than retaining all captures in memory.

Flags use the existing Viper configuration and `WARC_*` environment mapping,
e.g. `WARC_INDEX_FORMAT`, `WARC_BADGER_DIR` and `WARC_ADDRESS`.

## Immutable zipnum clusters

```sh
warc zipnum build captures.cdxj --output-dir cluster
warc zipnum add cluster additions.cdxj --output-dir enlarged
warc zipnum merge cluster-a cluster-b --output-dir merged
warc zipnum repartition cluster --lines 3000 --blocks 32 --output-dir repartitioned
warc serve --index-format zipnum --zipnum-dir cluster
```

Each operation has its own subcommand. Layout inheritance,
UUIDv7 part names, manifest ordering, duplicate retention and staged publication
are preserved from the zipnum engine. Source verification is opt-in with
`--verify-sources`; source clusters are never modified.

Zipnum serving is read-only and exposes only `/cdx` and `/warcserver/cdx`.
Ingestion inputs and database options are rejected. Startup validates metadata,
summary syntax, sequence continuity, safe part names, ranges and global key
ordering without decompressing the cluster. Searches binary-search the open
summary and read candidate gzip blocks, including predecessor blocks. Queries
preserve duplicate entries, arbitrary CDXJ properties and manifest order.

Ordinary queries scan every candidate block. Pagination is explicit through
`page`, `pageSize` or `showNumPages=true`; the default page size is ten blocks.
Pages are zero-based and count candidate blocks, including the predecessor block.
The selected cluster is fixed for the process lifetime. Never modify published
clusters in place; publish another directory and restart the server to select it.

## pywb remote CDX configuration

With pywb 2.11.1, a standard remote CDX collection can load WARC bytes independently:

```yaml
collections:
  archive:
    index:
      type: cdx
      api_url: http://127.0.0.1:9999/cdx?url={url}&closest={closest}&sort=closest
      replay_url: ''
    archive_paths:
      - /srv/warcs/
      - https://archives.example.org/warcs/
```

The empty `replay_url` tells pywb to use returned `filename`, `offset`, `length`
and other CDX metadata with its archive resource paths. No plugin or database
conversion is required. The zipnum server has no record-retrieval endpoint.

The integration test exercises normal captures, revisits and explicitly addressed
POST-derived keys. **pywb's stock remote CDX provider interpolates `{url}`, not
its internal POST-augmented `alt_url`. Raw browser POST replay therefore has an
upstream limitation with this configuration.** Explicit augmented URLs work;
this test does not claim transparent browser POST forwarding.

## Compatibility and verification

Golden fixtures pin cdxj-indexer commit
`a65917b12fbef23e58ee3e66878b852923811fb6`, with warcio 1.8.2. They compare parsed
CDXJ keys and JSON values, not whitespace. Fixtures cover plain/gzip offsets,
missing digests, request pairing, multipart binary uploads, JSON and JSON-lines,
text/binary bodies, content lengths and the 4,096-character query limit.
`cmd/index/testdata/generate.py` regenerates them with the pinned reference.

Indexing and queries use whatwg-url/surt v0.9.0. The SURT fixture explicitly records
two differences from the Python reference: HTTP backslashes become path separators
(`http://example.com\path` becomes `com,example)/path`), and opaque DNS URLs retain
case (`dns:Example.COM`). Original URI evidence is retained. AMF bodies use opaque
base64 binary extraction rather than AMF decoding.

```sh
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
go generate ./docs
WARC_BINARY=/path/to/warc python integration/pywb_replay.py
```

The replay test requires pywb 2.11.1 and starts a temporary local server. The Go tests
also cover external-sort publication, source collisions, archive reopening,
backend persistence and cancellation, duplicate block boundaries, pagination and
narrow queries that avoid unrelated corrupt parts.

## Yearly preservation collections

Use [`warc redupe`](redupe.md) to resolve source revisits and rebuild independently
replayable UTC years before generating their CDXJ indexes. Capture databases now
use format v2 to retain every record-ID occurrence, including repeated IDs at
different offsets within one source file. Reindex v1 databases into a new directory.
