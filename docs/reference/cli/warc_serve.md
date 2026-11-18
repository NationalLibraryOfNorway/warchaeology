---
title: "warc serve"
---
## warc serve

Serve a capture database or immutable zipnum cluster

```
warc serve [FILE/DIR...] [flags]
```

### Options

```
      --address string                      HTTP listen address (default ":9999")
      --badger-database string              Badger database name
      --badger-dir string                   Badger database directory (default "./warcdb")
      --close-input-file-hook string        command to run after closing each input file; the command receives these environment variables:
                                                WARC_COMMAND contains the subcommand name
                                                WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                                WARC_FILE_NAME contains the file name of the input file
                                                WARC_ERROR_COUNT contains the number of errors found if the file was validated and the validation failed
      --collections-dir string              directory of independently writable collections
  -c, --concurrency int                     number of input files to process in parallel (default 21)
      --continue-on-error[=true|false]      continue processing remaining files and directories after errors
      --dir-root string                     export filenames relative to this directory
  -f, --force[=true|false]                  continue iterating even when record read errors occur
      --ftp-pool-size int32                 size of the FTP connection pool (default 1)
      --fulltext[=true|false]               index available text extracts in embedded Bleve
      --fulltext-dir string                 text index directory (default: BADGER-DIR/fulltext)
      --fulltext-rebuild[=true|false]       rebuild text search from registered captures
  -h, --help[=true|false]                   help for serve
      --id strings                          only process records with these record IDs; repeat the flag or use a comma-separated list
      --index-format string                 badger or zipnum (default "badger")
      --index-out string                    output filename or - for stdout (default "-")
      --index-tmp-dir string                temporary sort directory
  -i, --input-file string                   input filesystem source; default is the local OS filesystem
                                            Legal values:
                                                /path/to/archive.( tar | tar.gz | tgz | zip | wacz )
                                                zip://LITERAL_PATH, tar://LITERAL_PATH, tgz://LITERAL_PATH (no URL decoding)
                                                ftp://user:password@host:port (endpoint only; optional trailing /)
                                            FTP paths belong in command arguments; URL queries and fragments are not supported.

      --lax-host-parsing[=true|false]       allow lenient host parsing in URL parsing
      --lenient[=true|false]                minimize validation for faster, more permissive parsing
  -l, --limit int                           maximum number of records to process; ignored when --nth is set
  -m, --mime-type strings                   only process records with these MIME types; repeat the flag or use a comma-separated list
  -n, --nth int                             process only the n-th record after filtering
  -o, --offset int                          start processing at this byte offset in the input file (default: 0)
      --open-input-file-hook string         command to run before opening each input file; the command receives these environment variables:
                                                WARC_COMMAND contains the subcommand name
                                                WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                                WARC_FILE_NAME contains the file name of the input file
      --path-prefix string                  prefix for all routes
      --post-append[=true|false]            include paired request method and body in capture keys
      --query-result-limit int              maximum returned interactive captures (default 10000)
      --query-scan-limit int                maximum captures examined by interactive queries (default 10000)
      --read-only[=true|false]              reject ingestion and report mutations
  -t, --record-type strings                 only process records with these record types; repeat the flag or use a comma-separated list.
                                            Legal values:
                                                warcinfo, request, response, metadata, revisit, resource, continuation and conversion
  -r, --recursive[=true|false]              walk input directories recursively
  -S, --response-code string                only process records by HTTP response code
                                            Example:
                                                200    - only records with a 200 response
                                                200-300    - records with response codes between 200 (inclusive) and 300 (exclusive)
                                                500-    - response codes from 500 and above
                                                -400    - all response codes below 400
      --sort[=true|false]                   sort and deduplicate stdout CDXJ
      --source-file-list string             path to a file listing input paths, one per line
      --strict[=true|false]                 fail on the first validation error
      --suffixes strings                    only process files with these suffixes (default [.warc,.warc.gz])
  -s, --symlinks[=true|false]               follow symbolic links while walking
      --tmp-dir string                      directory used for temporary files (default "/tmp")
      --upload-max-bytes int                maximum decoded CDXJ request bytes (default 134217728)
      --upload-max-line int                 maximum CDXJ line bytes (default 1048576)
      --upload-timeout duration             maximum upload duration (default 5m0s)
      --wacz-index string                   WACZ indexing: auto imports embedded indexes; warc indexes contained WARCs (default "auto")
      --warcserver-prefix-max-records int   warcserver prefix result limit (default 1000)
      --zipnum-dir string                   immutable published zipnum cluster
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
* [warc serve compact](warc_serve_compact.md)     - Reclaim capture database storage offline
* [warc serve restore](warc_serve_restore.md)     - Restore a capture backup offline into a new directory

