---
title: "warc serve restore"
---
## warc serve restore

Restore a capture backup offline into a new directory

```
warc serve restore BACKUP NEW-DIRECTORY [flags]
```

### Options

```
  -h, --help[=true|false]   help for restore
```

### Options inherited from parent commands

```
      --config string       path to config file; if unset, searches standard XDG config locations and the current directory for config.yaml
  -O, --log-file string     log output destination ('-' for stderr) (default "-")
      --log-format string   log output format (text or json) (default "text")
      --log-level string    minimum log level (debug, info, warn, error) (default "info")
```

### SEE ALSO

* [warc serve](warc_serve.md)     - Serve a capture database or immutable zipnum cluster
