# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Hydrate stock pywb CDXJ and replay the result with pywb 2.11.1."""

import gzip
from importlib.metadata import version
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
from unittest.mock import patch

from pywb.apps.frontendapp import FrontEndApp
from pywb.indexer.cdxindexer import write_cdx_index
from pywb.warcserver.index.aggregator import BaseAggregator
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import WARCWriter
from werkzeug.test import Client
from werkzeug.wrappers import Response


BINARY = os.environ["WARC_BINARY"]
ORIGINAL_DATE = "2026-01-01T00:00:00.123Z"
REVISIT_DATE = "2026-02-01T00:00:00Z"
MONTH = "20260201000000"
DONOR_URL = "https://example.org/form"
BODY = b"Verified POST payload"


def run(*args, success=True):
    result = subprocess.run([BINARY, *map(str, args)], capture_output=True, text=True)
    assert (result.returncode == 0) == success, (args, result.stdout, result.stderr)
    return result.stderr


def response(writer, date, payload=BODY):
    record = writer.create_warc_record(
        DONOR_URL, "response", payload=io.BytesIO(payload),
        http_headers=StatusAndHeaders("200 OK", [("Content-Type", "text/plain"), ("X-Capture", "donor")],
                                      protocol="HTTP/1.1"),
        warc_headers_dict={"WARC-Date": date},
    )
    writer.write_record(record)
    write_post(writer, DONOR_URL, record, date)
    return record


def revisit(writer, url, donor, *, date=REVISIT_DATE, id_only=False, headerless=False, digest=None, redirect=False):
    headers = {"WARC-Date": date}
    if id_only:
        headers["WARC-Refers-To"] = donor.rec_headers.get_header("WARC-Record-ID")
    http = None
    if not headerless:
        if redirect:
            http = StatusAndHeaders("302 Found", [("Location", "https://example.org/new")], protocol="HTTP/1.1")
        else:
            http = StatusAndHeaders("200 OK", [("Content-Type", "text/plain"), ("X-Capture", url.rsplit("/", 1)[1])],
                                    protocol="HTTP/1.1")
    record = writer.create_revisit_record(
        url, digest or donor.rec_headers.get_header("WARC-Payload-Digest"),
        donor.rec_headers.get_header("WARC-Target-URI"), donor.rec_headers.get_header("WARC-Date"),
        http_headers=http, warc_headers_dict=headers,
    )
    if id_only:
        for key in ("WARC-Refers-To-Target-URI", "WARC-Refers-To-Date"):
            record.rec_headers.remove_header(key)
    writer.write_record(record)
    write_post(writer, url, record, date)
    return record


def write_post(writer, url, capture, date):
    request = writer.create_warc_record(
        url, "request", payload=io.BytesIO(b"a=1"),
        http_headers=StatusAndHeaders(
            "/form HTTP/1.1", [("Content-Type", "application/x-www-form-urlencoded"), ("Content-Length", "3")],
            protocol="POST",
        ),
        warc_headers_dict={"WARC-Date": date, "WARC-Concurrent-To": capture.rec_headers.get_header("WARC-Record-ID")},
    )
    writer.write_record(request)


def indexed_rows(path, root):
    output = io.BytesIO()
    with path.open("rb") as warc:
        write_cdx_index(output, warc, path.relative_to(root).as_posix(), cdxj=True, append_post=True)
    return [decode(row) for row in output.getvalue().decode().splitlines()]


def decode(row):
    key, stamp, fields = row.split(" ", 2)
    return key, stamp, json.loads(fields)


def encode(row):
    key, stamp, fields = row
    return f"{key} {stamp} {json.dumps(fields, separators=(',', ':'))}\n"


def write_rows(path, rows):
    path.write_text("".join(map(encode, rows)))


def replay(root, index, names):
    calls = []
    query = BaseAggregator.__call__

    def counted(source, params):
        calls.append(params["url"])
        return query(source, params)

    app = FrontEndApp(custom_config={"collections": {"test": {
        "index": index, "archive_paths": [str(root) + "/"],
    }}, "framed_replay": False})
    client = Client(app, Response)
    try:
        with patch.object(BaseAggregator, "__call__", counted):
            for name in names:
                calls.clear()
                url = "https://example.org/" + name
                resp = client.post(f"/test/{MONTH}id_/{url}", data="a=1",
                                   content_type="application/x-www-form-urlencoded")
                assert calls == [url], (name, calls, resp.status, resp.data)
                assert resp.headers["Memento-Datetime"] == "Sun, 01 Feb 2026 00:00:00 GMT"
                if name == "redirect":
                    assert resp.status_code == 302 and resp.data == b"", (resp.status, resp.data)
                else:
                    assert resp.status_code == 200 and resp.data == BODY, (resp.status, resp.data)
                    assert resp.headers["X-Capture"] == ("donor" if name == "headerless" else name)
    finally:
        app.warcserver_server.stop()


