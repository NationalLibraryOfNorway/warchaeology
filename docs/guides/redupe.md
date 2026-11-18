---
title: Rebuild self-contained yearly collections
---

`warc redupe` reorganizes an existing WARC collection by the **UTC calendar year
of each record's WARC-Date**. It resolves identical-payload revisits, including
cross-year chains, and deduplicates independently within each output year.
The first eligible occurrence of a payload is a full record. Subsequent eligible
occurrences refer directly to that record, subject to `--min-size-gain`.

## Preserve the input collection

There are deliberately **no record, MIME, status, or date filters** in this
command. Supply all source WARCs, including any older files needed to resolve
revisits. Requests, metadata, source warcinfo records, and other contextual records
are retained. Exactly repeated records with the same ID become one logical output
record with an alias entry for every occurrence in the manifest. Conflicting
records with the same ID are errors; IDs are never silently reassigned.

Traversal follows the usual source-list, archive-input, suffix, symlink, and
`--recursive` options. These options define which files constitute the supplied
collection; `redupe` cannot discover missing donor files outside that collection.
A hook requesting that an input file be skipped is an error, not silent success.
Put the output directory outside source directories.

## Audit first

```sh
warc redupe --audit --report audit.json --recursive sources/
warc redupe --audit --verify-payloads --report verified.json --recursive sources/
```

Both audit modes inventory every supplied record, detect conflicting IDs, and
resolve dependencies using explicit IDs or exact original URI and referenced date.
There is no closest-capture fallback. Inventory reads source blocks and fingerprints
records to distinguish identical aliases from conflicting IDs. A normal audit does
not validate declared payload digests: its deduplication counts are metadata
estimates and its payload checks are explicitly marked unchecked.
`--verify-payloads` reconstructs and hashes payloads, checking every link's declared
digest. Rewriting always performs this verification.

Reports give selected records, aliases, distinct referenced source records,
cross-year payload dependencies, planned full records and revisits, verification
counts, and findings for each year. Payload expansion estimates count uncompressed
payload bytes added when source revisits become full records. They are **not**
predictions of compressed disk usage. Contextual references outside a year are
reported without blocking; the independence guarantee concerns payload/replay
dependencies. Without `--report`, JSON is written to standard output.

Missing or ambiguous targets, conflicting IDs, cycles, chains longer than 32
records, invalid metadata, and unsupported dependencies produce a nonzero exit.
Only identical-payload revisit profiles from WARC 1.0/1.1 are supported.
Server-not-modified/304 reconstruction and segmented records are unsupported.
A 304 response is never reconstructed by attaching another capture's body.

## Rewrite and restart

```sh
warc redupe --output-dir yearly --index-dir working-index \
  --report transformation.json --recursive sources/
```

Local Badger v4 stores a source catalog and separate run state. Metadata sorting
uses the existing bounded external sorter. Timestamp, source locator, and offset
determine target selection, independently of inventory worker completion order.
Use `--concurrency` to control inventory workers; yearly rewriting follows the
sorted plan. Output uses the existing compression, rollover, prefix, writer count,
flush, and hook options. One-to-one naming and custom subdirectory layouts are
rejected because a source file can span years.

Every year is written in a hidden staging directory. After closing writers and
checking hook results, `redupe` rereads the files, reconstructs their revisits using
only that year's output, verifies payloads and record counts, and records SHA-256
file checksums. Only then is the directory atomically renamed to its final year.
A failed year is never published as complete. Independently valid years can finish
even when other years have blocking planning findings. An inventory failure with
unknown scope blocks the whole rewrite.

Each completed year contains:

- WARC files with generated output-file warcinfo records and corresponding links.
- `manifest.jsonl`: one provenance mapping per input occurrence, with source
  locator/offset, original ID, source fingerprint, transformation action, payload
  donor, output filename/offset, and any new revisit target.
- `completion.json`: configuration, inventory fingerprint, tool version, counts,
  verification results, and SHA-256 checksums for the WARCs and manifest.

Capture IDs, URLs, dates, and capture-specific HTTP headers are preserved. Changed
block lengths and digests are recomputed. Eligible output payload digest labels
are normalized to SHA-256 so mixed or missing source digest labels do not affect
replay or deduplication. Source files are never rewritten.

Repeat the same command with the same `--index-dir` to restart. The entire source
inventory is rebuilt and must match the effective configuration and recorded
source size/modification metadata. Completed years are checksum-checked and kept;
an interrupted year is rebuilt from its beginning. Changed inputs or configuration
require a new index/output destination. Sources must remain immutable during a run;
metadata is rechecked before publication. Omitting `--index-dir` uses a temporary
working index and does not retain interrupted staging state across process death.

Output hooks run against staging files, before publication, and must not move or
remove those files. Hook failures block publication. Cancellation joins inventory
workers and closes readers, writers, and databases.

## Index and replay

Generate CDXJ with the existing index command:

```sh
warc index --index-out yearly/2023.cdxj yearly/2023/
```

Point your replay system at that index and only the corresponding year's WARC
directory. `integration/redupe_replay.py` demonstrates `redupe → index → pywb`
with three isolated years, same-year and cross-year revisits, and revisit chains:

```sh
WARC_BINARY=/path/to/warc python integration/redupe_replay.py
```

The script requires pywb 2.11.1 and warcio. It verifies original payloads and
capture-specific HTTP headers after deleting the source WARC.

The record-ID occurrence key now includes the source offset. The capture database
format version was bumped accordingly: existing v1 capture databases must be
reindexed into a new database. This prevents old indexes from silently concealing
multiple occurrences of an ID within one file.
