// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/ftpfs"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipfs"
	"github.com/spf13/afero"
	"golang.org/x/net/idna"
)

type fsOptions struct {
	ftpPoolSize int32
	tempDir     string
}

func WithTempDir(dir string) func(*fsOptions) { return func(o *fsOptions) { o.tempDir = dir } }

func WithFtpPoolSize(poolSize int32) func(*fsOptions) {
	return func(o *fsOptions) { o.ftpPoolSize = poolSize }
}

const (
	archiveTar = "tar"
	archiveTGZ = "tgz"
	archiveZIP = "zip"
)

var ErrUnsupportedFilesystem = errors.New("unsupported filesystem")
var schemeRegexp = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// ResolveFilesystem treats plain paths and archive-prefixed paths literally.
// Archive prefixes select the format; only FTP endpoints undergo URL parsing.
func ResolveFilesystem(fs afero.Fs, name string, options ...func(*fsOptions)) (_ afero.Fs, resultErr error) {
	opts := &fsOptions{ftpPoolSize: 1}
	for _, option := range options {
		option(opts)
	}
	kind := ""
	// A Windows drive path is a filename even when its separators resemble ://.
	drivePath := len(name) >= 3 && name[1] == ':' && (name[2] == '/' || name[2] == '\\')
	if schemeRegexp.MatchString(name) && !drivePath {
		scheme, locator, _ := strings.Cut(name, "://")
		kind = strings.ToLower(scheme)
		switch kind {
		case "ftp":
			u, err := parseFTPURL(name)
			if err != nil {
				return nil, err
			}
			addr, user, password, err := ftpEndpoint(u)
			if err != nil {
				return nil, err
			}
			if opts.ftpPoolSize <= 0 {
				return nil, errors.New("FTP pool size must be positive")
			}
			return ftpfs.New(addr, user, password, opts.ftpPoolSize)
		case archiveTar, archiveTGZ, archiveZIP:
			name = locator
		default:
			return nil, ErrUnsupportedFilesystem
		}
	} else {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".tar":
			kind = archiveTar
		case ".tgz":
			kind = archiveTGZ
		case ".wacz", ".zip":
			kind = archiveZIP
		case ".gz":
			if strings.HasSuffix(name, ".tar.gz") {
				kind = archiveTGZ
			}
		}
		if kind == "" {
			return fs, nil
		}
	}
	f, err := fs.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	if kind == archiveZIP {
		if fs.Name() == "ftpfs" {
			// A mounted FTP file otherwise reserves a pool slot until command shutdown,
			// preventing traversal of further inputs with the default one-slot pool.
			staged, err := stageArchive(f, opts.tempDir)
			if err != nil {
				return nil, err
			}
			f = staged
		}
		info, err := f.Stat()
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		r, err := zip.NewReader(&lockedReaderAt{reader: f}, info.Size())
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		mounted := zipfs.New(r)
		if strings.EqualFold(filepath.Ext(name), ".wacz") {
			mounted = zipfs.NewWACZ(r)
		}
		return &mountedFS{Fs: mounted, closer: f}, nil
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("archive %q: %w", name, resultErr)
		}
	}()
	if kind == archiveTGZ {
		r, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, r.Close()) }()
		mounted, err := readTar(r)
		if err != nil {
			return nil, err
		}
		// tar EOF can precede the gzip trailer; verify it before accepting the mount.
		if _, err := io.Copy(io.Discard, r); err != nil {
			return nil, fmt.Errorf("read gzip trailer: %w", err)
		}
		return mounted, nil
	}
	return readTar(f)
}

func parseFTPURL(name string) (*url.URL, error) {
	u, err := url.Parse(name)
	if err != nil {
		var escape url.EscapeError
		if errors.As(err, &escape) {
			return nil, errors.New("invalid URL escape in FTP endpoint")
		}
		return nil, errors.New("failed to parse FTP URL")
	}
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return nil, errors.New("FTP URL must select only an endpoint; supply file or directory paths as command arguments")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("FTP endpoint URL must not contain a query")
	}
	// net/url does not retain an empty fragment delimiter.
	if strings.Contains(name, "#") {
		return nil, errors.New("FTP endpoint URL must not contain a fragment")
	}
	return u, nil
}

