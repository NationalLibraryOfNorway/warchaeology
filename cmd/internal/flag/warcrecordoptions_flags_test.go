// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/spf13/viper"
)

func TestURIValidationPolicy(t *testing.T) {
	t.Cleanup(viper.Reset)
	for _, mode := range []struct {
		name                 string
		strict, lenient, lax bool
	}{
		{name: "default"}, {name: "strict", strict: true}, {name: "lenient", lenient: true},
		{name: "lax", lax: true}, {name: "strict-lax", strict: true, lax: true}, {name: "strict-lenient", strict: true, lenient: true},
	} {
		for _, input := range []struct {
			uri     string
			invalid bool
		}{
			{uri: "HTTP://Example.COM:80/a/../%FC?q=a+b&x=%2f&x=&flag#fragment"},
			{uri: "urn:example:record"}, {uri: "https://blåbær.no/"},
			{uri: "https://example.org/%zz"}, // recoverable WHATWG issue, including in strict mode
			{uri: "foo://bad^host/", invalid: true},
		} {
			for _, field := range []string{gowarc.WarcTargetURI, gowarc.WarcRecordID, gowarc.WarcConcurrentTo} {
				t.Run(mode.name+"/"+field+"/"+input.uri, func(t *testing.T) {
					viper.Reset()
					viper.Set(StrictValidation, mode.strict)
					viper.Set(LenientValidation, mode.lenient)
					viper.Set(LaxHostParsing, mode.lax)
					viper.Set(TempDir, t.TempDir())
					value := input.uri
					if field != gowarc.WarcTargetURI {
						value = "<" + value + ">"
					}
					headers := map[string]string{
						gowarc.WarcType:      "resource",
						gowarc.WarcDate:      "2026-11-17T00:00:00Z",
						gowarc.ContentLength: "0",
						gowarc.WarcRecordID:  "<urn:example:original>",
						gowarc.WarcTargetURI: "https://example.org/",
					}
					headers[field] = value
					var raw strings.Builder
					raw.WriteString("WARC/1.1\r\n")
					for k, v := range headers {
						fmt.Fprintf(&raw, "%s: %s\r\n", k, v)
					}
					raw.WriteString("\r\n\r\n\r\n")
					rec, _, warnings, err := gowarc.NewUnmarshaler((WarcRecordOptionFlags{}).ToWarcRecordOptions()...).Unmarshal(bufio.NewReader(strings.NewReader(raw.String())))
					if rec != nil {
						defer testcheck.Close(t, rec)
					}
					invalid := input.invalid && !mode.lax
					if invalid && mode.strict {
						if err == nil {
							t.Fatal("strict mode accepted parse failure")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					wantWarnings := invalid && !mode.lenient
					if (len(warnings) > 0) != wantWarnings {
						t.Fatalf("warnings = %v, want warnings=%v", warnings, wantWarnings)
					}
					if got := rec.WarcHeader().Get(field); got != value {
						t.Fatalf("URI rewritten: %q -> %q", value, got)
					}
				})
			}
		}
	}
}
