package zipnum

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeVerificationCluster includes equal keys with different payloads, so using
// index keys instead of full record boundaries would change merge/add results.
func makeVerificationCluster(t *testing.T, dir string) (string, []string) {
	t.Helper()
	records := []string{"com,a)/ 1 {\"n\":1}", "com,a)/ 1 {\"n\":2}", "com,b)/ 1 {}", "com,c)/ 1 {}"}
	raw, root := filepath.Join(dir, "raw"), filepath.Join(dir, "base")
	writeRecords(t, raw, records, false)
	if err := (options{input: raw, output: root, concurrency: 2, linesPerBlock: 2, blocksPerPart: 2, newPartID: deterministicIDs(0)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return root, records
}

func TestVerifySourcesFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false}, {[]string{"-verify-sources"}, true}, {[]string{"-verify-sources=false"}, false},
	} {
		cli, err := parseCLI(tc.args, io.Discard)
		if err != nil || cli.options.verifySources != tc.want {
			t.Fatalf("parseCLI(%v) = %v, %v", tc.args, cli.options.verifySources, err)
		}
	}
	// The flag is harmless for raw creation.
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	writeRecords(t, raw, []string{"com,a)/ 1 {}"}, true)
	if err := runCLI(context.Background(), []string{"-in", raw, "-out", filepath.Join(dir, "out"), "-verify-sources"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestSourceVerificationPolicy(t *testing.T) {
	for _, corruption := range []string{"sidecar mismatch", "indexed range"} {
		for _, mode := range []string{"repartition", "merge", "add"} {
			t.Run(corruption+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				base, want := makeVerificationCluster(t, dir)
				m, err := readCluster(base)
				if err != nil {
					t.Fatal(err)
				}
				p := m.parts[0]
				sidecar := filepath.Join(base, indexDir, p.name)
				if corruption == "sidecar mismatch" {
					data, err := os.ReadFile(sidecar)
					if err != nil {
						t.Fatal(err)
					}
					data = bytes.Replace(data, []byte("com,a)/ 1"), []byte("com,wrong)/ 1"), 1)
					if err := os.WriteFile(sidecar, data, 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					// A section extending beyond EOF is usable for the last gzip member,
					// but strict range verification rejects it before processing.
					oldLength := p.blocks[len(p.blocks)-1].length
					for _, path := range []string{sidecar, filepath.Join(base, indexFile)} {
						data, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						old := fmt.Sprintf("\t%d\t%d", p.blocks[len(p.blocks)-1].offset, oldLength)
						replacement := fmt.Sprintf("\t%d\t%d", p.blocks[len(p.blocks)-1].offset, oldLength+100)
						if err := os.WriteFile(path, bytes.Replace(data, []byte(old), []byte(replacement), 1), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				empty := filepath.Join(dir, "empty")
				writeRecords(t, empty, nil, false)
				opts := options{input: base, concurrency: 1}
				switch mode {
				case "repartition":
					opts.repartition = true
				case "add":
					opts.add = empty
				case "merge":
					emptyCluster := filepath.Join(dir, "empty-cluster")
					if err := (options{input: empty, output: emptyCluster, concurrency: 1, linesPerBlock: 2, blocksPerPart: 2}).run(context.Background()); err != nil {
						t.Fatal(err)
					}
					opts.merge = true
					opts.input += "," + emptyCluster
				}
				opts.output = filepath.Join(dir, "verified")
				opts.verifySources = true
				if err := opts.run(context.Background()); err == nil {
					t.Fatal("verified operation accepted corrupt source")
				}
				if _, err := os.Stat(opts.output); !os.IsNotExist(err) {
					t.Fatalf("failed verification published output: %v", err)
				}
				opts.output = filepath.Join(dir, "trusted")
				opts.verifySources = false
				if err := opts.run(context.Background()); err != nil {
					t.Fatal(err)
				}
				// Default mode may preserve an inconsistent sidecar/range, so compare
				// actual records without invoking the strict reader used by other tests.
				result, err := readClusterManifest(opts.output)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, p := range result.parts {
					lines, err := readPartRecords(p)
					if err != nil {
						t.Fatal(err)
					}
					for _, line := range lines {
						got = append(got, strings.TrimSuffix(string(line), "\n"))
					}
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("records = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestRepartitionWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	root, want := makeVerificationCluster(t, dir)
	m, err := readClusterManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, indexDir, m.parts[0].name)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := runCLI(context.Background(), []string{"-in", root, "-out", out, "-repartition"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %q", got)
	}
	if err := runCLI(context.Background(), []string{"-in", root, "-out", filepath.Join(dir, "verified"), "-repartition", "-verify-sources"}, io.Discard); err == nil {
		t.Fatal("verified repartition accepted missing sidecar")
	}
}

func TestBoundaryReadsAreNotRepeated(t *testing.T) {
	dir := t.TempDir()
	base, _ := makeVerificationCluster(t, dir)
	empty := filepath.Join(dir, "empty")
	writeRecords(t, empty, nil, false)
	originalOpen := openPartFile
	t.Cleanup(func() { openPartFile = originalOpen })
	for _, verify := range []bool{false, true} {
		for _, mode := range []string{"create", "repartition", "add", "merge"} {
			t.Run(fmt.Sprintf("%s/verify=%v", mode, verify), func(t *testing.T) {
				opens := 0
				openPartFile = func(path string) (*os.File, error) {
					if !strings.HasPrefix(path, base+string(filepath.Separator)) {
						t.Errorf("unexpected boundary read, including staged output: %s", path)
					}
					opens++
					return originalOpen(path)
				}
				opts := options{input: base, output: filepath.Join(t.TempDir(), "out"), concurrency: 1, verifySources: verify}
				wantOpens := 1
				switch mode {
				case "create":
					opts.input = filepath.Join(dir, "raw")
					wantOpens = 0
				case "repartition":
					opts.repartition = true
					if !verify {
						wantOpens = 0
					}
				case "add":
					opts.add = empty
				case "merge":
					other := filepath.Join(t.TempDir(), "empty-cluster")
					if err := (options{input: empty, output: other, concurrency: 1, linesPerBlock: 2, blocksPerPart: 2}).run(context.Background()); err != nil {
						t.Fatal(err)
					}
					opts.merge = true
					opts.input += "," + other
				}
				if err := opts.run(context.Background()); err != nil {
					t.Fatal(err)
				}
				if opens != wantOpens {
					t.Fatalf("part opens = %d, want %d", opens, wantOpens)
				}
			})
		}
	}
}

func TestManifestLoadingDoesNotReadParts(t *testing.T) {
	dir := t.TempDir()
	root, _ := makeVerificationCluster(t, dir)
	if err := os.RemoveAll(filepath.Join(root, partDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, indexDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := (options{repartition: true}).readSourceCluster(root); err != nil {
		t.Fatal(err)
	}
	if _, err := (options{merge: true}).readSourceCluster(root); err == nil {
		t.Fatal("merge accepted missing boundary data")
	}
}

func TestBlockBoundaryModes(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	// A large first record also exercises records exceeding the line-reader buffer.
	first := "com,a)/ 1 {\"data\":\"" + strings.Repeat("x", 100000) + "\"}\n"
	last := "com,z)/ 1 {}\n"
	if _, err := io.WriteString(gz, first+last); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	block := blockIndex{length: int64(compressed.Len())}
	for _, mode := range []blockReadMode{firstRecord, blockBounds, verifiedBlockBounds} {
		bounds, err := readBlockBounds(bytes.NewReader(compressed.Bytes()), block, mode)
		if err != nil {
			t.Fatal(err)
		}
		wantLast := last
		if mode == firstRecord {
			wantLast = first
		}
		if string(bounds.first) != first || string(bounds.last) != wantLast {
			t.Fatalf("mode %d returned wrong boundaries", mode)
		}
	}
	// Retaining only boundaries must not remove strict in-block ordering checks.
	compressed.Reset()
	gz = gzip.NewWriter(&compressed)
	_, _ = io.WriteString(gz, last+first)
	_ = gz.Close()
	block.length = int64(compressed.Len())
	if _, err := readBlockBounds(bytes.NewReader(compressed.Bytes()), block, blockBounds); err != nil {
		t.Fatal(err)
	}
	if _, err := readBlockBounds(bytes.NewReader(compressed.Bytes()), block, verifiedBlockBounds); err == nil {
		t.Fatal("verified scan accepted unsorted block")
	}
	if err := readPartBoundaries(&sourcePart{name: "empty"}, false); err == nil {
		t.Fatal("part without blocks accepted")
	}
}

func TestOutputManifestValidation(t *testing.T) {
	id := deterministicIDs(0)().String()
	name := partPrefix + id
	good := outputPart{
		name:   name,
		blocks: []blockIndex{{key: "com,a)/ 1", file: name + partExt, length: 10}},
		first:  []byte("com,a)/ 1 {}\n"), last: []byte("com,b)/ 1 {}\n"),
	}
	for _, tc := range []struct {
		name   string
		change func(*outputPart)
	}{
		{"invalid name", func(p *outputPart) { p.name = "bad" }},
		{"no blocks", func(p *outputPart) { p.blocks = nil }},
		{"no boundary", func(p *outputPart) { p.first = nil }},
		{"backward bounds", func(p *outputPart) { p.first, p.last = p.last, p.first }},
		{"bad file", func(p *outputPart) { p.blocks[0].file = "wrong.gz" }},
		{"bad offset", func(p *outputPart) { p.blocks[0].offset = 1 }},
		{"bad length", func(p *outputPart) { p.blocks[0].length = 0 }},
		{"empty key", func(p *outputPart) { p.blocks[0].key = "" }},
		{"wrong key", func(p *outputPart) { p.blocks[0].key = "com,x)/ 1" }},
		{"unsorted keys", func(p *outputPart) {
			p.blocks = append(p.blocks, blockIndex{key: "com,0)/ 1", file: name + partExt, offset: 10, length: 10})
		}},
		{"overflow", func(p *outputPart) {
			p.blocks[0].length = math.MaxInt64
			p.blocks = append(p.blocks, blockIndex{key: "com,b)/ 1", file: name + partExt, offset: math.MaxInt64, length: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			p.blocks = append([]blockIndex(nil), good.blocks...)
			tc.change(&p)
			if err := (&zipnum{manifest: []outputPart{p}}).validateOutputManifest(); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
	if err := (&zipnum{manifest: []outputPart{good}}).validateOutputManifest(); err != nil {
		t.Fatal(err)
	}
	if err := (&zipnum{manifest: []outputPart{good, good}}).validateOutputManifest(); err == nil {
		t.Fatal("duplicate output part accepted")
	}
	second := good
	second.name = partPrefix + deterministicIDs(1)().String()
	second.blocks = append([]blockIndex(nil), good.blocks...)
	second.blocks[0].file = second.name + partExt
	if err := (&zipnum{manifest: []outputPart{good, second}}).validateOutputManifest(); err == nil {
		t.Fatal("globally unsorted output accepted")
	}
}

func TestVerifiedAndDefaultOutputsMatch(t *testing.T) {
	dir := t.TempDir()
	base, _ := makeVerificationCluster(t, dir)
	additions := filepath.Join(dir, "additions")
	writeRecords(t, additions, []string{"com,a)/ 1 {\"n\":2}", "com,a)/ 1 {\"n\":3}", "org,z)/ 1 {}"}, true)
	other := filepath.Join(dir, "other")
	createCluster(t, additions, other, deterministicIDs(100))
	for _, mode := range []string{"repartition", "merge", "add"} {
		t.Run(mode, func(t *testing.T) {
			outputs := []string{filepath.Join(t.TempDir(), "default"), filepath.Join(t.TempDir(), "verified")}
			for i, out := range outputs {
				opts := options{input: base, output: out, concurrency: 2, verifySources: i == 1, newPartID: deterministicIDs(200)}
				switch mode {
				case "repartition":
					opts.repartition = true
				case "merge":
					opts.merge = true
					opts.input += "," + other
				case "add":
					opts.add = additions
				}
				if err := opts.run(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := readCluster(out); err != nil {
					t.Fatal(err)
				}
			}
			for _, root := range outputs {
				// Compare both directions so extra files are detected too.
				peer := outputs[0]
				if root == peer {
					peer = outputs[1]
				}
				err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() || entry.Name() == metaFile {
						return nil
					}
					relative, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					other, err := os.ReadFile(filepath.Join(peer, relative))
					if err != nil {
						return err
					}
					if !bytes.Equal(data, other) {
						t.Errorf("output differs: %s", relative)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err := readMeta(filepath.Join(outputs[0], metaFile))
			if err != nil {
				t.Fatal(err)
			}
			b, err := readMeta(filepath.Join(outputs[1], metaFile))
			if err != nil {
				t.Fatal(err)
			}
			a.CreatedAt, a.Duration, b.CreatedAt, b.Duration = "", "", "", ""
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Fatalf("metadata differs: %v, %v", a, b)
			}
		})
	}
}
