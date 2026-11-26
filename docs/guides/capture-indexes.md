---
title: Capture indexing, serving and zipnum
---

`warc index` creates CDX indexes for URL/time lookup in web archive collections.
It reads WARC, WARC.gz and WACZ files, directories, and supported archive mounts.
Export CDXJ files or populate a Badger index database for serving. To manage
collections through CDXJ uploads, see [writable index collections](writable-captures.md).

## Export captures

```sh
warc index capture.warc.gz                         # unsorted CDXJ on stdout
warc index --sort capture.warc.gz > captures.cdxj  # sorted, exact-line deduplication
warc index -r --index-file captures.cdxj.gz warcs/  # sorted gzip output
warc index --post-append --dir-root warcs --index-file captures.cdxj warcs/a.warc.gz
warc index --index-format cdxpb --index-file captures.pb capture.warc.gz
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

Exported filenames default to basenames. Use `--dir-root` to keep the path
relative to an archive directory in the CDX `filename` field. For example,
from `/srv`, both commands emit `2025/news.warc.gz`:

```sh
warc index --dir-root /srv/warcs /srv/warcs/2025/news.warc.gz
warc index --dir-root /srv/warcs warcs/2025/news.warc.gz
```

Inputs outside the root can produce paths containing `../`. For example,
with the same root, `/srv/other/news.warc.gz` yields `../other/news.warc.gz`.

Database retrieval uses separate structured source locators containing literal
archive mount paths and member names. Basename collisions produce an ambiguity
error. Configure pywb's archive paths separately when using exported indexes.

`cdxpb` is a stream of unsigned-varint message lengths followed by serialized
`schema.Cdx` protobuf messages. There are no textual prefixes or newline
separators. The schema is in `internal/cdx/proto/cdx.proto` and includes original
URI, canonical key, source, filename, offset, stored length and request metadata.
Use a length-delimited protobuf decoder, such as Go's `protodelim.UnmarshalFrom`.

### Build a pywb index from WARCs

```sh
warc index --pywb --post-append --recursive --dir-root /srv/warcs \
  --index-file index.cdxj /srv/warcs
```

Point pywb's `index` at `index.cdxj` and its `archive_paths` at `/srv/warcs/`.
For a compressed handoff, export `index.cdxj.gz` and decompress it before serving,
or build a local ZipNum cluster from it. The WARC files can stay gzip-compressed.

`--pywb` retains the selected captures' keys and writes verified original
response locations as `orig.filename`, `orig.offset`, and `orig.length` for
HTTP identical-payload revisits. The CDXJ row carries the donor MIME so pywb
uses the explicit location. Redirect revisits use their own headers in pywb.
Include all referenced WARCs among the inputs. Response and revisit donors are
collected from the scanned range even when capture filters exclude them from the
export. The command fails if it cannot resolve a required donor or if archive
filenames are ambiguous. It uses temporary disk space under `--index-tmp-dir`
and leaves an existing named index intact on resolution failure.

For WACZ input, the command scans the contained WARCs. Make those same WARC
members available to pywb under the exported filenames.

To use the same resolved revisits in a local ZipNum collection, build from
the exported CDXJ. The cluster preserves the original-location fields. A lookup
can then read the donor WARC range even when the donor row was excluded by an
export filter.

```sh
warc zipnum build index.cdxj --output-dir /srv/indexes/cluster
```

Stock pywb can read the cluster directly with this collection configuration.

```yaml
collections:
  archive:
    index:
      type: zipnum
      path: /srv/indexes/cluster/cluster.idx
      shard_index_loc:
        match: '/$'
        replace: '/parts/'
    archive_paths:
      - /srv/warcs/
```

### Add revisit payload locations to an existing index

A WARC revisit can contain the headers for a capture while its payload is
stored in an earlier response. `warc index hydrate` verifies that response in
the mounted WARCs and adds its location to the index.

Suppose a February revisit of `https://example.org/latest` refers to the
payload of a January capture of `https://example.org/story`.

**Before hydration**

```text
org,example)/latest 20260201000000 {"filename":"feb.warc.gz","length":"430","mime":"warc/revisit","offset":"1200","url":"https://example.org/latest"}
```

**After hydration**

```text
org,example)/latest 20260201000000 {"filename":"feb.warc.gz","length":"430","mime":"text/html","offset":"1200","orig.filename":"jan.warc.gz","orig.length":"2380","orig.offset":"4800","url":"https://example.org/latest"}
```

The `filename`, `offset`, and `length` still point to the February headers.
The new `orig.*` fields point to the January response, and `mime` now gives
that response's MIME type. pywb can get both locations from one CDX query.

```sh
warc index hydrate captures.cdxj --warc-root /srv/warcs \
  --index-file captures.pywb.cdxj

warc index hydrate cluster --warc-root /srv/warcs \
  --output-dir cluster-pywb
```

Hydration looks up original responses in the input index, including when the
input is a ZipNum cluster. If a response is indexed only in a separate native
Badger catalogue, add `--donor-index-dir /srv/catalogue`. Set `--warc-root`
to the directory pywb uses in `archive_paths`.

