package wacz

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"os"
)

func Fingerprint(info os.FileInfo, z *zip.Reader) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d/%d", info.Size(), info.ModTime().UnixNano())
	for _, member := range z.File {
		if !member.FileInfo().IsDir() {
			_, _ = fmt.Fprintf(hash, "/%q/%d/%d", member.Name, member.CRC32, member.UncompressedSize64)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
