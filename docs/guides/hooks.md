---
title: Hooks
---

Hooks are executable commands that run before or after input/output file handling.

- `OpenInputFileHook`
- `CloseInputFileHook`
- `OpenOutputFileHook`
- `CloseOutputFileHook`

Hook context is passed through environment variables such as:

- `WARC_COMMAND`
- `WARC_HOOK_TYPE`
- `WARC_FILE_NAME`
- `WARC_SRC_FILE_NAME` (output hooks)
- `WARC_SIZE`, `WARC_INFO_ID`, `WARC_HASH`, `WARC_ERROR_COUNT` (close hooks when available)

A non-zero exit code is treated as an error.

## Hooks during yearly reconstruction

For [`warc redupe`](redupe.md), input hooks run while inventorying sources. An
input hook requesting a skipped file is treated as a failure so the supplied
collection cannot silently lose records.

Output hooks run on staging files before their year is published. Leave those
files in place: moving, deleting, or changing them can prevent output verification.
Open and close hook failures block publication of that year. Cancellation stops
running hooks, joins inventory workers, and closes the writers.

An output file can contain records from several input files, so a single
`WARC_SRC_FILE_NAME` is not available for yearly output. Use the completed year's
`manifest.jsonl` for record-level source provenance.