func ftpEndpoint(u *url.URL) (addr, user, password string, err error) {
	host := u.Hostname()
	if host == "" {
		return "", "", "", errors.New("FTP hostname is required")
	}
	if strings.HasPrefix(u.Host, "[") {
		ip, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !ip.Is6() {
			return "", "", "", errors.New("invalid FTP IPv6 address")
		}
		host = ip.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid FTP hostname: %w", err)
		}
		host = strings.ToLower(host)
	}
	port := 21
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 0 || port > 65535 {
			return "", "", "", errors.New("invalid FTP port")
		}
	}
	addr = net.JoinHostPort(host, strconv.Itoa(port))
	if u.User != nil {
		// net/url has already decoded userinfo once, preserving literal '+'.
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	return addr, user, password, nil
}

// FTPSourceRoot produces the persisted endpoint and opaque account identifier.
func FTPSourceRoot(raw string) (string, error) {
	u, err := parseFTPURL(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "ftp") {
		return "", errors.New("invalid FTP endpoint")
	}
	addr, user, _, err := ftpEndpoint(u)
	if err != nil {
		return "", err
	}
	root := url.URL{Scheme: "ftp", Host: addr}
	if u.User != nil {
		root.Scheme = "ftp+account"
		root.User = url.User(fmt.Sprintf("%x", sha256.Sum256([]byte(user))))
	}
	return root.String(), nil
}

var ErrFTPCredentials = errors.New("authenticated FTP source requires matching ftp-endpoints credentials")

type ftpCredentialsKey struct{}

// WithFTPEndpoints binds credentials to this command and its request contexts.
func WithFTPEndpoints(ctx context.Context, endpoints []string) (context.Context, error) {
	credentials := make(map[string]string, len(endpoints))
	for _, raw := range endpoints {
		root, err := FTPSourceRoot(raw)
		if err != nil {
			return nil, err
		}
		credentials[root] = raw
	}
	return context.WithValue(ctx, ftpCredentialsKey{}, credentials), nil
}

func ResolveSourceRoot(ctx context.Context, fs afero.Fs, root string) (afero.Fs, error) {
	if strings.HasPrefix(root, "ftp+account://") {
		credentials, _ := ctx.Value(ftpCredentialsKey{}).(map[string]string)
		endpoint, ok := credentials[root]
		if !ok {
			return nil, ErrFTPCredentials
		}
		root = endpoint
	}
	return ResolveFilesystem(fs, root)
}

// mountedFS owns the backing reader, which must outlive all queued consumers.
type mountedFS struct {
	afero.Fs

	closer io.Closer
	once   sync.Once
	err    error
}

func (f *mountedFS) Close() error { f.once.Do(func() { f.err = f.closer.Close() }); return f.err }

// afero.File implementations (including MemMapFs and nested archive files) do
// not all provide concurrent ReadAt, which archive/zip requires of its backing store.
type lockedReaderAt struct {
	mu     sync.Mutex
	reader io.ReaderAt
}

func (r *lockedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reader.ReadAt(p, off)
}

// temporaryArchive owns both the staging file and its directory entry.
type temporaryArchive struct {
	*os.File

	once sync.Once
	err  error
}

func (f *temporaryArchive) Close() error {
	f.once.Do(func() { f.err = errors.Join(f.File.Close(), os.Remove(f.Name())) })
	return f.err
}
func stageArchive(source afero.File, dir string) (afero.File, error) {
	file, err := os.CreateTemp(dir, "warchaeology-archive-*")
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	staged := &temporaryArchive{File: file}
	_, copyErr := io.Copy(file, source)
	if err := errors.Join(copyErr, source.Close()); err != nil {
		return nil, errors.Join(err, staged.Close())
	}
	return staged, nil
}
