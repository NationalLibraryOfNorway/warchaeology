package zipnum

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sourcePart describes an indexed part and its first and last records.
type sourcePart struct {
	recordBounds

	root   string
	name   string
	blocks []blockIndex
}

// clusterManifest keeps parts in cluster.idx sequence order, not filename order.
type clusterManifest struct {
	root  string
	meta  meta
	parts []sourcePart
}

// readClusterManifest parses metadata and index structure without opening parts.
func readClusterManifest(root string) (clusterManifest, error) {
	m := clusterManifest{root: root}
	var err error
	m.meta, err = readMeta(filepath.Join(root, metaFile))
	if err != nil {
		return m, fmt.Errorf("read cluster metadata: %w", err)
	}
	if m.meta.LinesPerBlock <= 0 || m.meta.BlocksPerPart <= 0 {
		return m, fmt.Errorf("cluster metadata has a nonpositive layout")
	}
	var current *sourcePart
	seen := make(map[string]struct{})
	expectedSeq := 1
	var previousKey string
	err = readInputLines(filepath.Join(root, indexFile), func(line []byte) error {
		b, seq, err := parseIndexFields(line, true)
		if err != nil {
			return err
		}
		if seq != expectedSeq {
			return fmt.Errorf("invalid sequence %d, want %d", seq, expectedSeq)
		}
		expectedSeq++
		name := strings.TrimSuffix(b.file, partExt)
		if current == nil || current.name != name {
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("part %q is not contiguous in cluster.idx", name)
			}
			seen[name] = struct{}{}
			m.parts = append(m.parts, sourcePart{root: root, name: name})
			current = &m.parts[len(m.parts)-1]
			previousKey = ""
		}
		if previousKey != "" && b.key < previousKey {
			return fmt.Errorf("part %q index keys are not sorted", name)
		}
		previousKey = b.key
		current.blocks = append(current.blocks, b)
		return nil
	})
	if err != nil {
		return m, fmt.Errorf("read cluster index: %w", err)
	}

	return m, nil
}

// readCluster verifies a source cluster and establishes its record boundaries.
// Verification checks boundary blocks, not the contents of every block.
func readCluster(root string) (clusterManifest, error) {
	m, err := readClusterManifest(root)
	if err != nil {
		return m, err
	}
	var previousLast []byte
	for i := range m.parts {
		p := &m.parts[i]
		if err := validateSourcePart(p); err != nil {
			return m, err
		}
		if len(previousLast) > 0 && bytes.Compare(previousLast, p.first) > 0 {
			return m, fmt.Errorf("cluster parts are not globally sorted at %q", p.name)
		}
		previousLast = append(previousLast[:0], p.last...)
	}
	return m, nil
}

// validateSourcePart checks that both indexes agree and cover the entire file.
// It then reads boundary blocks to populate the part range used by merge and add.
func validateSourcePart(p *sourcePart) error {
	if len(p.blocks) == 0 {
		return fmt.Errorf("part %q has no blocks", p.name)
	}
	partPath := filepath.Join(p.root, partDir, p.name+partExt)
	info, err := os.Stat(partPath)
	if err != nil {
		return fmt.Errorf("stat part %q: %w", p.name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("part %q is not a regular file", p.name)
	}
	var diskBlocks []blockIndex
	err = readInputLines(filepath.Join(p.root, indexDir, p.name), func(line []byte) error {
		b, _, err := parseIndexFields(line, false)
		if err != nil {
			return err
		}
		diskBlocks = append(diskBlocks, b)
		return nil
	})
	if err != nil {
		return fmt.Errorf("read index for %q: %w", p.name, err)
	}
	if len(diskBlocks) != len(p.blocks) {
		return fmt.Errorf("per-part index for %q disagrees with cluster.idx", p.name)
	}
	var end int64
	for i, b := range p.blocks {
		if diskBlocks[i] != b {
			return fmt.Errorf("per-part index for %q disagrees with cluster.idx", p.name)
		}
		if b.offset != end || b.length > info.Size()-b.offset {
			return fmt.Errorf("invalid byte range for %q", p.name)
		}
		end = b.offset + b.length
	}
	if end != info.Size() {
		return fmt.Errorf("indexed ranges do not cover part %q", p.name)
	}
	return readPartBoundaries(p, true)
}
