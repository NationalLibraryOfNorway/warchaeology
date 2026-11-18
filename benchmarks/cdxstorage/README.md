# CDX storage experiments

This is a separate Go module. Badger **v4.3.1** and Pebble **v2.1.7** are pinned
here; Pebble is absent from the production module. The encodings are synthetic
prototypes, not byte-for-byte copies of either production capture schema.

```sh
cd benchmarks/cdxstorage
go build -o /tmp/cdxstorage .
python3 run.py --output results.jsonl
python3 run.py --order sorted --workloads capture,inventory --output results-sorted.jsonl
python3 run.py --workloads inventory --wal-off --output results-scratch.jsonl
python3 verify_results.py results.jsonl results-sorted.jsonl results-scratch.jsonl
python3 summarize.py results.jsonl results-sorted.jsonl results-scratch.jsonl > summary.json
```

The default is one million captures for each of seeds 11, 29 and 47, with
200,000 distinct URLs and 333,333 distinct payloads. Inputs contain repeated
hosts and filenames, varied paths/query parameters, timestamps, offsets,
pseudorandom digests, revisits, POST metadata and a custom integer above 2^53.
Input generation is incremental. Globally sorted input is prepared through an
external sort limited to 64 MiB, before ingestion timing; generation time is
reported separately. Deduplication uses eight concurrent workers rather than a
meaningless global insertion order. Both engines receive identical key/value
bytes within each encoding comparison.

Capture values compare repeated textual fields with compact binary digests,
varints and file dictionary references. Dictionary rows are persisted and
included in all storage counts. Inventory also stores occurrence lookup
references, fingerprints and planning metadata. Current-style inventory lookup
values duplicate the record; compact lookup values reference it. The dedup
experiment compares Badger's transactional registration/conflict retry with a
coordinated Pebble lookup-and-register critical section. Duplicate payloads
have deterministic synthetic donor references so full engine checksums can be
compared independently of scheduling.

Durable runs use synced commits for both engines. Capture/inventory commits
contain approximately 500 entries; registration commits one new donor at a time.
Both engines use Snappy, a 64 MiB block cache and a 16 MiB memtable target.
Badger additionally has a 16 MiB index cache and 64 MiB value-log files. These
settings are comparable nominal budgets, not equal hard RSS limits; background
engine allocations are measured explicitly.

The harness runs engines in separate sequential child processes and closes and
reopens between phases. Results include total logical file sizes, allocated disk
blocks, sampled peak allocated disk, process peak RSS, ingestion/reclamation time,
exact-key seek latency, full entry counts and streaming checksums. Maintenance
peaks are sampled every 100 ms and can miss shorter spikes. Inputs and the
machine-readable result files are outside the database footprint. The runner
removes database directories after recording their final measurements.

Durable capture/inventory phases are ingest, reopen, compact/GC, reopen, delete
20% and reinsert half with changed values, reopen, reclaim, reopen. Each reopened
state must retain its checksum. `verify_results.py` additionally checks expected
live counts, equality between engines, order equivalence and deletion outcomes.
Dedup requires exactly one registration for every unique payload and verifies
reopened persistent state. `summary.json` reports bytes per live record; for
dedup that denominator is donors, not captures or unique URLs.

`--wal-off` means **disposable inventory only**. Pebble disables its WAL; Badger
keeps its WAL and disables synchronous writes because it has no equivalent
persistent WAL-off option. The `disposable`, `sync_writes` and `wal_off` fields
make the distinction explicit in the disposable results. Neither is a durable
service advantage. The original durable results omit the newer explanatory
fields; their `wal_off: false` rows all used synchronous writes.

## Recommendation

**Keep Badger in production. Optimize schemas independently.** On this host,
median allocated bytes after initial compaction in ingestion order were:

| Workload / live denominator | Current Badger | Compact Badger | Current Pebble | Compact Pebble |
| --- | ---: | ---: | ---: | ---: |
| Capture / capture | 219.3 | 105.0 | 190.8 | 91.3 |
| Dedup / donor | 105.9 | 60.1 | 91.3 | 49.6 |
| Inventory / occurrence | 490.4 | 200.0 | 434.9 | 162.5 |

The prototype encoding reduces Badger allocation by approximately 52%, 43% and
59% respectively. Pebble's additional savings with compact encodings are about
13%, 17% and 19%. This is not a consistent 20% engine-space improvement across
these workloads. Pebble also uses substantially less peak memory in several
runs, while the coordinated durable donor-registration workload is slower than
Badger here. Inspect individual rows, including reclamation peaks and post-churn
results, rather than treating a single metric as a migration decision.

These measurements support removing duplicated values and unnecessary metadata.
They do **not** establish production v3 savings, an optimal dictionary design,
billion-record capacity, or equivalent operational behavior at that scale.
The prototype does not exercise the HTTP publication journal, WARC output
publication, the full source checkpoint catalogue, or production query mixtures.
Its generic planning rows are not a measurement of the production publication
ledger. Before an engine-switch decision, repeat on representative archives and
hardware, include those production metadata costs, and benchmark isolated codec
changes. Do not attribute the prototype's combined schema reduction entirely to
dictionaries or compact references. The host was shared with validation work;
space/checksum results are more useful here than absolute throughput numbers.
