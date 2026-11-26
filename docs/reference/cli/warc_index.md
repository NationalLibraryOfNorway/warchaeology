---
title: "warc index"
---
## warc index

Index WARC captures

```
warc index FILE/DIR... [flags]
```

### Options

```
      --badger-database string           Badger database name
      --close-input-file-hook string     command to run after closing each input file; the command receives these environment variables:
                                             WARC_COMMAND contains the subcommand name
                                             WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                             WARC_FILE_NAME contains the file name of the input file
                                             WARC_ERROR_COUNT contains the number of errors found if the file was validated and the validation failed
  -c, --concurrency int                  number of input files to process in parallel (default 21)
      --continue-on-error[=true|false]   continue processing remaining files and directories after errors
      --dir-root string                  export filenames relative to this directory
  -f, --force[=true|false]               continue iterating even when record read errors occur
      --ftp-pool-size int32              size of the FTP connection pool (default 1)
      --fulltext[=true|false]            index available text extracts in embedded Bleve
      --fulltext-dir string              Bleve index directory (default: fulltext subdirectory of the index database)
      --fulltext-rebuild[=true|false]    rebuild text search from registered captures
  -h, --help[=true|false]                help for index
      --id strings                       only process records with these record IDs; repeat the flag or use a comma-separated list
      --index-dir string                 Badger index database directory (default: warchaeology/warcdb in the user data directory)
      --index-file string                index export filename or - for stdout (default "-")
      --index-format string              cdxj, cdxpb or badger (default "cdxj")
      --index-tmp-dir string             temporary sort directory
  -i, --input-file string                input filesystem source; default is the local OS filesystem
                                         Legal values:
                                             /path/to/archive.( tar | tar.gz | tgz | zip | wacz )
                                             zip://LITERAL_PATH, tar://LITERAL_PATH, tgz://LITERAL_PATH (no URL decoding)
                                             ftp://user:password@host:port (endpoint only; optional trailing /)
                                         FTP paths belong in command arguments; URL queries and fragments are not supported.

      --lax-host-parsing[=true|false]    allow lenient host parsing in URL parsing
      --lenient[=true|false]             minimize validation for faster, more permissive parsing
  -l, --limit int                        maximum number of records to process; ignored when --nth is set
      --long-url-keys string             persistent Badger ingestion of URL keys over 60000 bytes: preserve, skip, or error; file exports always preserve keys (default "preserve")
  -m, --mime-type strings                only process records with these MIME types; repeat the flag or use a comma-separated list
  -n, --nth int                          process only the n-th record after filtering
  -o, --offset int                       start processing at this byte offset in the input file (default: 0)
      --open-input-file-hook string      command to run before opening each input file; the command receives these environment variables:
                                             WARC_COMMAND contains the subcommand name
                                             WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                             WARC_FILE_NAME contains the file name of the input file
      --post-append[=true|false]         include paired request method and body in capture keys
      --pywb[=true|false]                resolve revisit payload locations for offline pywb CDXJ replay
  -t, --record-type strings              only process records with these record types; repeat the flag or use a comma-separated list.
                                         Legal values:
                                             warcinfo, request, response, metadata, revisit, resource, continuation and conversion
  -r, --recursive[=true|false]           walk input directories recursively
  -S, --response-code string             only process records by HTTP response code
                                         Example:
                                             200    - only records with a 200 response
                                             200-300    - records with response codes between 200 (inclusive) and 300 (exclusive)
                                             500-    - response codes from 500 and above
                                             -400    - all response codes below 400
      --sort[=true|false]                sort and deduplicate stdout CDXJ
      --source-file-list string          path to a file listing input paths, one per line
      --strict[=true|false]              fail on the first validation error
      --suffixes strings                 only process files with these suffixes (default [.warc,.warc.gz,.wacz])
  -s, --symlinks[=true|false]            follow symbolic links while walking
      --tmp-dir string                   directory used for temporary files (default "/tmp")
      --wacz-index string                WACZ indexing: auto imports embedded indexes; warc indexes contained WARCs (default "auto")
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
* [warc index backup](warc_index_backup.md)     - Back up an index database offline
* [warc index compact](warc_index_compact.md)     - Reclaim index database storage offline
* [warc index hydrate](warc_index_hydrate.md)     - Add verified revisit payload locations to an existing index for pywb
* [warc index reset](warc_index_reset.md)     - Reset the selected index database
* [warc index restore](warc_index_restore.md)     - Restore an index database backup offline into a new directory
