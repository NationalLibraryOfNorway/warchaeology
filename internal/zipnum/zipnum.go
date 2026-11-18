package zipnum

import (
	"fmt"
	"sync"
	"uuid"
)

// nameRetries bounds retries when the ID generator returns a reserved name.
const nameRetries = 100

// outputPart records a completed part and its blocks in logical record order.
type outputPart struct {
	recordBounds

	name   string
	blocks []blockIndex
}

// zipnum holds one build configuration and its ordered output manifest.
// The producer reserves names and manifest positions; workers fill those positions.
type zipnum struct {
	concurrency   int
	blocksPerPart int
	linesPerBlock int
	filter        func([]byte) bool
	getKey        func([]byte) []byte
	inputs        []string
	output        string
	newPartID     func() uuid.UUID

	mu       sync.Mutex
	reserved map[string]struct{}
	manifest []outputPart
}

// allocatePartName reserves a unique UUIDv7 name, retrying collisions.
func (z *zipnum) allocatePartName() (string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.reserved == nil {
		z.reserved = make(map[string]struct{})
	}
	gen := z.newPartID
	if gen == nil {
		gen = uuid.NewV7
	}
	for range nameRetries {
		id := gen()
		if id[6]>>4 != 7 || id[8]&0xc0 != 0x80 {
			return "", fmt.Errorf("part ID generator returned a non-v7 UUID: %s", id)
		}
		name := partPrefix + id.String()
		if _, exists := z.reserved[name]; exists {
			continue
		}
		z.reserved[name] = struct{}{}
		return name, nil
	}
	return "", fmt.Errorf("failed to allocate a unique part name after %d attempts", nameRetries)
}

// reserveReusableName keeps a source UUID name if no output already owns it.
func (z *zipnum) reserveReusableName(name string) bool {
	if !isV7PartName(name) {
		return false
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.reserved == nil {
		z.reserved = make(map[string]struct{})
	}
	if _, exists := z.reserved[name]; exists {
		return false
	}
	z.reserved[name] = struct{}{}
	return true
}

// appendManifest records completed parts in their logical order.
func (z *zipnum) appendManifest(parts ...outputPart) {
	z.mu.Lock()
	z.manifest = append(z.manifest, parts...)
	z.mu.Unlock()
}
