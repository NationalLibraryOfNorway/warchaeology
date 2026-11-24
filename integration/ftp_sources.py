# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

"""Authenticated FTP source lifecycle. Requires pyftpdlib and WARC_BINARY."""
import base64
import contextlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import zipfile

from pyftpdlib.authorizers import DummyAuthorizer
from pyftpdlib.handlers import FTPHandler
from pyftpdlib.servers import FTPServer


binary = os.environ["WARC_BINARY"]
user, password = "fixture-account", "fixture-password"


def public(data):
    assert user.encode() not in data and password.encode() not in data
    return data


def command(*args, check=True):
    result = subprocess.run([binary, *args], capture_output=True, timeout=30)
    public(result.stdout + result.stderr)
    if check:
        assert result.returncode == 0, result.stderr
    return result


@contextlib.contextmanager
def serve(*args):
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    proc = subprocess.Popen([binary, "serve", "--address", f"127.0.0.1:{port}", *args])
    try:
        for _ in range(100):
            try:
                urllib.request.urlopen(origin + "/ready", timeout=1).close()
                break
            except OSError:
                assert proc.poll() is None
                time.sleep(.05)
        else:
            raise RuntimeError("serve timed out")
        yield origin
    finally:
        proc.send_signal(signal.SIGINT)
        proc.wait(timeout=15)


def request(origin, path, status=200, **kwargs):
    try:
        response = urllib.request.urlopen(urllib.request.Request(origin + path, **kwargs), timeout=10)
    except urllib.error.HTTPError as err:
        response = err
    with response:
        body = public(response.read())
        assert response.status == status, (path, response.status, body)
        return body


with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp)
    archive = root / "archives"
    archive.mkdir()
    fixtures = json.loads((Path(__file__).parents[1] / "cmd/index/testdata/reference.json").read_text())
    fixture = next(f for f in fixtures if f["name"].endswith(".gz") and f["post"])
    data = base64.b64decode(fixture["warc"])
    (archive / fixture["name"]).write_bytes(data)
    text = b"cloudberry"
    (archive / "text.warc").write_bytes(
        b"WARC/1.1\r\nWARC-Type: resource\r\n"
        b"WARC-Record-ID: <urn:uuid:1789da6c-6444-4f9a-b5ca-cb591e39713a>\r\n"
        b"WARC-Date: 2020-01-01T00:00:00Z\r\nWARC-Target-URI: urn:text:https://example.org/\r\n"
        b"Content-Type: text/plain\r\nContent-Length: 10\r\n\r\n" + text + b"\r\n\r\n"
    )
    package = archive / "collection.wacz"
    with zipfile.ZipFile(package, "w") as z:
        z.writestr("archive/" + fixture["name"], data)
        z.writestr("indexes/index.cdxj", fixture["cdxj"])
        z.writestr("pages/pages.jsonl", json.dumps({"url": "https://example.net/", "text": "snowberry", "ts": "2020-01-01T00:00:00Z"}))
        z.writestr("datapackage.json", "{}")
    authorizer = DummyAuthorizer()
    authorizer.add_user(user, password, str(archive))
    FTPHandler.authorizer = authorizer
    server = FTPServer(("127.0.0.1", 0), FTPHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        endpoint = f"ftp://{user}:{password}@127.0.0.1:{server.socket.getsockname()[1]}"
        db = ["--index-dir", str(root / "db")]
        index = ["index", "--index-format", "badger", *db, "--fulltext"]
        command(*index, "--ftp-pool-size", "1", "--input-file", endpoint,
                "/" + fixture["name"], "/collection.wacz", "/text.warc")
        config = root / "config.yaml"
        for wrong in [[], [endpoint.replace(user, "different")], [endpoint.replace("127.0.0.1", "localhost")]]:
            config.write_text(json.dumps({"ftp-endpoints": wrong}))
            result = command(*index, "--fulltext-rebuild", "--config", str(config), check=False)
            assert result.returncode and b"matching ftp-endpoints" in result.stderr, result.stderr
        config.write_text(json.dumps({"ftp-endpoints": [endpoint]}))
        command(*index, "--fulltext-rebuild", "--config", str(config))
        with serve(*db, "--config", str(config), "--allow-same-origin-replay", "--fulltext") as origin:
            request(origin, "/cdx", 403, method="POST", headers={"Origin": "https://other.example"})
            request(origin, "/warcserver/web/202099id_/https://example.com/", 400)
            request(origin, "/warcserver/web/202001020304050id_/https://example.com/", 400)
            assert b"Hello replay" in request(origin, "/warcserver/web/202001id_/https://example.com/")
            assert json.loads(request(origin, "/search?q=snowberry"))["hits"]
            assert json.loads(request(origin, "/search?q=cloudberry"))["hits"]
            request(origin, "/packages")
            request(origin, "/export")
            request(origin, "/backup")
            lines = request(origin, "/archives/cdx?url=https://example.com/").splitlines()
            archive_paths = []
            for line in lines:
                capture = json.loads(line.split(b" ", 2)[2])
                name = "/archives/" + capture["filename"]
                assert request(origin, name) == data
                archive_paths.append(name)
            # Only the standalone archive changes. The mounted member still serves.
            standalone = archive / fixture["name"]
            with standalone.open("ab") as f:
                f.write(b"changed")
            statuses = []
            for path in set(archive_paths):
                try:
                    urllib.request.urlopen(origin + path, timeout=10).close()
                    statuses.append(200)
                except urllib.error.HTTPError as err:
                    statuses.append(err.code)
                    public(err.read())
            assert sorted(statuses) == [200, 409], statuses
        with serve(*db) as origin:
            assert b"matching ftp-endpoints" in request(origin, name, 503)
        package.write_bytes(b"invalid zip")
        result = command(*index, "--config", str(config), "--fulltext-rebuild", check=False)
        assert result.returncode and b"zip" in result.stderr, result.stderr
        with serve("--collections-dir", str(root / "collections")) as origin:
            request(origin, "/collections/test", 403, method="PUT", headers={"Sec-Fetch-Site": "cross-site"})
            request(origin, "/collections/test", method="PUT")
    finally:
        server.close_all()
        thread.join(timeout=2)
print("authenticated FTP ingestion, restart, replay, full-text and error cleanup passed")
