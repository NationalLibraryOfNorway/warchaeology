---
title: "warc cat"
---
## warc cat

Concatenate and print warc files

```
warc cat FILE/DIR ... [flags]
```

### Examples

```

# Print all content from a WARC file (in principle the same as zcat)
warc cat file1.warc.gz

# Pipe the payload of the 4th record into the image viewer feh
warc cat -n4 -P file1.warc.gz | feh -
```

### Options

```
  -z, --compress[=true|false]            compress output (per record)
      --continue-on-error[=true|false]   continue processing remaining files and directories after errors
  -f, --force[=true|false]               continue iterating even when record read errors occur
      --ftp-pool-size int32              size of the FTP connection pool (default 1)
  -w, --header[=true|false]              show WARC header
  -h, --help[=true|false]                help for cat
      --id strings                       only process records with these record IDs; repeat the flag or use a comma-separated list
  -i, --input-file string                input filesystem source; default is the local OS filesystem
                                         Legal values:
                                             /path/to/archive.( tar | tar.gz | tgz | zip | wacz )
                                             zip://LITERAL_PATH, tar://LITERAL_PATH, tgz://LITERAL_PATH (no URL decoding)
                                             ftp://user:password@host:port (endpoint only; optional trailing /)
                                         FTP paths belong in command arguments; URL queries and fragments are not supported.

      --lax-host-parsing[=true|false]    allow lenient host parsing in URL parsing
      --lenient[=true|false]             minimize validation for faster, more permissive parsing
  -l, --limit int                        maximum number of records to process; ignored when --nth is set
  -m, --mime-type strings                only process records with these MIME types; repeat the flag or use a comma-separated list
  -n, --nth int                          process only the n-th record after filtering
  -o, --offset int                       start processing at this byte offset in the input file (default: 0)
  -P, --payload[=true|false]             show payload
  -p, --protocol-header[=true|false]     show protocol header
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
      --source-file-list string          path to a file listing input paths, one per line
      --strict[=true|false]              fail on the first validation error
      --suffixes strings                 only process files with these suffixes (default [.warc,.warc.gz])
  -s, --symlinks[=true|false]            follow symbolic links while walking
      --tmp-dir string                   directory used for temporary files (default "/tmp")
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
