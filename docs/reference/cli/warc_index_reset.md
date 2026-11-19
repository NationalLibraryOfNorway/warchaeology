---
title: "warc index reset"
---
## warc index reset

Reset the selected index database

```
warc index reset [flags]
```

### Options

```
      --badger-database string   Badger database name
  -h, --help[=true|false]        help for reset
      --index-dir string         Badger index database directory (default: warchaeology/warcdb in the user data directory)
      --index-format string      database backend: badger (default "badger")
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
