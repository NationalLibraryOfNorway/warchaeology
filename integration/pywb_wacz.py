# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Stock pywb -> remote CDX -> HTTP byte ranges, with no WACZ awareness.

WARC_BINARY=/path/to/warc python integration/pywb_wacz.py
Requires unmodified pywb 2.11.1. Both services bind temporary loopback ports.
"""
import base64
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


binary = os.environ["WARC_BINARY"]
fixtures = json.loads((Path(__file__).parents[1] / "cmd/index/testdata/reference.json").read_text())
fixture = next(f for f in fixtures if f["name"].endswith(".gz") and f["post"])
warc_bytes = base64.b64decode(fixture["warc"])
leaf = package({"archive/" + fixture["name"]: warc_bytes, "indexes/index.cdxj": fixture["cdxj"], "datapackage.json": '{}'})
for mode, archive in [("leaf", leaf), ("nested", package({"child.wacz": leaf, "datapackage.json": json.dumps({"profile": "multi-wacz-package", "resources": [{"path": "child.wacz"}]})})), ("deflated", package({"child.wacz": leaf}, zipfile.ZIP_DEFLATED))]:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        source = root / "collection.wacz"
        source.write_bytes(archive)
        standalone = root / fixture["name"]
        standalone.write_bytes(warc_bytes)
        warc_port, pywb_port = port(), port()
        origin = f"http://127.0.0.1:{warc_port}/archive"
        frontend = f"http://127.0.0.1:{pywb_port}"
        config = {"collections": {"test": {"index": "cdx+" + origin + "/archives/cdx", "archive_paths": [origin + "/archives/"]}}, "framed_replay": False}
        (root / "config.yaml").write_text(json.dumps(config))
        with (root / "server.log").open("w+") as log:
            server = subprocess.Popen([binary, "serve", "--address", f"127.0.0.1:{warc_port}", "--path-prefix", "/archive", "--badger-dir", str(root / "db"), str(source), str(standalone)], stdout=log, stderr=log)
            pywb = None
            try:
                wait(origin + "/ready", server).close()
                for _ in range(100):
                    lines = urllib.request.urlopen(origin + "/archives/cdx?url=https://example.com/").read().splitlines()
                    if lines:
                        break
                    time.sleep(.05)
                capture = json.loads(lines[0].split(b" ", 2)[2])
                assert "wacz" not in capture["filename"] and str(root) not in capture["filename"], capture
                request = urllib.request.Request(origin + "/archives/" + capture["filename"], headers={"Range": f'bytes={capture["offset"]}-{int(capture["offset"])+int(capture["length"])-1}'})
                with urllib.request.urlopen(request) as response:
                    assert response.status == 206 and response.headers.get("Content-Encoding") is None
                    assert response.read() == warc_bytes[int(capture["offset"]):int(capture["offset"])+int(capture["length"])]
                pywb = subprocess.Popen([sys.executable, "-m", "pywb.apps.cli", "--port", str(pywb_port)], cwd=root, stdout=log, stderr=log)
                wait(frontend + "/", pywb).close()
                for timestamp in ["20200102030405", "20210102030405"]:
                    with urllib.request.urlopen(frontend + f"/test/{timestamp}id_/https://example.com/") as response:
                        assert response.status == 200 and b"Hello replay" in response.read()
                with urllib.request.urlopen(frontend + "/test/20200102030405id_/https://example.com/form?__wb_method=POST&a=hello%20world&b=+") as response:
                    assert response.status == 201 and response.read() == b"submitted"
                print("stock pywb passed:", mode, "CDX + exact WARC ranges + normal/revisit/POST-derived replay")
            except Exception:
                log.flush()
                log.seek(0)
                print(log.read(), file=sys.stderr)
                raise
            finally:
                for process in (pywb, server):
                    if process is not None and process.poll() is None:
                        process.send_signal(signal.SIGINT)
                        process.wait(timeout=35)
