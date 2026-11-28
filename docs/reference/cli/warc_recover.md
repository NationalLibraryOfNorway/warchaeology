---
title: "warc recover"
---
## warc recover

Restore missing revisit targets and correct payload digests in deduplicated WARCs

### Synopsis

Recover a deduplicated WARC collection when a revisit points to a missing
capture or its payload digest disagrees with the stored bytes. A revisit refers
to an earlier record (the donor) for its payload. Give COLLECTION as the root
directory of the surviving WARCs, including older files that may contain donors.

Recover scans .warc and .warc.gz files recursively. It can correct legacy
digests that included HTTP chunk framing. Supply --recovery-source to search a
backup for a missing donor, or --originals to replace a faulty revisit with a
verified original capture.

Choose a new --output-dir outside the inputs. Recover copies the surviving
WARCs, applies the repairs, and verifies the staged collection before publishing
it there. If verification finds unresolved revisits, the command exits nonzero
and reports the staging directory. The JSON report goes to stdout by default.

```
warc recover COLLECTION [flags]
```

### Examples

```
  warc recover surviving/ --recovery-source backups/ --output-dir recovered/ --report recovery.json
```

### Options

```
  -h, --help[=true|false]             help for recover
      --originals string              directory containing original captures
      --output-dir string             stage verified recovery in a fresh directory
      --recovery-source stringArray   backup WARC directory (repeatable)
      --report string                 new JSON report file (default stdout)
```

### Options inherited from parent commands

```
      --config string       path to config file; if unset, searches standard XDG config locations and the current directory for config.yaml
  -O, --log-file string     log output destination ('-' for stderr) (default "-")
      --log-format string   log output format (text or json) (default "text")
      --log-level string    minimum log level (debug, info, warn, error) (default "info")
```

### SEE ALSO

* [warc](warc.md)     - A tool for handling warc files
