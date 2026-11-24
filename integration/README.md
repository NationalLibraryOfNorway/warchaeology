# Integration tests

Run these commands from the repository root. The pywb tests require Python
with pywb 2.11.1 installed and a built `warc` executable.

## pywb with HTTP archive access

```sh
WARC_BINARY=/path/to/warc python integration/pywb_wacz.py
```

Tests vanilla pywb with CDX shorthand and Warchaeology's `/archives/cdx` and
`/archives/` endpoints. Covers mixed WARC/WACZ inputs, leaf and nested WACZs,
direct WACZ serving with streaming and seekable indexes,
deflated members, exact HTTP byte ranges, normal captures, revisits and
explicitly addressed POST-derived captures.

## pywb with mounted WARC files

```sh
WARC_BINARY=/path/to/warc python integration/pywb_replay.py
WARC_BINARY=/path/to/warc WARC_TEST_BACKEND=collections python integration/pywb_replay.py
```

Tests CDX lookup through zipnum serving (default) or writable collections,
with pywb reading local WARC files. Includes replay and pywb access-control checks.

## pywb with local CDXJ and ZipNum indexes

```sh
WARC_BINARY=/path/to/warc python integration/pywb_offline.py
WARC_BINARY=/path/to/warc python integration/pywb_hydrate.py
```

`pywb_offline.py` tests `warc index --pywb` with indexes built directly from
WARCs. `pywb_hydrate.py` starts from an existing index and checks that pywb
replays each revisit with one CDX query from both CDXJ and ZipNum.
