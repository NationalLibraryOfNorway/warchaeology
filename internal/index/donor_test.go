package index

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

func TestDonorFailureAndConcurrentRegistration(t *testing.T) {
	idx, e := NewDigestIndex(t.TempDir(), true, true)
	if e != nil {
		t.Fatal(e)
	}
	defer idx.Close()
	ref := &gowarc.RevisitRef{Profile: gowarc.ProfileIdenticalPayloadDigestV1_1, TargetRecordId: "https://example.com/record/1", TargetUri: "https://example.com/", TargetDate: "2020-01-01T00:00:00.123456789Z"}
	failure := errors.New("output failed")
	if e = idx.WriteWithDonor("sha1:1234", ref, func(*gowarc.RevisitRef) error { return failure }); !errors.Is(e, failure) {
		t.Fatal(e)
	}
	var first atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if e := idx.WriteWithDonor("sha1:1234", ref, func(existing *gowarc.RevisitRef) error {
				if existing == nil {
					first.Add(1)
				} else if existing.TargetDate != ref.TargetDate {
					t.Error("timestamp precision lost")
				}
				return nil
			}); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if first.Load() != 1 {
		t.Fatal(first.Load())
	}
}
func TestUnpublishedDonorRunRefusesRestart(t *testing.T) {
	dir := t.TempDir()
	idx, e := NewDigestIndex(dir, true, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = idx.BeginOutputRun(); e != nil {
		t.Fatal(e)
	}
	if e = idx.Close(); e != nil {
		t.Fatal(e)
	}
	if idx, e = NewDigestIndex(dir, true, false); e == nil {
		_ = idx.Close()
		t.Fatal("unpublished output reused")
	}
	idx, e = NewDigestIndex(dir, true, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = idx.BeginOutputRun(); e != nil {
		t.Fatal(e)
	}
	if e = idx.CommitOutputRun(); e != nil {
		t.Fatal(e)
	}
	_ = idx.Close()
	idx, e = NewDigestIndex(dir, true, false)
	if e != nil {
		t.Fatal(e)
	}
	_ = idx.Close()
}

func TestFileDonorVisibility(t *testing.T) {
	idx, err := NewDigestIndex(t.TempDir(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	first, second := idx.NewFileDonors(), idx.NewFileDonors()
	write := func(file *FileDonors, id, want string) {
		t.Helper()
		ref := &gowarc.RevisitRef{TargetRecordId: id, TargetDate: "2020-01-01T00:00:00Z"}
		err := file.WriteWithDonor("sha1:1234", ref, func(d *gowarc.RevisitRef) error {
			got := ""
			if d != nil {
				got = d.TargetRecordId
			}
			if got != want {
				t.Fatalf("donor %q, want %q", got, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	write(first, "one", "")
	write(first, "again", "one")
	write(second, "two", "")
	if err := second.Finish(true); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(false); err != nil {
		t.Fatal(err)
	}
	third := idx.NewFileDonors()
	write(third, "three", "two")
}
