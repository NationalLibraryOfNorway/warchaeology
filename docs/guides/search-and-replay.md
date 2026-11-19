---
title: Full-text search and replay
---

Search archived text with `warc`'s search console and API. For browser replay,
connect pywb to the server's CDX lookup and WARC-byte endpoints.

## Search available text

```sh
warc index --index-format badger --index-dir ./captures --fulltext collection.wacz
warc serve --index-dir ./captures --fulltext
```

Open `http://localhost:9999/console/`. The HTML/CSS console includes text search,
plain-text matching snippets, capture provenance, and a compact density timeline.
The timeline covers **all matching indexed documents**. Select an interval to filter it; use All Time to clear the interval.
The interface supports keyboard focus, reduced motion, and narrow viewports.

`--fulltext` imports Browsertrix `urn:text:` text resource records, other HTTP(S)
`text/plain` WARC resources, and inline text in `pages/*.jsonl` inside WACZs.
Nested packages retain separate identities, even when WARC names coincide.
Text search uses existing UTF-8 extracts. Decoded extracts and JSONL lines are
limited to 16 MiB each, and page metadata to 64 MiB per package. Oversized or
invalid extracts produce an indexing error.

Text records retain their own timestamps. When page metadata has the same URL,
the unique nearest page timestamp within that leaf WACZ supplies the title and
suggested replay time. Such associations are explicitly marked `inferred`.
Equal-distance ties remain unlinked. Inline page text is omitted when a WARC
extract has already been associated with that page.

Bleve stores the text index in the index database's `fulltext` subdirectory.
The default layout in the OS-specific user data directory is:

```text
warchaeology/
└── warcdb/
    └── fulltext/
```

`--index-dir ./captures` places the text index in `./captures/fulltext`.
Use `--fulltext-dir` to choose a separate location. Badger retains capture and
package metadata; the Bleve index can be rebuilt from the archive sources.
`serve` checks for catalogue changes every two seconds and reprocesses the
registered text when a change occurs. Reconciliation uses temporary disk storage
and resumes after interruption. Allow time and disk space for this work when
adding large collections.

Capture serving continues during text indexing. Search hides hits whose capture
generation or package fingerprint is obsolete. Counts and histogram bins are
estimates while reconciliation is pending. Search failures appear in
`/search/status` and logs; they do not stop the capture server. An `index` command
requesting full text fails if its text phase fails.

### Add text search to an existing index

If you already indexed your archives without `--fulltext`, start the server with:

```sh
warc serve --fulltext
```

The server reads the archive locations stored in the index database and builds
the text index in the background. You do not need to list the WARC or WACZ files
again. Keep the original archive files accessible at their indexed locations. If a rebuild
reports that a WACZ needs reindexing, run
`warc index --index-format badger --fulltext collection.wacz` with the original package to register its fingerprint.
Open `http://localhost:9999/console/` to search as indexing progresses.

For a database stored elsewhere, add `--index-dir`:

```sh
warc serve --index-dir ./captures --fulltext
```

### Rebuild the text index

Offline `warc index backup` includes the database's `fulltext/` index when present.
Restoring that backup restores search without a rebuild. HTTP backups contain
only Badger data.

Use `--fulltext-rebuild` to reprocess all registered text, for example after
restoring a Badger-only HTTP backup and its archive files. Stop any server using the
database, then run:

```sh
warc index --index-format badger --fulltext --fulltext-rebuild
warc serve --fulltext
```

The first command finishes text indexing and exits; the second starts the search
server. Both use the default database. Add the same `--index-dir` to both
commands when using another database.

If an upgrade requires a new text index format, add
`--fulltext-dir ./fulltext-new` to both commands, choosing a directory that does
not already contain an index.

To serve a completed text index without updating it:

```sh
warc serve --read-only --fulltext
```

Text search requires an index built from archive files. Collections populated
only through CDXJ uploads contain capture metadata, not the archive source
locations needed to import text.

## HTTP APIs

### Capture lookup and archive access

The core API exposes index data, WARC records and database operations. The
warcserver API exposes CDX lookup and archived HTTP resources for replay clients.

| API | Routes | Purpose |
| --- | --- | --- |
| Core capture lookup | `GET /cdx` | Find captures by URL and timestamp |
| Core record lookup | `GET /id`, `/id/{urn}`, `/record/{urn}` | List record IDs, resolve storage references and retrieve WARC records |
| Core metadata | `GET /file`, `/file/{filename}`, `/packages`, `/report`, `/report/{id}` | Inspect indexed files, WACZ membership and reports |
| Core database operations | `/collections`, `/stats`, `/export`, `/backup`, `POST /cdx`, `POST /delete` | Manage collections and index data; see [writable collections](writable-captures.md) |
| Warcserver resource retrieval | `GET /warcserver/web/{timestamp}id_/{url}` | Retrieve an archived HTTP response by URL and time |
| HTTP archive access | `GET /archives/cdx`, `GET` or `HEAD /archives/{id}/{generation}/{name}` | Resolve virtual WARC filenames and serve WARC byte ranges |
| Full-text search | `GET /search`, `/search/status` | Search indexed text and inspect indexing progress |