The input can be plain CDXJ, gzip-compressed CDXJ, or a ZipNum directory. For
CDXJ output, use `--index-file` and add a `.gz` suffix for gzip compression.
CDXJ output keeps the input order, and pywb expects a sorted CDXJ file. Use
`--output-dir` to sort the rows and write a new ZipNum cluster. Choose an
output path that is separate from the input.

By default, every revisit that needs a payload location must resolve before
the output is published. Use `--allow-unresolved` to keep verified updates and
carry unresolved rows over unchanged. The command reports each unresolved
capture and the reason it could not be resolved.

## Databases

```sh
warc index --index-format badger --index-dir ./captures warcs/a.warc.gz
warc index reset --index-format badger --index-dir ./captures
```

Badger **v4** is the supported database backend and is used throughout
warchaeology, including processing and deduplication indexes. Index databases have a format/canonicalization marker;
unversioned or incompatible stores are rejected with reindexing guidance.
Capture format v4 requires a fresh database. For native indexes, run
`warc index --index-format badger --index-dir NEW_DIR` with the original
archives and indexing flags. For uploaded collections, export CDXJ with the
previous version, create each collection in the new store, and POST its CDXJ
to `/collections/{name}/cdx`. Older backups require the previous binary to
restore and export their contents.

For FTP ingestion, pass the endpoint URL to `--input-file` and the remote
archive path as a positional argument. Locators retain the normalized host,
port and an opaque account identifier. Reopening an authenticated source in a
later command requires a matching URL in the command's configuration file.

```yaml
ftp-endpoints:
  - "ftp://reader:password@archive.example:21"
```

Protect this configuration as a credential file. Changing the password for the
same endpoint and account does not require reindexing.

