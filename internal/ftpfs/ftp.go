// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ftpfs

import (
	"context"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/jackc/puddle"
	"github.com/jlaffaye/ftp"
	"github.com/spf13/afero"
)

// Fs is an implementation of afero.Fs that utilizes functions from the ftp package.
//
// For detailed information on any method, please refer to the documentation of the ftp
// package available at github.com/jlaffaye/ftp.
type Fs struct {
	pool *puddle.Pool
}

func New(addr, user, passwd string, poolSize int32) afero.Fs {
	constructor := func(ctx context.Context) (any, error) {
		return connect(func() (connection, error) {
			c, err := ftp.Dial(addr, ftp.DialWithContext(ctx))
			if err != nil {
				return nil, err
			}
			return serverConnection{c}, nil
		}, user, passwd)
	}

	destructor := func(value any) {
		_ = value.(connection).Quit()
	}

	pool := puddle.NewPool(constructor, destructor, poolSize)
	return &Fs{
		pool: pool,
	}
}

func (s Fs) Name() string { return "ftpfs" }

func (s Fs) Create(name string) (afero.File, error) {
	return nil, syscall.EROFS
}

func (s Fs) Mkdir(name string, perm os.FileMode) error {
	return syscall.EROFS
}

func (s Fs) MkdirAll(path string, perm os.FileMode) error {
	return syscall.EROFS
}

func (s Fs) Open(name string) (afero.File, error) {
	res, err := s.pool.Acquire(context.Background())
	if err != nil {
		return nil, err
	}
	return FileOpen(res, name)
}

func (s Fs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	if flag == os.O_RDONLY {
		return s.Open(name)
	}
	return nil, syscall.EROFS
}

func (s Fs) Remove(name string) error {
	return syscall.EROFS
}

func (s Fs) RemoveAll(path string) error {
	return syscall.EROFS
}

func (s Fs) Rename(oldname, newname string) error {
	return syscall.EROFS
}

func (s Fs) Stat(name string) (os.FileInfo, error) {
	f, err := s.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}

func (s Fs) Chmod(name string, mode os.FileMode) error {
	return syscall.EROFS
}

func (s Fs) Chown(name string, uid, gid int) error {
	return syscall.EROFS
}

func (s Fs) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return syscall.EROFS
}

// connection isolates the protocol transport for deterministic contract tests.
type connection interface {
	Login(string, string) error
	Quit() error
	GetEntry(string) (*ftp.Entry, error)
	List(string) ([]*ftp.Entry, error)
	retrieve(string, uint64) (io.ReadCloser, error)
}
type serverConnection struct{ *ftp.ServerConn }

func (c serverConnection) retrieve(name string, offset uint64) (io.ReadCloser, error) {
	return c.RetrFrom(name, offset)
}
func connect(dial func() (connection, error), user, password string) (connection, error) {
	c, err := dial()
	if err != nil {
		return nil, err
	}
	if err := c.Login(user, password); err != nil {
		_ = c.Quit()
		return nil, err
	}
	return c, nil
}

// Close waits for borrowed connections to be returned before shutting down.
func (s Fs) Close() error { s.pool.Close(); return nil }
