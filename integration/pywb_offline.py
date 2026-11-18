# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Replay POST revisits from an offline CDXJ export with stock pywb."""

import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import zipfile

from pywb.apps.frontendapp import FrontEndApp
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import WARCWriter
from werkzeug.test import Client
from werkzeug.wrappers import Response


def write_post(writer, url, capture, date):
    request = writer.create_warc_record(
        url, "request", payload=io.BytesIO(b"a=1"),
        http_headers=StatusAndHeaders(
            "/form HTTP/1.1",
            [("Content-Type", "application/x-www-form-urlencoded"), ("Content-Length", "3")],
            protocol="POST",
        ),
        warc_headers_dict={"WARC-Date": date, "WARC-Concurrent-To": capture.rec_headers.get_header("WARC-Record-ID")},
    )
    writer.write_record(request)


with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    donor_path, revisit_path = root / "original.warc.gz", root / "revisits.warc.gz"
    output = root / "index.cdxj"
    original_url = "https://example.org/form"
    original_date, revisit_date = "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"
    payload = b"Original POST payload"
    with donor_path.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        donor = writer.create_warc_record(
            original_url, "response", payload=io.BytesIO(payload),
            http_headers=StatusAndHeaders("200 OK", [("Content-Type", "text/plain")], protocol="HTTP/1.1"),
            warc_headers_dict={"WARC-Date": original_date},
        )
        writer.write_record(donor)
        write_post(writer, original_url, donor, original_date)

    # Cover both record-ID and URI/date/digest donor resolution.
    with revisit_path.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        headerless_url = "https://example.org/headerless"
        for url in (original_url, "https://example.org/other", headerless_url):
            headers = {"WARC-Date": revisit_date}
            if url == original_url:
                headers["WARC-Refers-To"] = donor.rec_headers.get_header("WARC-Record-ID")
            revisit = writer.create_revisit_record(
                url, donor.rec_headers.get_header("WARC-Payload-Digest"), original_url, original_date,
                http_headers=None if url == headerless_url else StatusAndHeaders(
                    "200 OK", [("Content-Type", "text/plain"), ("X-Capture", "revisit")], protocol="HTTP/1.1",
                ),
                warc_headers_dict=headers,
            )
            writer.write_record(revisit)
            write_post(writer, url, revisit, revisit_date)

    command = [os.environ["WARC_BINARY"], "index", "--pywb", "--post-append", "--record-type", "revisit",
               "--index-file", str(output), str(revisit_path)]
    subprocess.run([*command, str(donor_path)], check=True)
    original_index = output.read_bytes()
    rows = [line.split(b" ", 2) for line in original_index.splitlines()]
    assert len(rows) == 3
    for key, date, raw in rows:
        row = json.loads(raw)
        assert key.endswith(b"?__wb_method=post&a=1") and date == b"20260201000000"
        assert (row["mime"], row["filename"], row["orig.filename"], row["orig.offset"]) == (
            "text/plain", revisit_path.name, donor_path.name, "0",
        )

    cluster = root / "cluster"
    subprocess.run([os.environ["WARC_BINARY"], "zipnum", "build", str(output),
                    "--output-dir", str(cluster), "--lines", "1", "--blocks", "1"], check=True)
    zipnum = {"type": "zipnum", "path": str(cluster / "cluster.idx"),
              "shard_index_loc": {"match": "/$", "replace": "/parts/"}}
    for index in (str(output), zipnum):
        app = FrontEndApp(custom_config={"collections": {"test": {
            "index": index, "archive_paths": [str(root) + "/"],
        }}, "framed_replay": False})
        client = Client(app, Response)
        for _, _, raw in rows:
            response = client.post("/test/20260201000000id_/" + json.loads(raw)["url"],
                                   data="a=1", content_type="application/x-www-form-urlencoded")
            assert response.status_code == 200 and response.data == payload, (response.status, response.data)
            assert response.headers["Memento-Datetime"] == "Sun, 01 Feb 2026 00:00:00 GMT"
            if json.loads(raw)["url"] != headerless_url:
                assert response.headers["X-Capture"] == "revisit"

    package = root / "captures.wacz"
    with zipfile.ZipFile(package, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for path in (donor_path, revisit_path):
            archive.write(path, "archive/" + path.name)
    subprocess.run([*command[:-1], str(package)], check=True)
    assert output.read_bytes() == original_index

    result = subprocess.run(command, capture_output=True, text=True)
    assert result.returncode and "resolve pywb revisit" in result.stderr, result.stderr
    assert output.read_bytes() == original_index
    print("CDXJ and ZipNum replayed all POST revisits and preserved the prior index on failure.")
