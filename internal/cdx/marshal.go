// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/json"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

type Pywb struct{ *schema.Cdx }

func (p Pywb) MarshalCdxj() ([]byte, error) { return Line(p.Cdx) }

func (p Pywb) MarshalJSON() ([]byte, error) {
	m, e := Properties(p.Cdx)
	if e != nil {
		return nil, e
	}
	m["urlkey"] = p.Ssu
	m["timestamp"] = timestamp.Format14(p.Sts.AsTime())
	return json.Marshal(m)
}
