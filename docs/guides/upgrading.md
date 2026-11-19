---
title: Migration notes
---

These notes describe compatibility changes in the dependency and filesystem
handling update.

## Build and Go API changes

- Building requires Go 1.27.0 or later. The preferred toolchain and CI use 1.27.3.
- Exported reader APIs now use types from
  `github.com/nationallibraryofnorway/gowarc/v3`, replacing
  `github.com/nlnwa/gowarc/v3`. Update downstream imports and dependency
  declarations together.
- Code calling command options' `Run()` methods must call `Close()` on those
  options afterward, including on errors. Writer finalization, index cleanup,
  and mounted-filesystem cleanup belong to the caller. CLI users need no extra
  cleanup step.
- Command `Run()` methods return errors instead of terminating the process.
  Traversal errors that could previously be masked by a successful process exit
  now produce a nonzero CLI exit status.

## Archive paths are literal

The `zip://`, `tar://`, and `tgz://` prefixes now select only the archive format.
Everything after the prefix is a literal filesystem path.

| Previous intent | New spelling |
| --- | --- |
| Open a filename containing a space | `"zip://my archive.zip"` |
| Open a filename containing the literal text `%20` | `zip://my%20archive.zip` |
| Open an absolute path | `zip:///var/data/archive.zip` |

Query-like text, fragments, userinfo-like text, and ports are no longer
discarded from archive filenames. For example, `zip://archive.zip#backup`
now opens `archive.zip#backup`, not `archive.zip`. URL normalization is no
longer applied to these paths.

Malformed TAR input and unsupported entry types now return errors instead of
being silently accepted or failing through the former adapter's error paths.
The supported TAR entry types are regular files and directories.

## FTP URLs select endpoints only

Move remote paths out of the URL and into command arguments:

```shell
# Old form: the URL path was silently ignored.
warc ls --input-file 'ftp://user:password@example.org/archives/2025'

# New form:
warc ls --input-file 'ftp://user:password@example.org' --recursive archives/2025
```

FTP URLs accept an empty path or `/`, both selecting the login directory.
Other paths, queries, and fragments are rejected. FTP endpoints use
`net/url` with explicit hostname and port checks, rather than WHATWG browser
repair. Credentials are decoded exactly once; a literal `+` remains a plus
sign.

WARC URI validation and derived hostname output retain their WHATWG behavior.
Stored URI headers are preserved.

See [Archive and FTP inputs](archive-inputs.md) for runnable usage examples.

## Capture indexes and Badger v4

All Badger databases now use v4, including processing and deduplication indexes.
Keep existing v3 directories until replacement indexes have been rebuilt and
verified. New index databases use versioned canonicalization and source-position
identities; older index databases require reindexing. See
[Capture indexing, serving and zipnum](capture-indexes.md) for migration and replay
configuration, including the stock pywb remote provider's POST limitation.

## Index database format v2

The record-ID index now includes the source offset. Multiple records sharing an
ID within the same WARC file remain visible, allowing conflicting occurrences to
be detected instead of silently overwriting one another.

Existing v1 index databases must be reindexed into a new directory or namespace.
Keep the old database until the replacement has been verified. This change does
not rewrite source WARCs or require regenerating existing CDXJ exports.

## Yearly reconstruction with redupe

[`warc redupe`](redupe.md) rebuilds self-contained UTC yearly collections. Unlike
record-selection commands, it has no record, MIME, status, or date filters. Every
supplied record is retained or accounted for as an identical alias; conflicting
IDs and unresolved payload dependencies block publication of affected years.

Audit before rewriting, and use a separate output directory. Reuse the same
working index only with unchanged sources and configuration. Completed years are
checksum-checked on restart, and interrupted years are rebuilt from the beginning.

## Index and workload databases

Index layout v3, donor v2 and redupe ledger v2 require fresh directories. Follow the
[rebuild and export/import procedures](writable-captures.md#layout-upgrades-and-workload-boundaries),
then verify the rebuilt indexes before retiring the old directories.

## Canonicalization profile v0.9.1

Version 0.9.1 changes the canonical keys for `dns:`, `mailto:` and `warcinfo:`
URIs. For example, `dns:Example.COM` becomes `dns:example.com`. Filedesc URIs
retain their original spelling.

Rebuild index databases created with the v0.9.0 profile before using this
version. The index layout remains v3; the stored profile marker identifies
the key-generation rules. The server rejects databases and backups carrying
the older marker.

- For CDXJ collections, export with the old executable and import into a fresh
  database using the new executable.
- For native archive retrieval, reindex the original WARCs or WACZs into a fresh
  database to retain source mappings and record-ID lookup.
- Rebuild full-text indexes against the new index database.

For zipnum, regenerate affected CDXJ keys from the original `url` and stored
`method`/`requestBody`, sort the result, and build a new cluster. The builder
preserves supplied keys, so their canonicalization profile must match the URL
query service. Keep the original CDXJ exports as migration inputs.
