package zipnum

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"uuid"
)

// Cluster files use tab-separated indexes and concatenated gzip members.
// Each part index has four fields; cluster.idx adds a one-based sequence number.
const (
	indexFile          = "cluster.idx"
	metaFile           = "cluster.json"
	indexDir           = "idx"
	partDir            = "parts"
	partPrefix         = "part-"
	partFilePattern    = partPrefix + "*"
	partExt            = ".gz"
	partIndexFields    = 4
	clusterIndexFields = 5
	indexSeparator     = "\t"
	partIndexFormat    = "%s\t%s\t%d\t%d\n"
	clusterIndexFormat = "%s\t%s\t%d\t%d\t%d\n"
)

// blockIndex locates one gzip member by byte range in a part file.
type blockIndex struct {
	key    string
	file   string
	offset int64
	length int64
}

// isV7PartName recognizes the UUIDv7 names used for newly written parts.
func isV7PartName(name string) bool {
	if len(name) <= len(partPrefix) || name[:len(partPrefix)] != partPrefix {
		return false
	}
	id, err := uuid.Parse(name[len(partPrefix):])
	return err == nil && id[6]>>4 == 7 && id[8]&0xc0 == 0x80
}

// validSourcePartName also accepts legacy numeric part names when reading.
func validSourcePartName(name string) bool {
	if isV7PartName(name) {
		return true
	}
	if !strings.HasPrefix(name, partPrefix) || len(name) == len(partPrefix) {
		return false
	}
	for _, r := range name[len(partPrefix):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseIndexFields validates a tab-separated index row without accessing disk.
// Cluster rows include a positive sequence number; part rows return sequence zero.
func parseIndexFields(line []byte, cluster bool) (blockIndex, int, error) {
	want := partIndexFields
	if cluster {
		want = clusterIndexFields
	}
	fields := strings.Split(strings.TrimRight(string(line), "\r\n"), indexSeparator)
	if len(fields) != want {
		return blockIndex{}, 0, fmt.Errorf("index entry has %d fields, want %d", len(fields), want)
	}
	if fields[0] == "" {
		return blockIndex{}, 0, fmt.Errorf("empty index key")
	}
	filename := fields[1]
	if filepath.Base(filename) != filename || !strings.HasSuffix(filename, partExt) {
		return blockIndex{}, 0, fmt.Errorf("unsafe part filename %q", filename)
	}
	name := strings.TrimSuffix(filename, partExt)
	if !validSourcePartName(name) {
		return blockIndex{}, 0, fmt.Errorf("invalid part filename %q", filename)
	}
	offset, err := parseInt64("offset", fields[2])
	if err != nil {
		return blockIndex{}, 0, err
	}
	if offset < 0 {
		return blockIndex{}, 0, fmt.Errorf("offset must not be negative")
	}
	length, err := parseInt64("length", fields[3])
	if err != nil {
		return blockIndex{}, 0, err
	}
	if length <= 0 {
		return blockIndex{}, 0, fmt.Errorf("length must be positive")
	}
	seq := 0
	if cluster {
		seq64, parseErr := strconv.ParseInt(fields[4], 10, 32)
		if parseErr != nil || seq64 <= 0 {
			return blockIndex{}, 0, fmt.Errorf("invalid sequence %q", fields[4])
		}
		seq = int(seq64)
	}
	return blockIndex{key: fields[0], file: filename, offset: offset, length: length}, seq, nil
}

// parseInt64 adds the index field name to numeric parsing errors.
func parseInt64(field, value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	return n, nil
}

// cdxIndexKey returns the URL key and timestamp, the first two CDXJ fields.
func cdxIndexKey(line []byte) (string, error) {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	first := bytes.IndexByte(line, ' ')
	if first <= 0 {
		return "", fmt.Errorf("invalid CDXJ record %q", line)
	}
	secondRel := bytes.IndexByte(line[first+1:], ' ')
	if secondRel <= 0 {
		return "", fmt.Errorf("invalid CDXJ record %q", line)
	}
	return string(line[:first+1+secondRel]), nil
}
