---
title: Full-text search and replay
---

The Go server and search console retain Warchaeology's Apache-2.0 license.
Use stock pywb for browser replay, with Warchaeology providing CDX lookup and
WARC bytes. The experimental browser replay client, session APIs and live-leak
monitoring are deferred and are not included in this branch.

## Search available text

```sh
warc index --index-format badger --badger-dir ./captures --fulltext collection.wacz
warc serve --badger-dir ./captures --fulltext
```

Open `http://localhost:9999/console/`. The HTML/CSS console includes text search,
plain-text matching snippets, capture provenance, and a compact density timeline.
The timeline covers **all matching indexed documents**, not just the displayed
page. Select an interval to filter it; use All Time to clear the interval.
The interface supports keyboard focus, reduced motion, and narrow viewports.

`--fulltext` imports Browsertrix `urn:text:` text resource records, other HTTP(S)
`text/plain` WARC resources, and inline text in `pages/*.jsonl` inside WACZs.
Nested packages retain separate identities, even when WARC names coincide.
No HTML extraction, OCR, language-specific stemming, or boilerplate removal is
performed. Text is UTF-8; decoded extracts are limited to 16 MiB each, JSONL lines
to 16 MiB, and page metadata to 64 MiB per package. Failures are reported rather
than silently truncating extracted text.

Text records retain their own timestamps. When page metadata has the same URL,
the unique nearest page timestamp within that leaf WACZ supplies the title and
suggested replay time. Such associations are explicitly marked `inferred`.
Equal-distance ties remain unlinked. Inline page text is omitted when a WARC
extract has already been associated with that page.

Bleve stores the rebuildable text index in `BADGER-DIR/fulltext`; override with
`--fulltext-dir`. Badger retains authoritative capture and package metadata.
`serve` checks a durable catalogue change marker every two seconds. A changed
catalogue triggers reconciliation: candidate metadata is spooled to temporary
disk, then text is read without holding the capture publication lock. The
completed marker advances only after publication and obsolete-document cleanup.
Interrupted work is safely retried. This first implementation reprocesses text
on a catalogue change; it is not a claim of incremental per-record efficiency.

Capture serving continues during text indexing. Search hides hits whose capture
generation or package fingerprint is obsolete. Counts and histogram bins are
estimates while reconciliation is pending. Search failures appear in
`/search/status` and logs; they do not stop the capture server. An `index` command
requesting full text fails if its text phase fails.

Backfill or reprocess a database without supplying its input paths again:

```sh
warc index --index-format badger --badger-dir ./captures --fulltext --fulltext-rebuild
```

Cached WACZ capture indexing does not prevent text backfill. `--read-only
--fulltext` requires an existing text index and never backfills. Badger backups
exclude Bleve; rebuild text after restoring the capture database and its sources.
Text indexes have a separate schema marker. An incompatible schema is rejected;
choose a new `--fulltext-dir` to rebuild it without changing Badger.
Search requires a native Badger database with
registered archive sources, not index-only collections or zipnum.

## Search API

```sh
curl 'http://localhost:9999/search?q=%22northern%20lights%22&limit=20'
curl 'http://localhost:9999/search/status'
```

`GET /search` accepts `q`, `host`, `package`, `from`, `to`, `offset` and `limit`.
Words are ANDed across title/text; double quotes require a phrase. Dates use
RFC3339 UTC. Titles receive extra relevance weight. The default page size is 20,
the maximum is 100, and pagination is limited to 10,000 matches. Concurrent
indexing can move hits between pages. Results include `hits`, `totalEstimate`,
`timeline` (48 time buckets), `offset` and `limit`. Each hit's `fields` contains
URL, title, timestamps, snippet, source, package ID and capture reference.

Every HTTP endpoint honors `--path-prefix`.

## Vanilla pywb: CDX and archive bytes

```sh
warc serve --badger-dir ./captures collection.wacz
```

Configure an unmodified pywb installation using `config.yaml`:

```yaml
collections:
  archive:
    index:
      type: cdx
      api_url: 'http://127.0.0.1:9999/pywb/cdx?url={url}&closest={closest}&sort=closest&from={from}&to={to}'
      replay_url: ''
    archive_paths:
      - 'http://127.0.0.1:9999/archives/'
```

Run `pywb` in the directory containing that configuration. No
`GowarcserverIndexSource`, `register_source`, Python override, shared filesystem,
or WACZ plugin is required. Keep the trailing slash on `archive_paths` and the
empty `replay_url`: pywb should read archived WARC bytes, rather than call another
HTTP replay engine.

`/pywb/cdx` projects each filename to an opaque, generation-specific virtual WARC
path. It preserves the member's original offsets and record lengths. Pywb then
requests HTTP byte ranges from `/archives/...`; Warchaeology maps these back to
registered WARC sources, including members of nested WACZ files. Pywb never
receives the WACZ paths. The regular `/cdx` and offline exports retain their
existing filename conventions.

The byte endpoint supports GET, HEAD, Range, conditional requests and ETags. It
returns stored WARC bytes without applying HTTP `Content-Encoding: gzip`, so a
`.warc.gz` member's offsets remain valid. An obsolete source generation returns
410; an unregistered source returns 404. Only registered sources can be opened.
STORE ZIP members support direct reads; DEFLATE members may require streaming
through a prefix to reach a requested offset. Neither requires unpacking a WACZ.

Stock pywb still controls its own query filters, replay rewriting, ACLs and
capture selection. Query caps apply to the remote CDX endpoint. As with the
existing remote-CDX integration, pywb 2.11.1's `{url}` template does not forward
its POST-derived `alt_url`: explicitly augmented POST keys are tested, but this
configuration does not claim transparent browser POST lookup.

The integration test runs **unmodified pywb 2.11.1** with only this configuration:

```sh
WARC_BINARY=/path/to/warc python integration/pywb_wacz.py
```

It checks leaf, nested, and deflated WACZs, exact HTTP ranges, normal captures,
revisits and explicitly addressed POST-derived captures.
