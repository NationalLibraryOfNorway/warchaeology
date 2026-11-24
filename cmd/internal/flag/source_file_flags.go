// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"bufio"
	"fmt"
	"os"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	SrcFileSystem     = "input-file"
	SrcFileSystemHelp = `input filesystem source; default is the local OS filesystem
Legal values:
	/path/to/archive.( tar | tar.gz | tgz | zip | wacz )
	zip://LITERAL_PATH, tar://LITERAL_PATH, tgz://LITERAL_PATH (no URL decoding)
	ftp://user:password@host:port (endpoint only; optional trailing /)
FTP paths belong in command arguments; URL queries and fragments are not supported.
`
	SrcFileList     = "source-file-list"
	SrcFileListHelp = `path to a file listing input paths, one per line`

	FtpPoolSize     = "ftp-pool-size"
	FtpPoolSizeHelp = `size of the FTP connection pool`
)

type SrcFileListFlags struct {
}

func NewSrcFileListeFlags() SrcFileListFlags {
	return SrcFileListFlags{}
}

func (f SrcFileListFlags) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringP(SrcFileSystem, "i", "", SrcFileSystemHelp)
	flags.String(SrcFileList, "", SrcFileListHelp)
	flags.Int32(FtpPoolSize, 1, FtpPoolSizeHelp)
}

func (f SrcFileListFlags) SrcFilesystem() string {
	return viper.GetString(SrcFileSystem)
}

func (f SrcFileListFlags) SrcFileList() string {
	return viper.GetString(SrcFileList)
}

func (f SrcFileListFlags) FtpPoolSize() int32 {
	return viper.GetInt32(FtpPoolSize)
}

func (f SrcFileListFlags) ToFs() (afero.Fs, error) {
	return fs.ResolveFilesystem(afero.NewOsFs(), f.SrcFilesystem(), fs.WithFtpPoolSize(f.FtpPoolSize()), fs.WithTempDir(viper.GetString(TempDir)))
}

func ReadSrcFileList(name string) ([]string, error) {
	var paths []string

	if name == "" {
		return nil, nil
	}

	sourceFile, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("error opening file: %w", err)
	}
	defer func() {
		_ = sourceFile.Close()
	}()

	scanner := bufio.NewScanner(sourceFile)
	for scanner.Scan() {
		path := scanner.Text()
		paths = append(paths, path)

	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading from file: %w", err)
	}
	return paths, nil
}
