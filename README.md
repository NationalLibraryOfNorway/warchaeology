# warchaeology

Warchaeology is a command-line tool for inspecting, validating, converting,
indexing and serving web archives.

## Installation

Install the latest release into `~/.local/bin` on Linux or macOS.
Supports x86-64 (AMD64) and ARM64 (including Apple Silicon), plus 32-bit x86 on Linux:

```sh
curl -fsSL https://nationallibraryofnorway.github.io/warchaeology/install.sh | bash
```

The installer verifies the release checksum.

[Installation instructions](https://nationallibraryofnorway.github.io/warchaeology/getting-started/installation)

## Usage

### Inspect and validate

```sh
# List timestamps, HTTP status codes and URLs
warc ls --fields bsa capture.warc.gz

# List HTML responses as JSON lines
warc ls --record-type response --mime-type text/html --json capture.warc.gz

# Print the first response record
warc cat --record-type response --limit 1 capture.warc.gz

# Print the fourth record
warc cat -n 4 capture.warc.gz

# Get the byte offset of the tenth record
offset=$(warc ls --fields V -n 10 capture.warc.gz)

# Print the record at that offset
warc cat --offset "$offset" -n 1 capture.warc.gz

# Print the fourth response starting at that offset
warc cat --offset "$offset" --record-type response -n 4 capture.warc.gz

# Validate a directory tree
warc validate --recursive warcs/

# Validate all WARC files inside a WACZ, including nested bundles
warc validate collection.wacz

# Validate one WARC file inside a WACZ
warc validate --input-file collection.wacz archive/capture.warc.gz

# Open an interactive shell for a WARC file
warc console capture.warc.gz

# Open an interactive shell for a directory
warc console warcs/
```

`--offset` starts at a record boundary in the input file. `-n` counts from that
position, starting at 1, after record filters are applied. They can be combined
for both plain and gzip-compressed WARCs.

### Convert and deduplicate

```sh
mkdir -p converted deduplicated

# Convert ARC files to compressed WARC files
warc convert arc --recursive --output-dir converted arcs/

# Rewrite WARCs with 1 GB output files
warc convert warc --recursive --file-size 1GB --output-dir converted warcs/

# Replace duplicate payloads with revisit records
warc dedup --recursive --output-dir deduplicated warcs/
```

### Index and serve WARC or WACZ collections

```sh
# Write CDXJ to stdout
warc index capture.warc.gz

# Write a sorted, compressed index from a directory tree
warc index --recursive --index-out captures.cdxj.gz warcs/

# Import an embedded index
warc index capture.wacz > captures.cdxj

# Index the contained WARC records directly
warc index --wacz-index warc capture.wacz > records.cdxj

# Index WARC and WACZ files into the same Badger index database
warc index --index-format badger capture.warc.gz capture.wacz

# Index a mixed directory tree, including WACZ files
warc index --recursive --index-format badger archives/

# Serve the existing index database
warc serve

# Index while serving
warc serve capture.wacz

# Serve an existing index database read-only
warc serve --read-only

# Use a separate index database for a collection
warc serve --badger-dir ./news-index news.wacz
```

Indexing and serving accept `.warc`, `.warc.gz` and `.wacz` inputs by default.
Without `--badger-dir`, the index database is stored in the OS-specific user data
directory under `warchaeology/warcdb`.

Query a running server from another terminal:

```sh
# Find captures of a URL
curl 'http://localhost:9999/cdx?url=https://example.com/'

# Find captures with filenames served through /archives/
curl 'http://localhost:9999/archives/cdx?url=https://example.com/'

# Find captures across a domain within a year
curl 'http://localhost:9999/cdx?url=example.com&matchType=domain&from=20250101&to=20251231'

# Read WACZ manifests and package membership
curl http://localhost:9999/packages
```

[Indexing and serving guide](docs/guides/capture-indexes.md)

### Maintain an index database

Stop the server before running offline maintenance:

```sh
# Back up the database to a new file
warc index backup ./archive-index archive.backup

# Restore into a new database directory
warc index restore archive.backup ./restored-index

# Reclaim space after deletions or reindexing
warc index compact ./archive-index
```

Offline backups contain the Badger index database and its `fulltext/` index when
present. Restore recovers both. Keep the original WARC and WACZ files separately.

### Repair WARC records

`--repair` (`-R`) rewrites recoverable records into new WARC files:

```sh
mkdir -p repaired
warc convert warc --repair --output-dir repaired capture.warc.gz
warc validate --recursive repaired/
```

| Problem | Repair behavior |
| --- | --- |
| Missing `WARC-Record-ID` | Generates a record ID |
| Missing or incorrect WARC `Content-Length` | Sets the length from the readable record block |
| Missing or mismatched block/payload digests | Computes digests from the available bytes, where applicable |
| Missing HTTP end-of-headers separator | Adds the separator when the HTTP block can be parsed |
| Syntax errors in `application/warc-fields` blocks | Rewrites the fields recovered by the parser; unparseable fields may be lost |

Repair requires readable records. It cannot recover missing payload bytes,
reconstruct unreadable gzip data, retrieve missing revisit targets, or repair
HTML and other captured content. Recalculating a digest records the bytes
present; it does not restore the original content or prove it is intact.
Some validation errors may remain, so validate the output separately.
Keep the output directory separate from the input files.

### Search archived text

Search text extracts from Browsertrix WACZ files created with text extraction
enabled, or compatible WACZ files containing text extracts.

Start indexing, serving and full-text search:

```sh
warc serve --fulltext capture.wacz
```

Open **http://localhost:9999/console/** for search, snippets and the capture timeline.
Captures and text are indexed in the background. To index a directory tree or
serve an existing text index:

```sh
warc serve --recursive --fulltext archives/
warc serve --fulltext
```

Query the API from another terminal:

```sh
curl --get --data-urlencode 'q="northern lights"' http://localhost:9999/search
curl http://localhost:9999/search/status
```

[Full-text search and pywb configuration](docs/guides/search-and-replay.md)

### Manage collections through HTTP

```sh
warc serve --collections-dir ./collections
```

Create, populate and query a collection from another terminal:

```sh
curl --fail -X PUT http://localhost:9999/collections/news
curl --fail --data-binary @captures.cdxj http://localhost:9999/collections/news/cdx
curl 'http://localhost:9999/collections/news/cdx?url=https://example.com/'
curl --fail http://localhost:9999/collections/news/export -o news.cdxj
```

[Writable collections guide](docs/guides/writable-captures.md)

### Build and serve zipnum clusters

```sh
# Build a cluster from a CDXJ index
warc zipnum build captures.cdxj --output-dir cluster

# Add captures to a new cluster, preserving the original
warc zipnum add cluster additions.cdxj --output-dir enlarged

# Combine two clusters into a new cluster
warc zipnum merge cluster-a cluster-b --output-dir merged

# Serve capture lookups from a cluster
warc serve --index-format zipnum --zipnum-dir cluster
```

### Reorganize and deduplicate by time interval

```sh
# Audit revisit dependencies
warc redupe --audit --report audit.json --recursive sources/

# Resolve revisits and deduplicate within each year (default interval)
warc redupe --output-dir yearly --index-dir working-index \
  --report transformation.json --recursive sources/

# Build independently replayable monthly or daily collections
warc redupe --interval month --output-dir monthly --recursive sources/
warc redupe --interval day --output-dir daily --recursive sources/

# Generate a replay index for one year
warc index --index-out yearly/2025.cdxj yearly/2025/
```

[Time-based preservation guide](docs/guides/redupe.md)

## Documentation

```sh
warc help
warc --help
```

[Complete documentation](https://nationallibraryofnorway.github.io/warchaeology/)