`/cdx` returns original archive filenames for mounted-file access.
`/archives/cdx` returns server-relative filenames for HTTP archive access, for
both standalone WARCs and WARC members inside WACZs. It includes only captures
with registered archive sources. Both views use the same query parameters and
output formats. Full-text search is a separate API backed by Bleve. `/warcserver/web/...` returns an archived HTTP resource; `/archives/...`
returns WARC bytes. Pywb supplies browser replay rewriting and access control.

```sh
# Core API: capture lookup
curl 'http://localhost:9999/cdx?url=https://example.com/'

# HTTP archive view: filenames resolved against /archives/
curl 'http://localhost:9999/archives/cdx?url=https://example.com/'

# Warcserver API: archived HTTP response nearest the requested timestamp
curl 'http://localhost:9999/warcserver/web/20250101000000id_/https://example.com/'
```

Native Badger databases provide record and resource retrieval. Index-only
collections expose index-management routes. Zipnum serving exposes `/cdx`. Full-text search requires `--fulltext` and a native Badger
index database. Every endpoint honors `--path-prefix`.

### Search APIs

The server provides two search APIs:

| API | Searches | Returns |
| --- | --- | --- |
| `GET /cdx` | Capture metadata by URL and timestamp | CDXJ or newline-delimited JSON records with WARC locations |
| `GET /search` | Indexed text and titles | JSON search hits, snippets, provenance and a timeline |

#### Capture lookup: `GET /cdx` and `GET /archives/cdx`

```sh
curl --get --data-urlencode 'url=https://example.com/' \
  http://localhost:9999/cdx

curl --get --data-urlencode 'url=example.com' \
  --data 'matchType=domain&from=2025&to=2025&output=json' \
  --data-urlencode 'filter==status:200' http://localhost:9999/cdx
```

| Parameter | Description | Default |
| --- | --- | --- |
| `url` | URL to look up; a trailing `*` selects prefix matching, and leading `*.` selects domain matching | Required unless `urlkey` is supplied |
| `urlkey` | Canonical URL key; takes precedence over `url` | Derived from `url` |
| `matchType` | `exact`, `prefix`, `host` or `domain` | `exact` |
| `from`, `to` | Inclusive UTC timestamp bounds, up to 14 digits (`YYYYMMDDhhmmss`); prefixes such as `2025` are accepted | Unbounded |
| `sort` | `reverse` or `closest` | URL-key and timestamp order |
| `closest` | Target UTC timestamp for `sort=closest` | Required for closest ordering |
| `filter` | Repeatable field filter: `mime:html` for substring, `=status:200` for exact, `~url:REGEX` for a start-anchored regular expression; prefix with `!` to negate | None |
| `fields`, `fl` | Comma-separated output fields; `fl` is an alias | All fields |
| `output` | `cdxj` or `json` (newline-delimited JSON) | `cdxj` |
| `limit` | Maximum returned records; positive values reduce the server limit | Server limit |
| `collapse` | Collapse adjacent equal field values, e.g. `digest` or `timestamp:4` | None |
| `collapseTo` | Keep the `first` or `last` record in each collapsed group | `first` |
| `method` | HTTP request method for POST/PUT-aware URL lookup | None |
| `requestBody` | Request-body query string produced by the indexer | None |
| `page` | Zero-based zipnum block page | `0` when pagination is enabled |
| `pageSize` | Zipnum blocks per page | `10` |
| `showNumPages` | `true` returns the number of zipnum block pages | `false` |

Zipnum pagination is enabled by `page`, `pageSize` or `showNumPages`.
Badger queries use independent scan and result caps, both 10,000 by default.
Inspect the `X-Cdx-Truncated` and `X-Cdx-Error` response trailers. Use `/export`
for a complete Badger export; see [writable collections](writable-captures.md).

`GET /collections/{name}/cdx` exposes the same lookup for a named collection.
`GET /archives/cdx` uses the same query parameters and returns virtual WARC filenames
for the HTTP archive endpoint described below.

#### Full-text search: `GET /search`

```sh
curl --get --data-urlencode 'q="northern lights"' \
  --data 'limit=20' http://localhost:9999/search
```

| Parameter | Description | Default |
| --- | --- | --- |
| `q` | Search terms, ANDed across title and text; double quotes select a phrase | Required |
| `host` | Exact indexed hostname | All hosts |
| `package` | Exact package ID | All packages |
| `from`, `to` | Timestamp bounds in RFC3339, e.g. `2025-01-01T00:00:00Z` | Unbounded |
| `offset` | Number of matching hits to skip | `0` |
| `limit` | Hits per page, from 1 to 100 | `20` |

