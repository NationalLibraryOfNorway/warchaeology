package index

import (
	"encoding/json"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
)

type Pywb struct{ *schema.Cdx }

func (p Pywb) MarshalCdxj() ([]byte, error) { return Line(p.Cdx) }

func (p Pywb) MarshalJSON() ([]byte, error) {
	m, e := Properties(p.Cdx)
	if e != nil {
		return nil, e
	}
	m["urlkey"] = p.Ssu
	m["timestamp"] = p.Sts.AsTime().UTC().Format("20060102150405")
	return json.Marshal(m)
}
