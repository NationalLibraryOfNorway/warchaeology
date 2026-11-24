# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Stock pywb -> remote CDX -> HTTP byte ranges, with no WACZ awareness.

WARC_BINARY=/path/to/warc python integration/pywb_wacz.py
Requires unmodified pywb 2.11.1. Both services bind temporary loopback ports.
"""
import base64
import gzip
import io
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import zipfile

from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import WARCWriter


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait(url, process):
    for _ in range(150):
        try:
            return urllib.request.urlopen(url, timeout=1)
        except OSError:
            if process.poll() is not None:
                raise RuntimeError("server exited")
            time.sleep(.1)
    raise RuntimeError("server did not become ready")


def package(entries, method=zipfile.ZIP_STORED):
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w", compression=method) as z:
        for name, data in entries.items():
            z.writestr(name, data)
    return output.getvalue()


def cross_url_revisit():
    """An embedded index with a slash-preserving donor key, as written by Browsertrix."""
    archive = io.BytesIO()
    writer = WARCWriter(archive, gzip=True)
    original = "https://example.com/original/"
    date = "2020-01-02T03:04:05Z"
    http = StatusAndHeaders("200 OK", [("Content-Type", "text/plain")], protocol="HTTP/1.1")
    donor = writer.create_warc_record(original, "response", payload=io.BytesIO(b"Cross-URL donor"),
                                      http_headers=http, warc_headers_dict={"WARC-Date": date})
    writer.write_record(donor)
    boundary = archive.tell()
    digest = donor.rec_headers.get_header("WARC-Payload-Digest")
    revisit_url = "https://example.com/revisit"
    revisit = writer.create_revisit_record(revisit_url, digest, original, date, http_headers=http,
                                           warc_headers_dict={"WARC-Date": "2021-01-02T03:04:05Z"})
    writer.write_record(revisit)
    rows = []
    for path, timestamp, mime, offset, length in [
        ("original/", "20200102030405", "text/plain", 0, boundary),
        ("revisit", "20210102030405", "warc/revisit", boundary, archive.tell() - boundary),
    ]:
        fields = dict(url="https://example.com/" + path, mime=mime, status="200", digest=digest,
                      filename="cross-url.warc.gz", offset=str(offset), length=str(length))
        rows.append(f"com,example)/{path} {timestamp} {json.dumps(fields)}")
    # A pre-resolved CDXJ row makes pywb fetch orig.filename directly.
    resolved_url = "https://example.com/resolved"
    offset = archive.tell()
    resolved = writer.create_revisit_record(resolved_url, digest, original, date, http_headers=http,
                                            warc_headers_dict={"WARC-Date": "2021-01-02T03:04:05Z"})
    writer.write_record(resolved)
    fields = dict(url=resolved_url, mime="text/plain", status="200", digest=digest,
                  filename="cross-url.warc.gz", offset=str(offset), length=str(archive.tell() - offset))
    fields.update({"orig.filename": "cross-url.warc.gz", "orig.offset": "0", "orig.length": str(boundary)})
    rows.append(f"com,example)/resolved 20210102030405 {json.dumps(fields)}")
    # Both spellings exist at the same instant, with different HTTP responses.
    for path, status in [("page/", "308 Permanent Redirect"), ("page", "200 OK")]:
        url = "https://example.com/" + path
        offset = archive.tell()
        headers = [("Location", "https://example.com/page")] if path.endswith("/") else []
        record = writer.create_warc_record(url, "response", payload=io.BytesIO(b""),
                                           http_headers=StatusAndHeaders(status, headers, protocol="HTTP/1.1"),
                                           warc_headers_dict={"WARC-Date": date})
        writer.write_record(record)
        fields = dict(url=url, status=status.split()[0], filename="cross-url.warc.gz",
                      offset=str(offset), length=str(archive.tell() - offset))
        rows.append(f"com,example)/{path} 20200102030405 {json.dumps(fields)}")
    return archive.getvalue(), rows


binary = os.environ["WARC_BINARY"]
fixtures = json.loads((Path(__file__).parents[1] / "cmd/index/testdata/reference.json").read_text())
fixture = next(f for f in fixtures if f["name"].endswith(".gz") and f["post"])
warc_bytes = base64.b64decode(fixture["warc"])
cross_warc, cross_rows = cross_url_revisit()
members = {"archive/" + fixture["name"]: warc_bytes, "archive/cross-url.warc.gz": cross_warc}
cdx_text = "\n".join(sorted(fixture["cdxj"].splitlines() + cross_rows)) + "\n"
leaf = package({**members, "indexes/index.cdxj": cdx_text, "datapackage.json": '{}'})
compressed = gzip.compress(cdx_text.encode())
summary = '!meta 0 {"format":"cdxj-gzip-1.0","filename":"index.cdx.gz"}\n'
first_key = cdx_text.split(" {", 1)[0]
summary += first_key + " " + json.dumps({"offset": 0, "length": len(compressed)}) + "\n"
seekable = package({**members,
                    "indexes/index.cdx.gz": compressed, "indexes/index.idx": summary})
for mode, archive in [("direct", leaf), ("direct-seek", seekable), ("warc-scan", leaf), ("leaf", leaf), ("nested", package({"child.wacz": leaf, "datapackage.json": json.dumps({"profile": "multi-wacz-package", "resources": [{"path": "child.wacz"}]})})), ("deflated", package({"child.wacz": leaf}, zipfile.ZIP_DEFLATED))]:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        source = root / "collection.wacz"
        source.write_bytes(archive)
        direct = mode.startswith("direct")
        if direct:
            source.chmod(0o444)
        standalone = root / fixture["name"]
        standalone.write_bytes(warc_bytes)
        warc_port, pywb_port = port(), port()
        origin = f"http://127.0.0.1:{warc_port}/archive"
        frontend = f"http://127.0.0.1:{pywb_port}"
        config = {"collections": {"test": {"index": "cdx+" + origin + "/archives/cdx", "archive_paths": [origin + "/archives/"]}}, "framed_replay": False}
        (root / "config.yaml").write_text(json.dumps(config))
        with (root / "server.log").open("w+") as log:
            command = [binary, "serve", "--address", f"127.0.0.1:{warc_port}", "--path-prefix", "/archive"]
            if direct:
                command += ["--wacz-index", "direct", "--read-only", str(source)]
            else:
                command += ["--index-dir", str(root / "db"), str(source), str(standalone)]
            if mode == "warc-scan":
                command += ["--wacz-index", "warc", "--post-append"]
            server = subprocess.Popen(command, stdout=log, stderr=log,
                                      env={**os.environ, "XDG_DATA_HOME": str(root / "data")})
            pywb = None
            try:
                wait(origin + "/ready", server).close()
                for _ in range(100):
                    with urllib.request.urlopen(origin + "/archives/cdx?url=https://example.com/") as response:
                        captures = [json.loads(line.split(b" ", 2)[2]) for line in response.read().splitlines()]
                    if len({capture["filename"] for capture in captures}) == (1 if direct else 2):
                        break
                    time.sleep(.05)
                else:
                    raise AssertionError("expected the configured WARC sources")
                for capture in captures:
                    assert "wacz" not in capture["filename"] and str(root) not in capture["filename"], capture
                    offset, length = int(capture["offset"]), int(capture["length"])
                    request = urllib.request.Request(
                        origin + "/archives/" + capture["filename"],
                        headers={"Range": f"bytes={offset}-{offset + length - 1}"},
                    )
                    with urllib.request.urlopen(request) as response:
                        assert response.status == 206 and response.headers.get("Content-Encoding") is None
                        assert response.read() == warc_bytes[offset:offset + length]
                if mode != "warc-scan":
                    for path, expected in [("page/", ["308"]), ("page", ["200"]), ("page/&filter==status:200", [])]:
                        query = "/archives/cdx?output=json&sort=closest&closest=20200102030405&url=https://example.com/"
                        with urllib.request.urlopen(origin + query + path) as response:
                            assert [json.loads(line)["status"] for line in response] == expected
                pywb = subprocess.Popen([sys.executable, "-m", "pywb.apps.cli", "--port", str(pywb_port)], cwd=root, stdout=log, stderr=log)
                wait(frontend + "/", pywb).close()
                for timestamp in ["20200102030405", "20210102030405"]:
                    with urllib.request.urlopen(frontend + f"/test/{timestamp}id_/https://example.com/") as response:
                        assert response.status == 200 and b"Hello replay" in response.read()
                with urllib.request.urlopen(frontend + "/test/20200102030405id_/https://example.com/form?__wb_method=POST&a=hello%20world&b=+") as response:
                    assert response.status == 201 and response.read() == b"submitted"
                with urllib.request.urlopen(frontend + "/test/20210102030405id_/https://example.com/revisit") as response:
                    assert response.status == 200 and response.read() == b"Cross-URL donor"
                with urllib.request.urlopen(frontend + "/test/20210102030405id_/https://example.com/resolved") as response:
                    assert response.status == 200 and response.read() == b"Cross-URL donor"
                if direct:
                    assert not (root / "data").exists(), "direct mode created a data directory"
                print("stock pywb passed:", mode, "CDX + exact WARC ranges + normal/revisit/POST-derived replay")
            except Exception:
                log.flush()
                log.seek(0)
                print(log.read(), file=sys.stderr)
                raise
            finally:
                server.send_signal(signal.SIGINT)
                try:
                    if pywb is not None and pywb.poll() is None:
                        pywb.terminate()
                        try:
                            pywb.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            pywb.kill()
                            pywb.wait(timeout=5)
                finally:
                    server.wait(timeout=35)
