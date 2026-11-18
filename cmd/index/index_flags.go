package index

import (
	"fmt"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type IndexFlags struct {
	CaptureDatabaseFlags  flag.CaptureDatabaseFlags
	FileWalkerFlags       flag.FileWalkerFlags
	FilterFlags           flag.FilterFlags
	WarcIteratorFlags     flag.WarcIteratorFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	ErrorFlags            flag.ErrorFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	InputHookFlags        *flag.InputHookFlags
}

func NewIndexFlags() IndexFlags { return IndexFlags{InputHookFlags: &flag.InputHookFlags{}} }

func (f IndexFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd)
	f.FilterFlags.AddFlags(cmd)
	f.WarcIteratorFlags.AddFlags(cmd)
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.CaptureDatabaseFlags.AddFlags(cmd)
	cmd.Flags().String("wacz-index", "auto", "WACZ indexing: auto imports embedded indexes; warc indexes contained WARCs")
	cmd.Flags().String("index-format", "cdxj", "cdxj, cdxpb or badger")
	cmd.Flags().String("index-out", "-", "output filename or - for stdout")
	cmd.Flags().String("index-tmp-dir", "", "temporary sort directory")
	cmd.Flags().Bool("sort", false, "sort and deduplicate stdout CDXJ")
	cmd.Flags().String("dir-root", "", "export filenames relative to this directory")
	cmd.Flags().Bool("post-append", false, "include paired request method and body in capture keys")
}

func (f IndexFlags) ToOptions() (*IndexOptions, error) {
	parser := f.WarcRecordOptionFlags
	o := &IndexOptions{
		WaczConfig:        fmt.Sprintf("strict=%t;lenient=%t;lax-host-parsing=%t", parser.StrictValidation(), parser.LenientValidation(), parser.LaxHostParsing()),
		WaczIndex:         viper.GetString("wacz-index"),
		Concurrency:       f.ConcurrencyFlags.Concurrency(),
		Force:             f.WarcIteratorFlags.Force(),
		ContinueOnError:   f.ErrorFlags.ContinueOnError(),
		WarcRecordOptions: parser.ToWarcRecordOptions(),
		PostAppend:        viper.GetBool("post-append"),
		DirRoot:           viper.GetString("dir-root"),
		Offset:            f.WarcIteratorFlags.Offset(),
		Nth:               f.WarcIteratorFlags.RecordNum(),
		Limit:             f.WarcIteratorFlags.Limit(),
	}
	o.database = f.CaptureDatabaseFlags.ToOptions()
	o.format = viper.GetString("index-format")
	o.output = viper.GetString("index-out")
	o.tmpDir = viper.GetString("index-tmp-dir")
	o.sort = viper.GetBool("sort")
	var e error
	o.Paths, e = flag.ReadSrcFileList(f.FileWalkerFlags.SrcFileListFlags.SrcFileList())
	if e != nil {
		return nil, e
	}
	o.Filter, e = f.FilterFlags.ToFilter()
	if e != nil {
		return nil, e
	}
	if len(f.FilterFlags.RecordType()) == 0 {
		o.Filter.RecordTypes = gowarc.Response | gowarc.Revisit | gowarc.Resource | gowarc.Metadata
		o.DefaultRecords = true
	}
	o.OpenInputFileHook, e = f.InputHookFlags.ToOpenInputFileHook()
	if e != nil {
		return nil, e
	}
	o.CloseInputFileHook, e = f.InputHookFlags.ToCloseInputFileHook()
	if e != nil {
		return nil, e
	}
	o.FileWalker, e = f.FileWalkerFlags.ToFileWalker()
	if e != nil {
		return nil, fmt.Errorf("file walker: %w", e)
	}
	o.FileWalker.SourceRoot = f.FileWalkerFlags.SrcFileListFlags.SrcFilesystem()
	return o, nil
}
