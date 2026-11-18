---
title: Capture indexing, serving and zipnum
---

`warc index` creates replay capture indexes separately from the processing and
deduplication indexes. It accepts WARC and WARC.gz files and directories, plus
warchaeology's archive mounts. ARC indexing, Kafka,
standalone CDXJ file serving and AMF decoding are not implemented. For writable
CDXJ collections, see [writable capture collections](writable-captures.md).

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
closest ordering, CDXJ and newline-delimited JSON. Exact-URL closest and reverse queries
use ordered seeks; other sorted queries use external sorting. Interactive queries
now have separate scan/result caps and explicit truncation trailers.

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
differences from the pinned internetarchive/surt 0.3.1 reference: HTTP backslashes become path separators
(`http://example.com\path` becomes `com,example)/path`), and opaque DNS URLs retain
case (`dns:Example.COM` and `mailto:Person@Example.COM`). Leading `/../a`
and `/%2e%2e/a` normalize to `/a`, whereas the Python reference retains `/../a`.
Other tested dot segments, escaping, Unicode, query ordering and POST augmentation
match the pinned reference. HTTP ingestion rejects malformed absolute URLs rather
than accepting the reference library’s permissive repairs. Original URI evidence is retained. AMF bodies use opaque
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
use format v3 to retain every record-ID occurrence, including repeated IDs at
different offsets within one source file. Reindex v1/v2 databases into a new directory.

## WACZ packages

Positional `.wacz` files are indexed as packages without requiring `--recursive`:

```sh
warc index capture.wacz
warc index --index-format badger --badger-dir ./captures capture.wacz
warc serve --badger-dir ./captures capture.wacz
warc serve --wacz-index warc capture.wacz
```

The default `--wacz-index auto` imports embedded CDXJ (`.cdx`, `.cdxj`, and
 gzip-compressed variants, including concatenated gzip blocks). When no index
exists it indexes the contained WARCs. A malformed embedded index fails instead
of silently falling back. `--wacz-index warc` explicitly indexes the WARCs;
explicit record filters and WARC parsing options also select native indexing.
The output default remains CDXJ; `serve` defaults to Badger in `./warcdb`.

Badger retains container membership and structured source locations. Identical
member filenames in different packages remain distinct. Completed unchanged
packages reuse their index; package metadata fingerprints are cache checks, not
cryptographic verification of all WARC payloads. WARCs stay inside the WACZ.
Stored ZIP members support direct offset reads; deflated members use bounded
streaming decompression and may need to reread preceding bytes when seeking.
Keep packages immutable while serving them.

Embedded CDX does not supply a complete record-ID catalogue. URL/time retrieval
uses the package/member location; ID lookups and ID-dependent revisits may need
native indexing. An existing index-only database cannot host native WACZ content;
choose a fresh database directory.

### Multi-WACZ bundles and manifests

The same commands accept Browsertrix multi-WACZ bundles. The importer follows
`datapackage.json` child resources when present and discovers nested `.wacz`
members when there is no child manifest. It retains outer-package → child-package
→ WARC locations, supports up to 16 WACZ levels, and never extracts the packages.
A package may contain both WARCs and child packages. Missing, duplicate, unsafe,
or remote child references fail explicitly. Unchanged bundles reuse their index;
replacing a bundle removes captures belonging to children that were removed.
Completed children can remain published after a later child's failure; retrying
finishes the package without duplicating captures.

Original manifest JSON is preserved for every package, including descriptive
metadata, resource hashes, and extension fields. Read it from the server:

```sh
warc serve --badger-dir ./captures collection.wacz
curl --fail http://localhost:9999/packages
```

`GET /packages` streams NDJSON package records with `source`, `children`,
`sources` (WARC members), `manifest`, `version`, and `fingerprint`. It honors
`--path-prefix` and is available in read-only serving. An empty fingerprint means
package publication has not completed. Check the `X-Packages-Error` trailer for
streaming failures. The manifest is omitted when absent; its hashes are retained,
not automatically verified against every WARC byte. Browsertrix's zero CRC
placeholders for stored child WACZs are accepted; other member checksums retain
their existing validation.

Standalone JSON manifests referencing separate files or remote URLs, HTTP WACZ
downloads, and a replay UI are not included.

## Full-text search and pywb

See [full-text search and replay](search-and-replay.md) for `--fulltext`, the
search console and API, vanilla pywb configuration using `/pywb/cdx` plus HTTP
WARC ranges. Experimental browser replay and live-leak monitoring are deferred.