Full-text dates use RFC3339; CDX dates use compact numeric timestamps.
Results are ranked by relevance, with additional weight for titles.
`offset + limit` must not exceed 10,000. Concurrent indexing can move hits
between pages.

The JSON response contains `hits`, `totalEstimate`, `timeline` (48 time buckets),
`offset` and `limit`. Each hit's `fields` contains URL, title, timestamps,
snippet, source, package ID and capture reference.

#### Full-text indexing status: `GET /search/status`

```sh
curl http://localhost:9999/search/status
```

No parameters. Returns whether text search is enabled and the background
indexing status, including errors.

## Replay with pywb

### Index-only server with mounted WARC files

Warchaeology serves CDX lookups; pywb reads the WARC files from its filesystem.

```sh
warc serve --collections-dir ./indexes
```

In another terminal, create a collection and upload its CDXJ index:

```sh
curl --fail -X PUT http://localhost:9999/collections/archive
curl --fail --data-binary @captures.cdxj \
  http://localhost:9999/collections/archive/cdx
```

Configure pywb's `config.yaml` with the paths mounted inside its container or host:

```yaml
collections:
  archive:
    index: cdx+http://127.0.0.1:9999/collections/archive/cdx
    archive_paths:
      - '/mnt/warcs/'
```

A CDXJ `filename` of `2025/01/capture.warc.gz` resolves to
`/mnt/warcs/2025/01/capture.warc.gz` in pywb. Add more directories to
`archive_paths` when files span multiple mounts. Warchaeology only needs the
CDXJ data in this setup.

To generate an index with paths relative to the archive root:

```sh
warc index --recursive --dir-root /srv/warcs \
  --index-file captures.cdxj /srv/warcs
```

Here `/srv/warcs` is the indexing host's path; pywb can mount the same files at
`/mnt/warcs`. For an existing native index database, use `/cdx` as the API path
with the same filesystem-based `archive_paths` configuration.

### Combine index sources

Use `index_group` to query a local zipnum cluster, OutbackCDX and Warchaeology
within one pywb collection:

```yaml
collections:
  $root:
    archive_paths:
      - /mnt/archives/
      - /mnt/crawls/
    index_group:
      zipnum:
        type: zipnum
        path: /mnt/index/cluster.idx
        max_blocks: 32
        shard_index_loc:
          match: '$'
          replace: 'parts/'
      outbackcdx: cdx+http://outbackcdx:8080/archive
      warchaeology: cdx+http://warchaeology:9999/cdx
```

All three sources return filenames resolved against the mounted `archive_paths`.
Adjust the service addresses, collection names and mount paths for your deployment.

### Serve indexes and WARC bytes over HTTP

```sh
warc serve collection.wacz
```

Create a pywb `config.yaml`:

```yaml
collections:
  archive:
    index: cdx+http://127.0.0.1:9999/archives/cdx
    archive_paths:
      - 'http://127.0.0.1:9999/archives/'
```

Run `pywb` in the directory containing that configuration. Pywb resolves WARC
filenames against `archive_paths`, using local files or HTTP byte ranges.
Keep the trailing slash on each archive path.

In both examples, replace `127.0.0.1:9999` with the Warchaeology address reachable
from pywb. Use the server's container or service name when they run in separate
containers.

`/archives/cdx` projects each filename to an opaque, generation-specific virtual WARC
path. It preserves the member's original offsets and record lengths. Pywb then
requests HTTP byte ranges from `/archives/...`; Warchaeology maps these back to
registered WARC sources, including members of nested WACZ files. The regular
`/cdx` endpoint and offline exports use the source filenames.

The byte endpoint supports GET, HEAD, Range, conditional requests and ETags. It
returns stored WARC bytes without applying HTTP `Content-Encoding: gzip`, so a
`.warc.gz` member's offsets remain valid. An obsolete source generation returns
410; an unregistered source returns 404. A file or container whose indexed
metadata has changed returns 409. Reindex changed sources and previously indexed
mounted sources that have no container metadata. Keep registered source bytes
immutable between indexing and replay. Capture deletion changes the lookup
index while the registered WARC file and its byte offsets stay intact.
STORE ZIP members support direct reads; DEFLATE members may require streaming
through a prefix to reach a requested offset. Neither requires unpacking a WACZ.

Pywb handles query filters, replay rewriting, access control and capture
selection. The remote CDX endpoint applies the configured query caps. For
POST-derived captures, supply the augmented URL key explicitly: pywb 2.11.1's
remote `{url}` template omits the method/body augmentation for browser POSTs.
