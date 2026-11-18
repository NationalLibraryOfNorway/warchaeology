package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd"
	"github.com/spf13/cobra/doc"
)

//go:generate go run ./gen_docs.go
func main() {
	// Find 'this' directory relative to this file to allow callers to be in any package
	var _, b, _, _ = runtime.Caller(0)
	var dir = filepath.Dir(b)
	var docsDir = filepath.Join(dir, "reference", "cli")

	genMdDoc(docsDir)
}

const fmTemplate = `---
title: "%s"
---
`

func genMdDoc(dir string) {
	fmt.Println("Generating documentation...")

	if files, err := os.ReadDir(dir); err == nil {
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "warc") {
				p := filepath.Join(dir, f.Name())
				_ = os.Remove(p)
			}
		}
	}

	c := cmd.NewWarcCommand()
	c.DisableAutoGenTag = true

	filePrepender := func(filename string) string {
		name := filepath.Base(filename)
		base := strings.TrimSuffix(name, filepath.Ext(name))
		return fmt.Sprintf(fmTemplate, strings.ReplaceAll(base, "_", " "))
	}

	linkHandler := func(name string) string {
		base := strings.TrimSuffix(name, filepath.Ext(name))
		return strings.ToLower(base) + ".md"
	}

	if err := doc.GenMarkdownTreeCustom(c, dir, filePrepender, linkHandler); err != nil {
		panic(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "warc") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			lines[i] = strings.TrimRight(strings.ReplaceAll(line, "\t", "    "), " ")
		}
		contents := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
		if err = os.WriteFile(path, []byte(contents), 0o644); err != nil {
			panic(err)
		}
	}

}