For retained deduplication indexes from v5, see the
[migration notes](upgrading.md#retained-deduplication-indexes).

Index databases store capture entries, source identity, metadata and reports.
Identity includes source and offset, so equal URL/timestamp
captures from different files remain distinct. Each successful ingestion publishes
a durable file-generation checkpoint. Interrupted generations stay hidden; a
successful retry removes obsolete captures and record IDs from that source.

## Database serving

```sh
warc serve --index-dir ./captures
warc serve --index-dir ./captures --read-only
warc serve --index-dir ./captures warcs/a.warc.gz
```

The defaults are Badger and `:9999`. Inputs trigger one background ingestion pass;
the server remains running afterward. Ingestion and handlers share one database
handle. Read-only mode rejects inputs and omits report mutation routes. On
Windows, the serving account needs permission to open or create the shared
`LOCK` file in the database directory.
`--path-prefix /archive` prefixes every route. HTTP requests have bounded timeouts;
SIGINT/SIGTERM cancel ingestion, drain handlers and close resources.

Indexing and serving share the default index database location:

| OS | Directory |
| --- | --- |
| Linux | `$XDG_DATA_HOME/warchaeology/warcdb`, or `~/.local/share/warchaeology/warcdb` |
| macOS | `~/Library/Application Support/warchaeology/warcdb` |
| Windows | `%LOCALAPPDATA%\warchaeology\warcdb` |

Use `--index-dir` or `WARC_INDEX_DIR` to select another directory. To continue
using an existing working-directory database, pass `--index-dir ./warcdb`.

Database routes include `/cdx`, `/file`, `/file/:filename`, `/id`, `/id/:urn`,
`/record/:urn`, `/report`, `/report/:id`, `/debug` and the imported `/warcserver`
CDX/resource routes. CDX supports exact, prefix, host and domain matching, date
bounds, substring/exact/regex filters, `fields`, positive limits, reverse and
closest ordering, CDXJ and newline-delimited JSON. Exact-URL closest and reverse queries
use ordered seeks; other sorted queries use external sorting. Interactive queries
now have separate scan/result caps and explicit truncation trailers.

Flags use the existing Viper configuration and `WARC_*` environment mapping,
e.g. `WARC_INDEX_FORMAT`, `WARC_INDEX_DIR` and `WARC_ADDRESS`.

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

Zipnum serving is read-only and exposes only `/cdx`.
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
    index: cdx+http://127.0.0.1:9999/cdx
    archive_paths:
      - /srv/warcs/
      - https://archives.example.org/warcs/
```

Pywb reads WARC records using the returned `filename`, `offset` and `length`.
Configure `archive_paths` with the local or HTTP locations of those WARC files.

For POST-derived captures, supply the augmented URL key explicitly. Pywb's
remote CDX provider uses `{url}`, which omits the method/body augmentation for
browser POSTs. Integration tests cover explicitly augmented URLs, normal
captures and revisits.

## Compatibility and verification

Golden fixtures pin cdxj-indexer commit
`a65917b12fbef23e58ee3e66878b852923811fb6`, with warcio 1.8.2. They compare parsed
CDXJ keys and JSON values, not whitespace. Fixtures cover plain/gzip offsets,
missing digests, request pairing, multipart binary uploads, JSON and JSON-lines,
text/binary bodies, content lengths and the 4,096-character query limit.
`cmd/index/testdata/generate.py` regenerates them with the pinned reference.

Indexing and URL queries use the Internet Archive canonicalization profile in
whatwg-url/surt v0.9.1. Original URLs are preserved in capture metadata. DNS,
mailto and warcinfo URIs undergo canonicalization; filedesc URIs retain their
original spelling. If canonicalization fails, the original string is used as
the key. CDXJ ingestion requires valid absolute URLs.

Compatibility tests track differences from Python internetarchive/surt 0.3.1:

| Input | Warchaeology key | Python SURT key |
| --- | --- | --- |
| `http://example.com\path` | `com,example)/path` | `com\path,example)/` |
| `http://example.com/../a` | `com,example)/a` | `com,example)/../a` |
| `http://example.com/%2e%2e/a` | `com,example)/a` | `com,example)/../a` |

The fixtures also cover escaping, Unicode, query ordering and POST augmentation.
AMF request bodies are represented as base64 binary data. Multipart extraction
streams uploaded files and retains the first 4,096 characters of the escaped
query. For JSON, plain-text and URL-encoded bodies larger than 256 KiB, the
indexer records `__wb_post_sha256=<hex digest>` over the body bytes. Use the
stored requestBody when looking up these captures. External tools that independently
derive the Python CDXJ key can produce a different key for these large bodies.

With `warc index --lenient`, recoverable WARC specification findings produce
warnings and indexing continues. HTTP status, MIME type and payload digests
are still extracted. Use `--force` to attempt recovery from record read errors.

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
replayable yearly collections before generating their CDXJ indexes.

## WACZ packages

Pass `.wacz` files directly to index or serve them:

```sh
warc index capture.wacz
warc index --index-format badger --index-dir ./captures capture.wacz
warc serve --index-dir ./captures capture.wacz
warc serve --wacz-index warc capture.wacz
warc serve --wacz-index direct capture.wacz
```

The default `--wacz-index auto` imports embedded CDXJ (`.cdx`, `.cdxj`, and
 gzip-compressed variants, including concatenated gzip blocks). When no index
exists it indexes the contained WARCs. A malformed embedded index produces an
error. Use `--wacz-index warc` to index the WARCs directly;
explicit record filters and WARC parsing options also select native indexing.
The output default is CDXJ; `serve` defaults to Badger in the user data directory.

Badger retains container membership and structured source locations. Identical
member filenames in different packages remain distinct. Completed unchanged
packages reuse their index based on package metadata fingerprints. WARCs stay
inside the WACZ.
Stored ZIP members support direct offset reads; deflated members use bounded
streaming decompression and may need to reread preceding bytes when seeking.
Keep packages immutable while serving them.

URL/time retrieval uses the package/member location. Use `--wacz-index warc`
when a complete record-ID catalogue is needed for ID lookups or revisit
resolution. Start with a fresh database directory when adding native WACZ
content to a collection previously managed through CDXJ uploads.

### Long URL keys

Canonical lookup keys encode literal ASCII spaces as `%20` so they fit in
one CDXJ column. The original URL retains its spelling. If an older catalogue
contains keys with literal spaces, rebuild the affected data from source.

CDXJ and CDXPB exports preserve the full lookup key, including when a WACZ
import or pywb export needs temporary storage. Embedded CDXJ uses the key in
its first column. Its input row limit is 1 MiB.

For persistent Badger file ingestion, `--long-url-keys=preserve` (default)
stores keys over 60,000 bytes in an overflow index. Exact queries use a digest
lookup. Prefix and domain queries also examine the dictionary of distinct long
keys and merge the matching captures in CDX order.

Use `--long-url-keys=skip` to omit these captures from the catalogue or
`--long-url-keys=error` to fail ingestion. Skip warnings identify the source
path, archive members, CDXJ line or WARC offset, and key length. Re-ingesting
with `preserve` recovers previously skipped captures. These settings apply to
file ingestion into a persistent catalogue. File exports always preserve keys.

The first persistent overflow write upgrades the catalogue format marker.
Older binaries reject that catalogue. To roll back, restore a backup made
before the upgrade or rebuild from source with the older binary.

### Serve one embedded index directly

`warc serve --wacz-index direct capture.wacz` serves one local WACZ in read-only
mode without creating a Badger database or a sidecar. The package must contain
CDXJ and WARC members. A valid block summary lets the server seek within a
stored gzip CDXJ member. Plain CDXJ and gzip indexes without a usable summary
are streamed, so some lookups may read a large portion of the index.

Use `/cdx` for capture lookup or point pywb at `/archives/cdx` and `/archives/`
for lookup and WARC ranges. Direct mode preserves embedded keys. URL lookups
use Warchaeology's canonicalization, and `urlkey=` accepts the exact stored key
if the index producer uses a different canonicalization. Keep the package
immutable while serving and restart the server after replacing it.

### Multi-WACZ bundles and manifests

The `auto` and `warc` modes accept Browsertrix multi-WACZ bundles. The importer follows
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
warc serve --index-dir ./captures collection.wacz
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

## Full-text search and pywb

See [full-text search and replay](search-and-replay.md) for `--fulltext`, the
search console and API, and pywb configuration using `/archives/cdx` and HTTP
WARC ranges.