def test_hydration(root):
    (root / "nested").mkdir()
    donor_path = root / "nested/donor.warc.gz"
    revisit_path, ambiguous_path = root / "revisits.warc.gz", root / "ambiguous.warc.gz"
    with donor_path.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        donor = response(writer, ORIGINAL_DATE)
        response(writer, "2026-01-01T00:00:00.456Z", b"A separate capture in the same second")
    with ambiguous_path.open("wb") as stream:
        response(WARCWriter(stream, gzip=True), ORIGINAL_DATE)
    with revisit_path.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        cross = revisit(writer, "https://example.org/cross", donor)
        revisit(writer, "https://example.org/chain", cross)
        revisit(writer, "https://example.org/id-only", donor, id_only=True)
        revisit(writer, "https://example.org/headerless", donor, headerless=True)
        revisit(writer, "https://example.org/redirect", donor, redirect=True)
        revisit(writer, "https://example.org/mismatch", donor, digest="sha1:" + "A" * 32)

    donors = indexed_rows(donor_path, root)
    assert len(donors) == 2 and donors[0][1] == donors[1][1]
    revisits = indexed_rows(revisit_path, root)
    mismatch = revisits.pop()
    for key, _, fields in [*revisits, *donors, mismatch]:
        assert key.endswith("?__wb_method=post&a=1"), key
        fields["custom"] = {"precise": 9007199254740993, "array": [1, None]}
    # Donors appear after their dependants, and the same row occurs twice.
    rows = [*revisits, revisits[0], *donors]
    raw = root / "input.cdxj"
    write_rows(raw, rows)
    original = raw.read_bytes()
    hydrate = ("index", "hydrate")
    cdxj = root / "hydrated.cdxj"
    log = run(*hydrate, raw, "--warc-root", root, "--index-file", cdxj)
    assert "resolved=5 already-hydrated=0 skipped=3 unresolved=0" in log, log
    expected_output = cdxj.read_bytes()
    output = list(map(decode, cdxj.read_text().splitlines()))
    assert len(output) == len(rows)
    for before, after in zip(rows, output):
        assert before[:2] == after[:2]
        assert all(after[2][k] == v for k, v in before[2].items() if k != "mime")
        if before[2]["url"].endswith("/redirect") or before in donors:
            assert before == after
        else:
            assert tuple(after[2][key] for key in ("mime", "orig.filename", "orig.offset", "orig.length")) == (
                "text/plain", "nested/donor.warc.gz", "0", donors[0][2]["length"],
            )

    gzip_input, gzip_output = root / "input.cdxj.gz", root / "hydrated.cdxj.gz"
    gzip_input.write_bytes(gzip.compress(original))
    run(*hydrate, gzip_input, "--warc-root", root, "--index-file", gzip_output)
    assert gzip.decompress(gzip_output.read_bytes()) == expected_output
    repeat = root / "repeat.cdxj"
    log = run(*hydrate, gzip_output, "--warc-root", root, "--index-file", repeat)
    assert "resolved=0 already-hydrated=5 skipped=3 unresolved=0" in log, log
    assert repeat.read_bytes() == expected_output and raw.read_bytes() == original

    # A stale original is repaired while a verified copy and numeric offset survive.
    equivalent_name = "equivalent.warc.gz"
    (root / equivalent_name).write_bytes(donor_path.read_bytes())
    retained_fields = dict(output[0][2], **{"orig.filename": equivalent_name, "orig.offset": 0})
    stale_fields = dict(output[1][2], **{"orig.length": "1"})
    edited = root / "edited.cdxj"
    write_rows(edited, [(output[0][0], output[0][1], retained_fields),
                        (output[1][0], output[1][1], stale_fields), *output[2:]])
    log = run(*hydrate, edited, "--warc-root", root, "--index-file", repeat)
    assert "resolved=1 already-hydrated=4" in log, log
    repaired = list(map(decode, repeat.read_text().splitlines()))
    assert repaired[0][2] == retained_fields and repaired[1] == output[1]

    sorted_raw = root / "sorted.cdxj"
    sorted_raw.write_text("".join(sorted(map(encode, rows))))
    source, cluster, converted = root / "cluster", root / "cluster-pywb", root / "converted"
    run("zipnum", "build", sorted_raw, "--lines", 1, "--blocks", 1, "--output-dir", source)
    source_summary = (source / "cluster.idx").read_bytes()
    run(*hydrate, source, "--warc-root", root, "--output-dir", cluster)
    run(*hydrate, gzip_input, "--warc-root", root, "--output-dir", converted)
    assert len(list((cluster / "parts").iterdir())) == len(rows)
    manifest = json.loads((cluster / "cluster.json").read_text())
    assert (manifest["lines_per_block"], manifest["blocks_per_part"], manifest["source"]) == (1, 1, [str(source)])
    assert (source / "cluster.idx").read_bytes() == source_summary
    for directory in (cluster, converted):
        run(*hydrate, directory, "--warc-root", root, "--index-file", repeat)
        assert repeat.read_text().splitlines() == sorted(cdxj.read_text().splitlines())
    names = ["cross", "chain", "id-only", "headerless", "redirect"]
    # Stock pywb binary-searches standalone indexes by key and timestamp.
    sorted_hydrated = root / "sorted-hydrated.cdxj"
    run(*hydrate, sorted_raw, "--warc-root", root, "--index-file", sorted_hydrated)
    replay(root, str(sorted_hydrated), names)
    replay(root, {"type": "zipnum", "path": str(cluster / "cluster.idx"),
                  "shard_index_loc": {"match": "/$", "replace": "/parts/"}}, names)

    catalogue, no_donors = root / "catalogue", root / "no-donors.cdxj"
    run("index", "--index-format", "badger", "--index-dir", catalogue,
        "--post-append", donor_path)
    write_rows(no_donors, revisits)
    log = run(*hydrate, no_donors, "--warc-root", root, "--donor-index-dir", catalogue, "--index-file", repeat)
    assert "resolved=4 already-hydrated=0 skipped=1 unresolved=0" in log, log
    assert all(row[2].get("orig.filename") == "nested/donor.warc.gz"
               for row in list(map(decode, repeat.read_text().splitlines()))[:-1])

    plain_warc = root / "plain.warc"
    with plain_warc.open("wb") as stream:
        writer = WARCWriter(stream, gzip=False)
        original = response(writer, ORIGINAL_DATE)
        revisit(writer, "https://example.org/plain", original, id_only=True)
    plain_index = root / "plain.cdxj"
    write_rows(plain_index, indexed_rows(plain_warc, root))
    run(*hydrate, plain_index, "--warc-root", root, "--index-file", repeat)
    assert decode(repeat.read_text().splitlines()[1])[2]["orig.filename"] == plain_warc.name

    ambiguous = indexed_rows(ambiguous_path, root)[0]
    for label, bad_rows, extras, reason in [
        ("missing", [revisits[0]], [], "not found"),
        ("ambiguous", [revisits[0]], [*donors, ambiguous], "ambiguous"),
        ("digest", [mismatch], donors, "digest mismatch"),
        ("inaccessible", [revisits[0]], [(donors[0][0], donors[0][1], dict(donors[0][2], filename="missing.warc"))],
         "no such file"),
    ]:
        broken = root / f"{label}.cdxj"
        # The redirect is always copied. For the ambiguous/digest cases, also
        # verify that an ID-only reference can still be resolved in partial mode.
        fixture = [revisits[4], *bad_rows, *extras]
        if label in {"ambiguous", "digest"}:
            fixture.append(revisits[2])
        write_rows(broken, fixture)
        repeat.write_bytes(b"previous output")
        log = run(*hydrate, broken, "--warc-root", root, "--index-file", repeat, success=False)
        assert "unresolved=1" in log and reason in log.lower() and "offset=" in log, log
        assert repeat.read_bytes() == b"previous output"
        log = run(*hydrate, broken, "--warc-root", root, "--index-file", repeat, "--allow-unresolved")
        assert "unresolved=1" in log and repeat.read_text().splitlines()[1] == encode(bad_rows[0]).strip(), log
        if label in {"ambiguous", "digest"}:
            assert decode(repeat.read_text().splitlines()[-1])[2]["orig.filename"] == "nested/donor.warc.gz"
        assert not list(root.glob(".warc-index-*"))

    cycle = root / "cycle.warc.gz"
    cycle_url = "https://example.org/cycle"
    with cycle.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        record = writer.create_revisit_record(
            cycle_url, donor.rec_headers.get_header("WARC-Payload-Digest"), cycle_url, REVISIT_DATE,
            warc_headers_dict={"WARC-Date": REVISIT_DATE},
        )
        writer.write_record(record)
    cycle_index = root / "cycle.cdxj"
    write_rows(cycle_index, indexed_rows(cycle, root))
    failed_cluster = root / "failed-cluster"
    log = run(*hydrate, cycle_index, "--warc-root", root, "--output-dir", failed_cluster, success=False)
    assert "cycle or chain too deep" in log and not failed_cluster.exists(), log
    run(*hydrate, cycle_index, "--warc-root", root, "--output-dir", failed_cluster, "--allow-unresolved")
    log = run(*hydrate, failed_cluster, "--warc-root", root, "--index-file", repeat, "--allow-unresolved")
    assert "unresolved=1" in log and repeat.read_bytes() == cycle_index.read_bytes(), log


if __name__ == "__main__":
    assert version("pywb") == "2.11.1"
    with tempfile.TemporaryDirectory() as directory:
        test_hydration(Path(directory))
    print("Hydrated CDXJ and ZipNum both replayed with one CDX query per capture.")
