---
title: "warc redupe"
---
## warc redupe

Reorganize WARC records by year, month or day

### Synopsis

Group records by their WARC-Date, using UTC and --interval (year, month or day).
Resolve revisits and deduplicate within each period so each collection can be
replayed independently. Use --audit for a rewrite preview. With --index-dir, a preview retains
the inventory and sorted order. Add --verify-payloads to retain the verified plan for execution.

```
warc redupe SOURCE... [flags]
```

### Options

```
      --audit[=true|false]              plan without writing replacement WARCs
      --close-input-file-hook string    command to run after closing each input file; the command receives these environment variables:
                                            WARC_COMMAND contains the subcommand name
                                            WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                            WARC_FILE_NAME contains the file name of the input file
                                            WARC_ERROR_COUNT contains the number of errors found if the file was validated and the validation failed
      --close-output-file-hook string   command to run after closing each output file; the command receives these environment variables:
                                            WARC_COMMAND contains the subcommand name
                                            WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                            WARC_FILE_NAME contains the file name of the output file
                                            WARC_SIZE contains the size of the output file
                                            WARC_INFO_ID contains the ID of the output file's WARCInfo-record if created
                                            WARC_SRC_FILE_NAME contains the file name of the input file if the output file is generated from an input file
                                            WARC_HASH contains the hash of the output file if computed
                                            WARC_ERROR_COUNT contains the number of errors found if the file was validated and the validation failed
  -z, --compress[=true|false]           enable gzip compression for WARC output files (default true)
      --compression-level int           gzip compression level (1-9, -1 uses the gzip library default) (default -1)
  -c, --concurrency int                 number of input files to process in parallel (default 21)
  -C, --concurrent-writers int          maximum number of WARC files written concurrently.
                                        This may create at least this many output files even with a single input file. (default 16)
      --default-date string             fallback date used when records are missing WARC-Date metadata (time is set to 12:00 UTC) (default "2026-11-26")
      --file-size string                maximum size of each WARC output file (default "1GB")
      --flush[=true|false]              sync each WARC file to disk after every record
      --ftp-pool-size int32             size of the FTP connection pool (default 1)
  -h, --help[=true|false]               help for redupe
      --index-dir string                local working directory; retained run state enables safe restart
  -i, --input-file string               input filesystem source; default is the local OS filesystem
                                        Legal values:
                                            /path/to/archive.( tar | tar.gz | tgz | zip | wacz )
                                            zip://LITERAL_PATH, tar://LITERAL_PATH, tgz://LITERAL_PATH (no URL decoding)
                                            ftp://user:password@host:port (endpoint only; optional trailing /)
                                        FTP paths belong in command arguments; URL queries and fragments are not supported.

      --interval string                 group records by UTC year, month or day (default "year")
      --min-size-gain string            minimum payload size saving required to emit a revisit (default "2KB")
      --name-generator string           name generator strategy.
                                        With 'identity', the input filename is reused for output (prefix/suffix may still change),
                                        and exactly one output file is created per input file. (default "default")
      --one-to-one[=true|false]         write each input file to exactly one output file.
                                        Equivalent to: --concurrent-writers=1 --file-size=0 --name-generator=identity
      --open-input-file-hook string     command to run before opening each input file; the command receives these environment variables:
                                            WARC_COMMAND contains the subcommand name
                                            WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                            WARC_FILE_NAME contains the file name of the input file
      --open-output-file-hook string    command to run before opening each output file; the command receives these environment variables:
                                            WARC_COMMAND contains the subcommand name
                                            WARC_HOOK_TYPE contains the hook type (OpenInputFile, CloseInputFile, OpenOutputFile, CloseOutputFile)
                                            WARC_FILE_NAME contains the file name of the output file
                                            WARC_SRC_FILE_NAME contains the file name of the input file if the output file is generated from an input file
  -w, --output-dir string               output directory for generated WARC files (must already exist) (default ".")
  -p, --prefix string                   filename prefix for generated WARC files
  -r, --recursive[=true|false]          walk input directories recursively
      --report string                   write the audit or transformation report as JSON
      --source-file-list string         path to a file listing input paths, one per line
      --subdir-pattern string           pattern used to create output subdirectories.
                                        Use '/' to separate subdirectories on all platforms.
                                        Supported tokens: {YYYY}, {YY}, {MM}, {DD}.
                                        The WARC-Date of each record is used, so one input file may be split across subdirectories.
                                        With --name-generator=identity, only the first record date is used per input file.
      --suffixes strings                only process files with these suffixes (default [.warc,.warc.gz])
  -s, --symlinks[=true|false]           follow symbolic links while walking
      --verify-payloads[=true|false]    verify payload digests during audit (always enabled for rewriting)
      --warc-version string             WARC version used for generated files (default "1.1")
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
