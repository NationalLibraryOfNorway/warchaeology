# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Verify redupe -> index -> pywb replay with one output year at a time.

Run: WARC_BINARY=/path/to/warc python integration/redupe_replay.py
Requires pywb 2.11.1 (and its warcio dependency).
"""
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

from pywb.apps.frontendapp import FrontEndApp
from warcio.archiveiterator import ArchiveIterator
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import WARCWriter
from werkzeug.test import Client
from werkzeug.wrappers import Response

binary = os.environ["WARC_BINARY"]
payload = b"Preserved yearly payload.\n" * 300
url = "https://example.org/preserved"

with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp)
    source = root / "source.warc.gz"
    with source.open("wb") as stream:
        writer = WARCWriter(stream, gzip=True)
        previous_id = None
        previous_date = None
        for year in (2022, 2023, 2024):
            for month in (1, 6):
                date = f"{year}-{month:02d}-01T00:00:00Z"
                record_id = f"<urn:uuid:00000000-0000-0000-0000-{year:04d}{month:08d}>"
                headers = {
                    "WARC-Date": date,
                    "WARC-Record-ID": record_id,
                    "WARC-Payload-Digest": "sha256:" + hashlib.sha256(payload).hexdigest(),
                }
                record_type = "response" if previous_id is None else "revisit"
                if previous_id is not None:
                    headers.update({
                        "WARC-Profile": "http://netpreserve.org/warc/1.1/revisit/identical-payload-digest",
                        "WARC-Refers-To": previous_id,
                        "WARC-Refers-To-Target-URI": url,
                        "WARC-Refers-To-Date": previous_date,
                    })
                http = StatusAndHeaders("200 OK", [
                    ("Content-Type", "text/plain"),
                    ("Content-Length", str(len(payload))),
                    ("X-Capture", date),
                ], protocol="HTTP/1.1")
                record = writer.create_warc_record(
                    url, record_type, payload=io.BytesIO(payload if previous_id is None else b""),
                    http_headers=http, warc_headers_dict=headers,
                )
                writer.write_record(record)
                previous_id, previous_date = record_id, date

    # Lose the first member (the full donor) and retain its first direct revisit.
    surviving = root / "surviving"
    backup = root / "backup"
    repaired = root / "repaired"
    surviving.mkdir()
    backup.mkdir()
    with source.open("rb") as stream, source.open("rb") as raw:
        records = ArchiveIterator(stream)
        for record, dest in zip(records, (backup, surviving)):
            offset, size = records.get_record_offset(), records.get_record_length()
            raw.seek(offset)
            (dest / "capture.warc.gz").write_bytes(raw.read(size))
    subprocess.run([binary, "audit", str(surviving), "--repair-to", str(repaired),
                    "--recovery-source", str(backup), "--report", str(root / "recovery.json")],
                   check=True)
    shutil.rmtree(backup)
    repaired_cdx = root / "repaired.cdxj"
    subprocess.run([binary, "index", "--index-file", str(repaired_cdx), str(repaired)], check=True)
    app = FrontEndApp(custom_config={
        "collections": {"fixed": {"index": str(repaired_cdx), "archive_paths": [str(repaired) + "/"]}},
        "framed_replay": False,
    })
    response = Client(app, Response).get(f"/fixed/20220601000000id_/{url}")
    assert response.status_code == 200 and response.data == payload, response.status
    assert response.headers["X-Capture"] == "2022-06-01T00:00:00Z"

    yearly = root / "yearly"
    subprocess.run([
        binary, "redupe", "--output-dir", str(yearly), "--index-dir", str(root / "work"),
        "--report", str(root / "report.json"), str(source),
    ], check=True)
    report = json.loads((root / "report.json").read_text())
    assert all(y["completed"] for y in report["periods"].values()), report
    source.unlink()

    for year in (2022, 2023, 2024):
        isolated = root / f"isolated-{year}"
        shutil.move(str(yearly / str(year)), isolated)
        cdx = root / f"{year}.cdxj"
        subprocess.run([binary, "index", "--index-file", str(cdx), str(isolated)], check=True)
        app = FrontEndApp(custom_config={
            "collections": {"year": {"index": str(cdx), "archive_paths": [str(isolated) + "/"]}},
            "framed_replay": False,
        })
        client = Client(app, Response)
        for month in (1, 6):
            response = client.get(f"/year/{year}{month:02d}01000000id_/{url}")
            assert response.status_code == 200, (response.status, response.data)
            assert response.data == payload, response.data
            assert response.headers["X-Capture"] == f"{year}-{month:02d}-01T00:00:00Z"
        shutil.rmtree(isolated)
    print("redupe -> index -> pywb replay passed for three isolated years, including revisit chains")
