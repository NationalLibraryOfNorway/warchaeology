---
title: Overview
---

Warchaeology provides command-line tools for inspecting, validating, converting,
deduplicating, indexing, and serving web archive collections. The executable is
named `warc`.

## Choose a workflow

| Task | Command | Documentation |
| --- | --- | --- |
| Inspect records and payloads | `warc ls`, `warc cat`, `warc console` | [CLI reference](./reference/cli/index.md) |
| Validate an archive | `warc validate` | [Validation options](./reference/cli/warc_validate.md) |
| Convert ARC, NEDLIB, or WARC inputs | `warc convert` | [Conversion options](./reference/cli/warc_convert.md) |
| Deduplicate payloads | `warc dedup` | [Deduplication options](./reference/cli/warc_dedup.md) |
| Rebuild independently replayable yearly collections | `warc redupe` | [Yearly preservation guide](./guides/redupe.md) |
| Export CDXJ or populate a capture database | `warc index` | [Capture indexing guide](./guides/capture-indexes.md) |
| Build immutable indexes and serve capture queries | `warc zipnum`, `warc serve` | [Zipnum and serving](./guides/capture-indexes.md#immutable-zipnum-clusters) |

## Rebuild yearly collections

`redupe` inventories all supplied WARC records, resolves revisit dependencies,
and groups captures by their own UTC year. It retains request and metadata
records, accounts for identical aliases, and reports conflicting IDs or missing
payload donors. There are no record or date filters in this command.

Start with an audit, then write the yearly collection and index a completed year:

```sh
warc redupe --audit --verify-payloads --report audit.json --recursive sources/
warc redupe --output-dir yearly --index-dir working-index \
  --report transformation.json --recursive sources/
warc index --index-out yearly/2023.cdxj yearly/2023/
```

Supply older donor WARCs as well as the captures you want to reorganize. Each year
is published only after its output files and payload dependencies have been
verified. Keep `working-index` to restart an interrupted rewrite with the same
sources and configuration.

See the [yearly preservation guide](./guides/redupe.md) for audit findings,
checksums, provenance manifests, restart behavior, and replay setup. Capture
indexes are generated separately with `warc index`; they do not replace WARCs.

## Getting started

- [Install Warchaeology](./getting-started/installation.md).
- [Read directory, archive, and FTP inputs](./guides/archive-inputs.md).
- [Configure flags, environment variables, and YAML](./guides/configuration.md).
- [Run hooks around file handling](./guides/hooks.md).
- [Check migration requirements](./guides/upgrading.md), including capture database reindexing.
- [Contribute to the project](./contributing.md).

Run `warc help` or `warc COMMAND --help` for command-specific options.
