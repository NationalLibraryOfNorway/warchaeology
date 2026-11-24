---
title: "warc index backup"
---
## warc index backup

Back up an index database offline

### Synopsis

Back up a stopped index database and its fulltext subdirectory to a new archive. Source WARC and WACZ files are not included.

```
warc index backup DIRECTORY BACKUP [flags]
```

### Options

```
  -h, --help[=true|false]   help for backup
```

### Options inherited from parent commands

```
      --config string       path to config file; if unset, searches standard XDG config locations and the current directory for config.yaml
  -O, --log-file string     log output destination ('-' for stderr) (default "-")
      --log-format string   log output format (text or json) (default "text")
      --log-level string    minimum log level (debug, info, warn, error) (default "info")
```

### SEE ALSO

* [warc index](warc_index.md)     - Index WARC captures
