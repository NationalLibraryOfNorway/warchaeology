package warc

import (
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

func TestHostnamePreservesURI(t *testing.T) {
	for _, tt := range []struct{ uri, host string }{
		{"HTTP://Example.COM:80/a/../%FC?q=+&q=%2F#x", "example.com"},
		{"https://blåbær.no/", "xn--blbr-roah.no"},
		{"http://127.1/", "127.0.0.1"}, {"http://[2001:db8::1]:8080/", "[2001:db8::1]"},
		{"urn:example:record", ""}, {"relative/path", ""}, {"foo://bad^host/", ""},
	} {
		t.Run(tt.uri, func(t *testing.T) {
			rec := newTestRecord(t, gowarc.Resource, tt.uri, "text/plain")
			defer rec.Close()
			if got := Hostname(rec); got != tt.host {
				t.Errorf("hostname = %q, want %q", got, tt.host)
			}
			if got := URL(rec); got != tt.uri {
				t.Errorf("URI changed to %q", got)
			}
		})
	}
}
func TestRequestMIMEType(t *testing.T) {
	b := gowarc.NewRecordBuilder(gowarc.Request)
	b.AddWarcHeader(gowarc.ContentType, "application/http;msgtype=request")
	b.AddWarcHeader(gowarc.WarcTargetURI, "https://example.org/")
	if _, err := b.WriteString("POST / HTTP/1.1\r\nHost: example.org\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatal(err)
	}
	rec, _, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if got := MIMEType(rec); got != "application/json" {
		t.Fatalf("MIME type = %q", got)
	}
}
