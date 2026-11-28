---
title: "warc"
---
## warc

A tool for handling warc files

### Synopsis

Validate records with validate, or inspect revisit resolution across a collection with audit.
Use convert warc to rewrite with different output settings, repair to correct individual records,
and recover to stage and verify a recovered collection. Dedup writes eligible captures as revisits.
Redupe builds independently replayable periods. Preview with redupe --audit and retain work with --index-dir.

### Options

```
      --config string       path to config file; if unset, searches standard XDG config locations and the current directory for config.yaml
  -h, --help[=true|false]   help for warc
  -O, --log-file string     log output destination ('-' for stderr) (default "-")
      --log-format string   log output format (text or json) (default "text")
      --log-level string    minimum log level (debug, info, warn, error) (default "info")
```

### SEE ALSO

* [warc audit](warc_audit.md)     - Check revisit targets and payloads, optionally comparing original captures
* [warc cat](warc_cat.md)     - Concatenate and print warc files
* [warc console](warc_console.md)     - A shell for working with WARC files
* [warc convert](warc_convert.md)     - Convert web archive files to WARC files. Use subcommands for the supported formats
* [warc dedup](warc_dedup.md)     - Deduplicate WARC files
* [warc index](warc_index.md)     - Index WARC captures
* [warc ls](warc_ls.md)     - List WARC record fields
* [warc recover](warc_recover.md)     - Restore missing revisit targets and correct payload digests in deduplicated WARCs
* [warc redupe](warc_redupe.md)     - Build new deduplicated WARC collections by capture year, month or day
* [warc repair](warc_repair.md)     - Correct records using their own bytes
* [warc serve](warc_serve.md)     - Serve a Badger index, zipnum cluster, or WACZ
* [warc validate](warc_validate.md)     - Validate WARC files
* [warc version](warc_version.md)     - Show extended version information
* [warc zipnum](warc_zipnum.md)     - Build and transform immutable zipnum clusters
