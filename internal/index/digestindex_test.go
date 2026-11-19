// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func TestDigestIndex_IsRevisit_FirstInsertThenLookup(t *testing.T) {
	idx, err := NewDigestIndex(t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)

	ref := &gowarc.RevisitRef{
		Profile:        gowarc.ProfileIdenticalPayloadDigestV1_1,
		TargetRecordId: "<urn:uuid:ce151eae-2bb0-41a7-a1b5-a984d5e4fa70>",
		TargetDate:     "2006-11-17T11:48:47Z",
		TargetUri:      "http://www.example.com",
	}

	got, err := idx.IsRevisit("digest-key", ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil on first insert, got: %+v", got)
	}

	got, err = idx.IsRevisit("digest-key", ref)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("expected existing revisit reference")
	}
	if got.TargetRecordId != ref.TargetRecordId {
		t.Fatalf("unexpected target record id: got %s, want %s", got.TargetRecordId, ref.TargetRecordId)
	}
}

func TestDigestIndex_IsRevisit_CorruptValue(t *testing.T) {
	idx, err := NewDigestIndex(t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)

	err = idx.db.Update(func(txn *badger.Txn) error {
		return txn.Set(digestKey("corrupt"), []byte{9, 1, 2})
	})
	if err != nil {
		t.Fatal(err)
	}

	ref := &gowarc.RevisitRef{
		Profile:        gowarc.ProfileIdenticalPayloadDigestV1_1,
		TargetRecordId: "<urn:uuid:ce151eae-2bb0-41a7-a1b5-a984d5e4fa70>",
		TargetDate:     "2006-11-17T11:48:47Z",
		TargetUri:      "http://www.example.com",
	}

	got, err := idx.IsRevisit("corrupt", ref)
	if err == nil {
		t.Fatalf("expected error for corrupt index value")
	}
	if got != nil {
		t.Fatalf("expected nil revisit ref on corrupt value")
	}
}
