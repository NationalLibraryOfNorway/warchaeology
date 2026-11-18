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

func TestIngestValidationAndPostKeys(t *testing.T) {
	base := `ignored 20200102030405 {"url":"https://example.com/form","filename":"a","offset":9007199254740993,"length":"9223372036854775807","method":"POST","requestBody":"b=2&a=1","nested":{"n":9007199254740993}}`
	c, e := ParseIngestLine([]byte(base))
	if e != nil {
		t.Fatal(e)
	}
	q, e := ParseQuery(map[string][]string{"url": {"https://example.com/form"}, "method": {"POST"}, "requestBody": {"b=2&a=1"}})
	if e != nil || q.Key != c.GetSsu() {
		t.Fatalf("%s %s %v", q.Key, c.GetSsu(), e)
	}
	b, e := Line(c)
	if e != nil || !strings.Contains(string(b), "9007199254740993") {
		t.Fatal(string(b), e)
	}
	for _, line := range []string{strings.Replace(base, "20200102030405", "00000102030405", 1), strings.Replace(base, "20200102030405", "20200230030405", 1), strings.Replace(base, `"length":"9223372036854775807"`, `"length":"9223372036854775808"`, 1), strings.Replace(base, `"offset":9007199254740993`, `"offset":-1`, 1), strings.Replace(base, `"offset":9007199254740993`, `"offset":1.5`, 1), strings.Replace(base, `"method":"POST"`, `"method":"GET"`, 1), strings.Replace(base, "https://example.com/form", "http:///missing", 1), strings.Replace(base, "https://example.com/form", "not a url", 1), base + " {}", "com,example)/ 2020 http://example.com/ text/html 200 digest - - 100 0 a"} {
		if _, e = ParseIngestLine([]byte(line)); e == nil {
			t.Fatalf("accepted %s", line)
		}
	}
}

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
