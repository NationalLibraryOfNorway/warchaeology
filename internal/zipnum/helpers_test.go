package zipnum

import (
	"bytes"
	"testing"
)

func TestGetPartitionKey(t *testing.T) {
	tests := []struct {
		name string
		line []byte
		want []byte
	}{
		{
			"scheme prefix (dns)",
			[]byte("dns:0.gravatar.com timestamp json"),
			[]byte("dns"),
		},
		{
			"scheme prefix (filedesc)",
			[]byte("filedesc:file.warc.gz timestamp json"),
			[]byte("filedesc"),
		},
		{
			"IPv4",
			[]byte("1,1,168,192)/ timestamp json"),
			[]byte("ip"),
		},
		{
			"IPv6",
			[]byte("2001:0db8:0000:0000:0000:0000:0000:0001)/ timestamp json"),
			[]byte("ip"),
		},
		{
			"IPv6 loopback address",
			[]byte("::1)/ timestamp json"),
			[]byte("ip"),
		},
		{
			"TLD only (com)",
			[]byte("com)/robots.txt timestamp"),
			[]byte("com"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Must survive the line filter
			if !lineFilter(tt.line) {
				t.Fatalf("lineFilter returned false for line: %s", tt.line)
			}
			got := getPartitionKey(tt.line)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
