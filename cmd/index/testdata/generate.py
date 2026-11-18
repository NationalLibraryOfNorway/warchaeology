"""Regenerate with cdxj-indexer a65917b12fbef23e58ee3e66878b852923811fb6.
Run this script with warcio 1.8.2 and the pinned indexer installed.
"""
import base64
import io
import json
import re
from pathlib import Path
from tempfile import TemporaryDirectory
from warcio.warcwriter import WARCWriter
from warcio.statusandheaders import StatusAndHeaders
from cdxj_indexer.main import CDXJIndexer
from cdxj_indexer.postquery import query_extract

root = Path(__file__).parent
fixtures = []
for compressed in [False, True]:
    out = io.BytesIO()
    writer = WARCWriter(out, gzip=compressed)
    def record(kind, url, body, http=None, extra=None):
        h = {"WARC-Date": "2020-01-02T03:04:05Z", **(extra or {})}
        r = writer.create_warc_record(url, kind, payload=io.BytesIO(body), http_headers=http, warc_headers_dict=h)
        writer.write_record(r)
        return r
    record("response", "https://example.com/", b"<html><body>Hello replay</body></html>", StatusAndHeaders("200 OK", [("Content-Type", "text/html; charset=utf-8")], protocol="HTTP/1.1"), {"WARC-Record-ID": "<urn:uuid:00000000-0000-4000-8000-000000000001>"})
    record("revisit", "https://example.com/", b"", StatusAndHeaders("200 OK", [("Content-Type", "text/html")], protocol="HTTP/1.1"), {"WARC-Date": "2021-01-02T03:04:05Z", "WARC-Refers-To": "<urn:uuid:00000000-0000-4000-8000-000000000001>", "WARC-Refers-To-Target-URI": "https://example.com/", "WARC-Refers-To-Date": "2020-01-02T03:04:05Z", "WARC-Payload-Digest": "sha1:CE7LXI7QO3CFBH2ZOSPQK3BZF72TQY6N", "WARC-Profile": "http://netpreserve.org/warc/1.1/revisit/identical-payload-digest"})
    req = record("request", "https://example.com/form", b"a=hello+world&b=%2B", StatusAndHeaders("/form HTTP/1.1", [("Content-Type", "application/x-www-form-urlencoded"), ("Content-Length", "19")], protocol="POST"))
    record("response", "https://example.com/form", b"submitted", StatusAndHeaders("201 Created", [("Content-Type", "text/plain")], protocol="HTTP/1.1"), {"WARC-Concurrent-To": req.rec_headers.get_header("WARC-Record-ID")})
    record("resource", "https://example.com/resource", b"resource", extra={"Content-Type": "text/plain"})
    record("metadata", "https://example.com/metadata", b"metadata", extra={"Content-Type": "application/warc-fields"})
    with TemporaryDirectory() as tmp:
        name = "captures.warc" + (".gz" if compressed else "")
        path = Path(tmp) / name
        path.write_bytes(out.getvalue())
        for post in [False, True]:
            output = io.StringIO()
            CDXJIndexer(output, [str(path)], post_append=post).process_all()
            fixtures.append({"name": name, "post": post, "warc": base64.b64encode(out.getvalue()).decode(), "cdxj": output.getvalue()})
# Missing digests must be computed from payload bytes, not the whole WARC block.
raw = base64.b64decode(fixtures[0]["warc"])
raw = re.sub(br"WARC-Payload-Digest:[^\r\n]*\r\n", b"", raw)
with TemporaryDirectory() as tmp:
    path = Path(tmp) / "missing-digests.warc"
    path.write_bytes(raw)
    output = io.StringIO()
    CDXJIndexer(output, [str(path)]).process_all()
    fixtures.append({"name": path.name, "post": False, "warc": base64.b64encode(raw).decode(), "cdxj": output.getvalue()})
(root / "reference.json").write_text(json.dumps(fixtures, indent=2) + "\n")

cases = [
    ("form", "application/x-www-form-urlencoded", b"a=hello+world&b=%2B", None),
    ("binary", "application/octet-stream", b"\x00\xffhello", None),
    ("json", "application/json", b'{"a":1,"nested":{"a":true,"b":null},"values":[1,2]}', None),
    ("duplicate-json-name", "application/json", b'{"b":0,"a":{"x":1},"c":2,"a":3}', None),
    ("invalid-json-utf8", "application/json", b'{"a":"\xff"}', None),
    ("invalid-text-utf8", "text/plain", b'{"a":"\xff"}', None),
    ("floats", "application/json", b'{"a":1.0,"b":1e20,"c":1e6,"d":1e-7}', None),
    ("json-lines", "application/json", b'{"a":1}\n{"a":2}', None),
    ("invalid-json", "application/json", b'bad json', None),
    ("text", "text/plain", b'hello there', None),
    ("short", "application/octet-stream", b'abcdef', "3"),
    ("invalid-length", "application/octet-stream", b'abcdef', "invalid"),
    ("negative-length", "text/plain", b'abcdef', "-1"),
    ("long", "application/x-www-form-urlencoded", b'a=' + b'x'*5000, "5002"),
    ("multipart", "multipart/form-data; boundary=X", b'--X\r\nContent-Disposition: form-data; name="text"\r\n\r\na b\r\n--X\r\nContent-Disposition: form-data; name="upload"; filename="a.bin"\r\nContent-Type: application/octet-stream\r\n\r\n\x00\xff+\r\n--X--\r\n', None),
]
post = []
for name, mime, body, length in cases:
    post.append({"name": name, "mime": mime, "body": base64.b64encode(body).decode(), "length": length or "", "query": query_extract(mime, length, io.BytesIO(body), "http://example.com/") or ""})
(root / "post.json").write_text(json.dumps(post, indent=2) + "\n")

import surt
urls = ["http://010.000.000.001/", "http://example.com:99999/", "http://example.com\\path", "https://bücher.example/", "https://EXAMPLE.com:443/a%2fb?B=2&a=1#frag", "dns:Example.COM", "http://example.com/?a=&b=2"]
canonical = []
for url in urls:
    try:
        reference = surt.surt(url)
    except Exception:
        reference = url
    canonical.append({"url": url, "reference": reference})
(root / "surt.json").write_text(json.dumps(canonical, ensure_ascii=False, indent=2) + "\n")
