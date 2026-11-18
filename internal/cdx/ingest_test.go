package index

import (
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

func TestBinaryRequestPrefix(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 3059, 3060, 3061, 3062, 3063, 3064, 1 << 20} {
		body := strings.Repeat("x", n)
		got, err := RequestQuery("application/octet-stream", strconv.Itoa(n), strings.NewReader(body))
		expected := "__wb_post_data=" + base64.StdEncoding.EncodeToString([]byte(body))
		if err != nil || got != expected[:min(len(expected), 4096)] {
			t.Fatalf("size %d: wrong key, error %v", n, err)
		}
	}
	failure := errors.New("body error")
	_, err := RequestQuery("application/octet-stream", "10000", io.MultiReader(strings.NewReader(strings.Repeat("x", 5000)), iotest.ErrReader(failure)))
	if !errors.Is(err, failure) {
		t.Fatalf("lost trailing read error: %v", err)
	}
}

func BenchmarkBinaryRequestQuery(b *testing.B) {
	for _, size := range []int{4096, 16 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			body := strings.Repeat("x", size)
			length := strconv.Itoa(size)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := RequestQuery("application/octet-stream", length, strings.NewReader(body)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
