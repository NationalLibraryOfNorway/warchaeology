# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""WARC -> CDXJ -> zipnum -> remote CDX -> pywb replay.
Run with: WARC_BINARY=/path/to/warc python integration/pywb_replay.py
Requires pywb 2.11.1. No pywb plugin or database conversion is used.
"""
import base64
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.request

from pywb.apps.frontendapp import FrontEndApp
from werkzeug.test import Client
from werkzeug.wrappers import Response

binary = os.environ["WARC_BINARY"]
fixtures = json.loads((Path(__file__).parents[1] / "cmd/index/testdata/reference.json").read_text())
with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp)
    fixture = next(f for f in fixtures if f["name"].endswith(".gz") and f["post"])
    warc = root / fixture["name"]
    warc.write_bytes(base64.b64decode(fixture["warc"]))
    cdx = root / "index.cdxj"
    subprocess.run([binary, "index", "--post-append", "--index-out", str(cdx), str(warc)], check=True)
    cluster = root / "cluster"
    subprocess.run([binary, "zipnum", "build", str(cdx), "--output-dir", str(cluster), "--lines", "1", "--blocks", "2"], check=True)
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    endpoint = f"http://127.0.0.1:{port}/cdx"
    backend = os.environ.get("WARC_TEST_BACKEND", "zipnum")
    command = [binary, "serve", "--address", f"127.0.0.1:{port}"]
    if backend == "collections":
        command += ["--collections-dir", str(root / "collections")]
        endpoint = f"http://127.0.0.1:{port}/collections/test/cdx"
    else:
        command += ["--index-format", "zipnum", "--zipnum-dir", str(cluster)]
    process = subprocess.Popen(command)
    try:
        for _ in range(100):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/ready") as response:
                    assert response.status == 200
                break
            except OSError:
                if process.poll() is not None:
                    raise RuntimeError("CDX server exited")
                time.sleep(.05)
        else:
            raise RuntimeError("CDX server did not start")
        if backend == "collections":
            with urllib.request.urlopen(urllib.request.Request(endpoint.removesuffix("/cdx"), method="PUT")) as response:
                assert response.status == 200
            with urllib.request.urlopen(urllib.request.Request(endpoint, data=cdx.read_bytes(), method="POST")) as response:
                assert response.status == 200
        app = FrontEndApp(custom_config={"collections": {"test": {"index": "cdx+" + endpoint, "archive_paths": [str(root) + "/"]}}, "framed_replay": False})
        client = Client(app, Response)
        for timestamp in ["20200102030405", "20210102030405"]:
            response = client.get(f"/test/{timestamp}id_/https://example.com/")
            assert response.status_code == 200, (response.status, response.data)
            assert b"Hello replay" in response.data, response.data
        # pywb's stock remote CDX source substitutes {url}, not its internal
        # alt_url. Exercise the POST-derived key explicitly through that stock
        # provider, without a custom index plugin.
        response = client.get("/test/20200102030405id_/https://example.com/form?__wb_method=POST&a=hello%20world&b=+")
        assert response.status_code == 201, (response.status, response.data)
        assert response.data == b"submitted", response.data
        acl = root / "blocked.aclj"
        acl.write_text('com,example)/ - {"access":"block","url":"https://example.com/"}\n')
        blocked = FrontEndApp(custom_config={"collections": {"test": {"index": "cdx+" + endpoint, "archive_paths": [str(root) + "/"], "acl_paths": [str(acl)]}}, "framed_replay": False})
        response = Client(blocked, Response).get("/test/20200102030405id_/https://example.com/")
        assert response.status_code in (403, 451), (response.status, response.data)
        print("pywb replay passed: normal capture, revisit, POST-derived key, pywb ACL", backend)
    finally:
        process.send_signal(signal.SIGINT)
        process.wait(timeout=35)
