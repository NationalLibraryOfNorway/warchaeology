---
title: "warc audit"
---
## warc audit

Verify revisit references and payloads across a local WARC collection

```
warc audit COLLECTION [flags]
```

### Options

```
  -h, --help[=true|false]             help for audit
      --originals string              directory containing original captures
      --recovery-source stringArray   backup WARC directory (repeatable)
      --repair-to string              stage verified repairs in a new directory
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
