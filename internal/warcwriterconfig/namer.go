// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcwriterconfig

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

func NewIdentityNamer(path, filePrefix, dir string) gowarc.WarcFileNameGenerator {
	basename := filepath.Base(path)
	basename = strings.TrimSuffix(basename, ".gz")
	basename = strings.TrimSuffix(basename, ".arc")
	basename = strings.TrimSuffix(basename, ".warc")

	return &gowarc.PatternNameGenerator{
		Pattern:   "%{prefix}s" + basename + ".%{ext}s",
		Prefix:    filePrefix,
		Directory: dir,
	}
}

func NewNedlibNamer(path, filePrefix, dir string) gowarc.WarcFileNameGenerator {
	filename := filepath.Base(path)
	return &gowarc.PatternNameGenerator{
		Pattern:   "%{prefix}s" + filename + "-%04{serial}d-%{hostOrIp}s.%{ext}s",
		Prefix:    filePrefix,
		Directory: dir,
	}
}

// Share only the sequence, never the output directory or prefix of a prior run.
var defaultSerial atomic.Int32

type defaultNameGenerator struct{ prefix, directory string }

func (g defaultNameGenerator) NewWarcfileName() (string, string) {
	namer := &gowarc.PatternNameGenerator{Prefix: g.prefix, Directory: g.directory, Serial: defaultSerial.Add(1) - 1}
	return namer.NewWarcfileName()
}
func NewDefaultNamer(filePrefix, dir string) gowarc.WarcFileNameGenerator {
	return defaultNameGenerator{prefix: filePrefix, directory: dir}
}

func parseSubdirPattern(dirPattern string, t time.Time) string {
	p := strings.ReplaceAll(dirPattern, "{YYYY}", t.Format("2006"))
	p = strings.ReplaceAll(p, "{YY}", t.Format("06"))
	p = strings.ReplaceAll(p, "{MM}", t.Format("01"))
	p = strings.ReplaceAll(p, "{DD}", t.Format("02"))
	return p
}
