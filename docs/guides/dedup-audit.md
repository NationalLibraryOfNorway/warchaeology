---
title: Audit and recover a deduplicated collection
---

A revisit can replay only if its donor supplies the right payload. Start by preserving the surviving WARCs, originals, backups, old CDX indexes, and any pre-loss file manifests. Audit a stable collection while writers are stopped or use a read-only snapshot.

## Find the impact of lost files

```sh
warc audit surviving/ --report impact.json
warc audit surviving/ --originals original-crawls/ --report comparison.json
```

Give the command the whole local collection root. It descends through subdirectories and reads regular `.warc` and `.warc.gz` files. Include older files that may contain donors. The command creates a temporary disk-backed inventory in the system temporary directory. Allow room for metadata for every record and the full report. Reports use new paths outside the inputs. Omitting `--report` writes JSON to standard output.

Check `summary.scanComplete` and `filesExamined` first. An unreadable file makes coverage incomplete. Other files are still examined. The records array includes file, byte offset, record ID, status, and available reference evidence. Findings for a missing target are grouped together. A nonzero exit means findings or an execution error require attention.

| Report result | What it establishes |
| --- | --- |
| `distinctMissingTargets` | Number of distinct unresolved reference identities. Multiple identities can belong to the same lost file. |
| `directly-dangling` | The immediate target is absent from this inventory. |
| `transitively-dangling` | An intermediate revisit exists, but a later link is missing. The target field identifies that missing link. |
| `digest-mismatch` or `invalid-record` | A resolved chain contains contradictory digest or record evidence. |
| `unverifiable` or `conflicting-id` | The detail explains the limitation, such as conflicting copies, a cycle, an unsupported profile, or a segmented dependency. |
| `output-consistent` | The chain resolves and its declared payload digests match the surviving donor bytes. |
| `original-verified` or `original-mismatch` | An original full capture with the same ID, URI, and date supplied independent payload and protocol-header evidence. |
| `pre-existing-revisit` | The matched input was already a revisit. Its retained bytes cannot establish the original response body. |

The audit supports WARC 1.0 and 1.1 identical-payload profiles and follows up to 32 records. It resolves explicit IDs first. References without IDs require the exact original URI and instant. Unsupported profiles, including server-not-modified, remain findings. Compare the missing IDs, URIs and dates with your pre-loss inventory to identify lost files. A surviving collection cannot enumerate captures from a missing file that no surviving record references.

## Assess historical deduplication

Tests against historical main revision `4486911` reproduced two incorrect revisits. One used a valid HTTP block digest as a donor key for a different capture whose payload contained that entire HTTP message. That run exited successfully. Another capture declared a stale payload digest for different bytes. The run reported an error and still wrote a revisit. Local main revision `7703b9b` contains the same digest selection logic.

These examples establish the mechanism. The incidence in a collection depends on its inputs. An accurate payload digest on two identical bodies is the ordinary deduplication case. Loss of a donor file can break an originally correct revisit. Measure dangling references across the full surviving collection. To estimate wrong-body incidence, compare with original captures, stratify by producer and run, record the numerator and denominator, and report the coverage of digest-error runs separately. A sample drawn only from known failures cannot estimate the collection-wide rate.

Current dedup requires verified payloads and matching record types and digest labels. HTTP block-only responses stay full. A complete resource can use its verified block digest as its payload digest. Repair mode may supply missing labels. A record with contradictory digest evidence stays full. Keep fresh donor and processed-file indexes together with their corresponding output when rebuilding.

## Recover into a fresh directory

```sh
warc audit surviving/ --repair-to recovered/ --recovery-source backup-a/ \
  --recovery-source backup-b/ --originals original-crawls/ --report recovery.json
```

Supply whichever evidence you have. A verified original can replace a bad revisit with the original full capture. When that original's block digest verifies but its payload header contradicts its bytes, recovery records the correction and writes the calculated SHA-256 payload digest. An exact backup donor with verified payload can restore many revisit references at once. A backup with a different digest label can require materializing a verified body with the revisit's own headers for local-CDX pywb replay. A matching digest on an unrelated capture produces a candidate for investigation and leaves the missing reference unresolved.

Recovery copies surviving WARCs into a sibling staging directory, retaining relative paths and copying unaffected WARCs byte-for-byte. Its output consists of WARC files. Supply a fresh destination outside all inputs. The JSON records restorations and changed offsets. The command reopens staged files and verifies chains against staged donors before publication. It checks restored full captures against the expected payload and protocol headers. Inspect `summary.afterRecovery` for the resulting counts. A partial result stays in the reported `.incomplete-` directory and exits nonzero. With incomplete input coverage the command reports the issue before staging.

Rebuild CDX, capture databases, and other offset-based indexes from the published WARCs before switching replay. Test a recovered capture's date, response headers, and body with the backups disconnected. Retain the originals, manifests, and audit reports as evidence.

## Recover historical chunked payload digests

Some older writers included HTTP chunk framing in the payload digest. For example,
a response storing the entity `hello` in one chunk could have a digest covering
the chunk size, delimiters, and terminator as well as those five bytes. Gowarc v4
computes the payload digest over the five entity bytes.

For a standalone full capture, `warc convert warc --repair` can recalculate the
digest from its stored body. For deduplicated WARCs, supply the directory
containing the full donor and its revisits together:

```sh
warc audit surviving/ --repair-to recovered/ --report recovery.json
```

A `legacy-chunked-digest` finding means the old label matched the stored HTTP
body including its framing. Recovery checks the donor, follows the references,
and writes matching entity digest labels to the affected records. The report
records each correction as `corrected-legacy-chunked-digest`. Recovery also
handles revisits whose donor already has the corrected digest. After verified
publication, rebuild the replay indexes against the recovered files.
