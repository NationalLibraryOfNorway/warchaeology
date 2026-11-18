package zipnum

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
)

var linkFile = os.Link

// linkOrCopy reuses immutable source files by hardlink when supported.
// Only cross-device and unsupported-link errors fall back to an exclusive copy.
func linkOrCopy(src, dst string) (err error) {
	if _, statErr := os.Lstat(dst); statErr == nil {
		return fmt.Errorf("destination already exists: %q", dst)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err = linkFile(src, dst); err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EOPNOTSUPP) && !errors.Is(err, syscall.ENOSYS) {
		return fmt.Errorf("hardlink %q to %q: %w", src, dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	info, err := in.Stat()
	if err != nil {
		_ = in.Close()
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		_ = in.Close()
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = syncFile(out)
	}
	if closeErr := closeIO(out); err == nil {
		err = closeErr
	}
	if closeErr := closeIO(in); err == nil {
		err = closeErr
	}
	return err
}

// rewriteIndexFile changes only the filename column when a part is renamed.
// Other field spellings are preserved, including in legacy indexes.
func rewriteIndexFile(src, dst, newPartName string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	info, err := in.Stat()
	if err != nil {
		_ = in.Close()
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		_ = in.Close()
		return err
	}
	w := bufio.NewWriter(out)
	err = readNormalizedLines(in, func(line []byte) error {
		fields := strings.Split(strings.TrimSuffix(string(line), "\n"), indexSeparator)
		if len(fields) != partIndexFields {
			return fmt.Errorf("malformed per-part index")
		}
		fields[1] = newPartName
		_, writeErr := w.WriteString(strings.Join(fields, indexSeparator) + "\n")
		return writeErr
	})
	if err == nil {
		err = flushBuffer(w)
	}
	if err == nil {
		err = syncFile(out)
	}
	if closeErr := closeIO(out); err == nil {
		err = closeErr
	}
	if closeErr := closeIO(in); err == nil {
		err = closeErr
	}
	return err
}

// checkOutputPath rejects an existing output before any work begins.
func checkOutputPath(output string) error {
	if _, statErr := os.Lstat(output); statErr == nil {
		return fmt.Errorf("output path already exists: %q", output)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect output path: %w", statErr)
	}
	return nil
}

// createStagingDirectory prepares a sibling directory for atomic publication.
// Failures remove the partial staging tree; on success the caller owns cleanup.
func createStagingDirectory(output string) (stage string, err error) {
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create output parent: %w", err)
	}
	stage, err = os.MkdirTemp(parent, "."+filepath.Base(output)+".tmp-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	if err = os.Mkdir(filepath.Join(stage, partDir), 0o755); err != nil {
		return stage, err
	}
	if err = os.Mkdir(filepath.Join(stage, indexDir), 0o755); err != nil {
		return stage, err
	}
	return stage, nil
}

// publishCluster syncs the staged tree and renames it to the final destination.
// The caller syncs the parent directory after the rename succeeds.
func publishCluster(stage, output string) error {
	for _, dir := range []string{filepath.Join(stage, partDir), filepath.Join(stage, indexDir), stage} {
		if err := publication.SyncDir(dir); err != nil {
			return err
		}
	}
	if err := publication.Rename(stage, output, false); err != nil {
		return fmt.Errorf("publish cluster: %w", err)
	}
	return nil
}
