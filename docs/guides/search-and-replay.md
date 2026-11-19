---
title: Full-text search and replay
---

Search archived text with `warc`'s search console and API. For browser replay,
connect pywb to the server's CDX lookup and WARC-byte endpoints.

## Search available text

```sh
warc index --index-format badger --badger-dir ./captures --fulltext collection.wacz
warc serve --badger-dir ./captures --fulltext
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

Bleve stores the rebuildable text index in `BADGER-DIR/fulltext`; override with
`--fulltext-dir`. Badger retains authoritative capture and package metadata.
`serve` checks for catalogue changes every two seconds and reprocesses the
registered text when a change occurs. Reconciliation uses temporary disk storage
and resumes after interruption. Allow time and disk space for this work when
adding large collections.

Capture serving continues during text indexing. Search hides hits whose capture
generation or package fingerprint is obsolete. Counts and histogram bins are
estimates while reconciliation is pending. Search failures appear in
`/search/status` and logs; they do not stop the capture server. An `index` command
requesting full text fails if its text phase fails.

Backfill or reprocess a database without supplying its input paths again:

```sh
warc index --index-format badger --badger-dir ./captures --fulltext --fulltext-rebuild
```

Text backfill also works for previously indexed WACZ packages. Use `--read-only
--fulltext` to serve an existing text index. After restoring a Badger backup and
its archive sources, rebuild the text index. If the text schema has changed,
choose a new `--fulltext-dir` for the rebuild. Search requires a native Badger
database with registered archive sources.

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

## Replay with pywb

```sh
warc serve --badger-dir ./captures collection.wacz
```

Create a pywb `config.yaml`:

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

Run `pywb` in the directory containing that configuration. Keep the trailing
slash on `archive_paths` and the empty `replay_url`; these settings direct pywb
to fetch WARC records from the HTTP archive endpoint.

`/pywb/cdx` projects each filename to an opaque, generation-specific virtual WARC
path. It preserves the member's original offsets and record lengths. Pywb then
requests HTTP byte ranges from `/archives/...`; Warchaeology maps these back to
registered WARC sources, including members of nested WACZ files. The regular
`/cdx` endpoint and offline exports use the source filenames.

The byte endpoint supports GET, HEAD, Range, conditional requests and ETags. It
returns stored WARC bytes without applying HTTP `Content-Encoding: gzip`, so a
`.warc.gz` member's offsets remain valid. An obsolete source generation returns
410; an unregistered source returns 404. Only registered sources can be opened.
STORE ZIP members support direct reads; DEFLATE members may require streaming
through a prefix to reach a requested offset. Neither requires unpacking a WACZ.

Pywb handles query filters, replay rewriting, access control and capture
selection. The remote CDX endpoint applies the configured query caps. For
POST-derived captures, supply the augmented URL key explicitly: pywb 2.11.1's
remote `{url}` template omits the method/body augmentation for browser POSTs.

To verify this configuration with pywb 2.11.1:

```sh
WARC_BINARY=/path/to/warc python integration/pywb_wacz.py
```

It checks leaf, nested, and deflated WACZs, exact HTTP ranges, normal captures,
revisits and explicitly addressed POST-derived captures.
