---
title: Archive and FTP inputs
---

These examples use shell quoting to keep spaces and punctuation in filenames.
Replace the sample filenames and server address with your own.

## Read a local archive

Use an archive as the input filesystem, then select files inside it:

```shell
warc ls --input-file collection.zip --recursive /
warc validate --input-file collection.tar.gz --recursive /
```

The archive's extension selects the format: `.zip` and `.wacz` use ZIP;
`.tar` uses TAR; `.tgz` and `.tar.gz` use compressed TAR.
The positional `/` refers to the archive filesystem's root.

To select one file within the archive:

```shell
warc cat --input-file collection.zip records/example.warc.gz
```

## Validate every WARC file in a WACZ

Mount the WACZ as the input filesystem and validate recursively from its root:

```shell
warc validate --input-file collection.wacz --recursive --continue-on-error /
```

This finds files ending in `.warc` or `.warc.gz` throughout the WACZ, including
its `archive/` directory. The `/` refers to the root inside the WACZ.
`--continue-on-error` allows remaining files to be processed when a file fails;
it does not turn validation failures into success.

The command reports per-file results and totals. It validates the contained
WARC files, not the WACZ package's manifests, indexes, or signatures.

## Select a format explicitly

Use `zip://`, `tar://`, or `tgz://` when the archive has an unusual extension:

```shell
warc ls --input-file 'zip://my archive.data' --recursive /
warc ls --input-file 'tar:///var/data/collection.bin' --recursive /
warc ls --input-file 'tgz://compressed-backup.data' --recursive /
```

The prefix selects the format. Everything after `://` is a literal filename,
including `%`, `#`, `?`, `@`, and `:`. Absolute paths keep their leading
separator.

For example, these select different files:

```shell
warc ls --input-file 'zip://my archive.zip' --recursive /
warc ls --input-file 'zip://my%20archive.zip' --recursive /
```

The first filename contains a space; the second contains the literal characters
`%20`. No percent decoding occurs. Likewise, `zip://archive.zip#backup` opens
a file named `archive.zip#backup`; it does not discard `#backup`.

Plain archive filenames follow the same literal-path rule. Older versions
interpreted archive prefixes as URLs; replace percent-encoded spaces with actual
spaces when migrating those commands.

## Read from FTP

An FTP URL selects the server and credentials. Supply directories or filenames
as positional arguments:

```shell
warc ls --input-file 'ftp://user:password@example.org:2121' --recursive archives/2025
warc cat --input-file 'ftp://user:password@example.org' archives/example.warc.gz
```

The default port is 21. An empty URL path or a trailing `/` selects the account's
login directory; the slash does not change to the server's root directory.

This form is rejected:

```shell
warc ls --input-file 'ftp://user:password@example.org/archives/2025'
```

Move `archives/2025` to the command arguments instead. Queries and fragments,
including empty `?` and `#` components, are also rejected.

FTP credentials use URL escaping. For example, `user%40example` decodes to
`user@example`, and `p%23ss+word` decodes to `p#ss+word`. A literal `+` stays
a plus sign; credentials are decoded once.

## Temporary storage and archive size

Processing a ZIP/WACZ archive stored on FTP requires temporary disk space
for the downloaded archive:

```shell
warc ls --input-file 'ftp://user:password@example.org' archives/collection.zip
```

FTP ZIP/WACZ archives encountered during traversal are downloaded to the system
temporary directory before their entries
are processed, releasing the FTP connection for other inputs. The files are
removed during normal command cleanup.

Local ZIP/WACZ archives remain open until their queued member reads finish. TAR/TGZ archives
are loaded into memory before processing; their uncompressed contents must fit
in available memory. `--tmp-dir` does not move TAR/TGZ contents out of RAM.

## Original URIs and derived hostnames

Display the stored target URI alongside its derived hostname:

```shell
warc ls --fields ah example.warc
```

For a target URI such as `HTTP://Example.COM:80/path?q=+`, the URI field retains
that text while the hostname field displays `example.com`. Hostname extraction
uses WHATWG normalization; it does not rewrite the stored URI.

To stop on WARC validation errors:

```shell
warc validate --strict example.warc
```

Strict WARC validation is not a switch to reject every recoverable WHATWG URL
issue. URI validation preserves the original header text.
