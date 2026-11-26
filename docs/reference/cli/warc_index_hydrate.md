---
title: "warc index hydrate"
---
## warc index hydrate

Add verified revisit payload locations to an existing index for pywb

```
warc index hydrate INPUT [flags]
```

### Options

```
      --allow-unresolved[=true|false]   copy unresolved revisit rows unchanged
      --donor-index-dir string          additional read-only native donor catalogue
  -h, --help[=true|false]               help for hydrate
      --index-file string               output CDXJ filename (use .gz for gzip)
      --output-dir string               new ZipNum cluster directory
      --warc-root string                root directory of the WARCs used for replay
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
