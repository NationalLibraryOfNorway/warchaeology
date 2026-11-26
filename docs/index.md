---
title: Overview
---

Warchaeology provides command-line tools for inspecting, validating, converting,
deduplicating, indexing, searching, and serving web archive collections.
The executable is named `warc`.

## Choose a command

| Command | Operator’s question | Result |
|---|---|---|
| `warc validate` | Do these WARC files pass record validation? | Reports validation findings. |
| `warc audit` | Do the revisits in my existing collection resolve to the expected payloads? | Examines the collection and reports findings. Optional `--originals` adds an independent comparison. |
| `warc convert warc` | How do I rewrite WARCs with different output settings? | Writes converted WARCs, for example with different compression. |
| `warc repair` | Can I correct a record using its own bytes? | Writes repaired records to an output directory. |
| `warc recover` | Can I restore consistency across donors and revisits? | Uses the collection and any supplied originals or backups to stage, verify, and publish a recovered collection. |
| `warc dedup` | Which captures can safely become revisits? | Writes deduplicated WARCs. Optional `--add-missing-digests` calculates absent labels during the same pass. |
| `warc redupe --audit` | What would this rewrite produce under my chosen interval and settings? | Reports a preview by period. An explicit `--index-dir` retains the inventory. Adding `--verify-payloads` also retains a verified plan. |
| `warc redupe` | How do I build independently replayable collections by year, month, or day? | Reuses compatible preview work, writes the periods, and verifies the output. A preview is optional. |

## Workflow guides

| Task | Command | Documentation |
| --- | --- | --- |
| Inspect records and payloads | `warc ls`, `warc cat`, `warc console` | [CLI reference](./reference/cli/index.md) |
| Validate an archive | `warc validate` | [Validation options](./reference/cli/warc_validate.md) |
| Convert ARC, NEDLIB, or WARC inputs | `warc convert` | [Conversion options](./reference/cli/warc_convert.md) |
| Deduplicate payloads | `warc dedup` | [Deduplication options](./reference/cli/warc_dedup.md) |
| Audit revisit references and recover lost donors | `warc audit`, `warc recover` | [Collection audit and recovery](./guides/dedup-audit.md) |
| Rebuild independently replayable yearly collections | `warc redupe` | [Yearly preservation guide](./guides/redupe.md) |
| Export CDXJ or populate an index database | `warc index` | [Capture indexing guide](./guides/capture-indexes.md) |
| Index WACZ packages | `warc index`, `warc serve` | [WACZ packages](./guides/capture-indexes.md#wacz-packages) |
| Search archived text | `warc index --fulltext`, `warc serve --fulltext` | [Search console and API](./guides/search-and-replay.md) |
| Build immutable indexes and serve capture queries | `warc zipnum`, `warc serve` | [Zipnum and serving](./guides/capture-indexes.md#immutable-zipnum-clusters) |

## Rebuild collections by time interval

A donor is a record that stores the payload referenced by a revisit record.

`redupe` inventories all supplied WARC records, resolves revisit dependencies,
and groups captures by UTC year, month or day (`--interval`). It retains request and metadata
records, accounts for identical aliases, and reports conflicting IDs or missing
payload donors.

Start with an audit, then write the yearly collection and index a completed year:

```sh
warc redupe --audit --verify-payloads --index-dir working-index --report preview.json --recursive sources/
warc redupe --output-dir yearly --index-dir working-index \
  --report transformation.json --recursive sources/
warc index --index-file yearly/2023.cdxj yearly/2023/
```

Supply older donor WARCs as well as the captures you want to reorganize. Each period
is published only after its output files and payload dependencies have been
verified. Keep `working-index` to restart an interrupted rewrite with the same
sources and configuration.

See the [yearly preservation guide](./guides/redupe.md) for audit findings,
checksums, provenance manifests, restart behavior, and replay setup. Capture
indexes generated with `warc index` provide URL/time lookup for the yearly WARCs.

## Getting started

- [Install Warchaeology](./getting-started/installation.md).
- [Read directory, archive, and FTP inputs](./guides/archive-inputs.md).
- [Configure flags, environment variables, and YAML](./guides/configuration.md).
- [Run hooks around file handling](./guides/hooks.md).
- [Check migration requirements](./guides/upgrading.md), including index database reindexing.
- [Contribute to the project](./contributing.md).

Run `warc help` or `warc COMMAND --help` for command-specific options.
