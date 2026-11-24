---
title: "warc zipnum build"
---
## warc zipnum build

Build a cluster from sorted CDXJ files

```
warc zipnum build INPUT... [flags]
```

### Options

```
      --blocks int                    blocks per part (inherit when applicable)
      --concurrency int               partition workers (default 7)
  -h, --help[=true|false]             help for build
      --lines int                     lines per block (inherit when applicable)
      --output-dir string             new cluster directory (default "cluster")
      --verify-sources[=true|false]   verify source parts and boundary blocks
```

### Options inherited from parent commands

```
      --config string       path to config file; if unset, searches standard XDG config locations and the current directory for config.yaml
  -O, --log-file string     log output destination ('-' for stderr) (default "-")
      --log-format string   log output format (text or json) (default "text")
      --log-level string    minimum log level (debug, info, warn, error) (default "info")
```

### SEE ALSO

* [warc zipnum](warc_zipnum.md)     - Build and transform immutable zipnum clusters
