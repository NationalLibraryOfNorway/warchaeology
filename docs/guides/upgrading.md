---
title: Migration notes
---

## Changes after v5

### Index destination flags

Use `--index-file` for CDXJ and protobuf exports and `--index-dir` for the
Badger capture database. Update scripts and config that used `index-out` or
`badger-dir`. The environment variables are `WARC_INDEX_FILE` and
`WARC_INDEX_DIR`.

### Archive paths are literal

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

### FTP URLs select endpoints only

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

### Retained deduplication indexes

The deduplication index now uses a new encoding for digest keys and revisit
references, and tracks whether the referenced output has been completed.
Existing nonempty digest indexes from v5 are rejected.

If you retained a deduplication index with `--keep-index`, start the next run
with a fresh `--index-dir` and output directory. A donor is a record that stores
the payload referenced by a revisit record. The new index registers donors
from that run; it does not carry forward references from the old index.
Existing WARC files remain unchanged.

`validate`, `convert arc`, `convert nedlib`, `convert warc` and `dedup` also use
Badger to retain file-processing status with `--keep-index`. That application
format is unchanged. The deduplication rebuild requirement comes from the
changed digest-index format, not simply the Badger v3-to-v4 dependency update.

## Dedup donor indexes and historical output

Start a new dedup run with a fresh index directory and a fresh output directory. The donor key now includes the record type and a verified payload digest. Retained indexes created under older rules can contain unsuitable donors, and old processed-file checkpoints can skip the inputs you need to rebuild. Existing WARC output can be audited directly. See the [collection audit and recovery guide](./dedup-audit.md).
