// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package storagecodec provides versioned, lossless protobuf storage envelopes.
package storagecodec

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const Version = 1

// Digest retains the original spelling unless a known canonical representation
// can be recovered exactly. The algorithm and representation are separate tags.
func Digest(s string) []byte {
	prefix, value, ok := strings.Cut(s, ":")
	if !ok {
		prefix = ""
		value = s
	}
	algorithms := []string{"", "sha1", "sha256", "sha512", "md5"}
	for algorithm, p := range algorithms {
		if p != prefix {
			continue
		}
		for format := byte(1); format <= 2; format++ {
			var b []byte
			var err error
			var roundtrip string
			if format == 1 {
				b, err = hex.DecodeString(value)
				roundtrip = hex.EncodeToString(b)
			} else {
				b, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(value)
				roundtrip = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
			}
			if err == nil && roundtrip == value && len(b) > 0 {
				return append([]byte{byte(algorithm) + 1, format}, b...)
			}
		}
	}
	return append([]byte{0}, s...)
}
func DigestString(b []byte) (string, error) {
	if len(b) == 0 {
		return "", fmt.Errorf("empty digest encoding")
	}
	if b[0] == 0 {
		return string(b[1:]), nil
	}
	names := []string{"", "sha1:", "sha256:", "sha512:", "md5:"}
	if int(b[0]) > len(names) || len(b) < 3 {
		return "", fmt.Errorf("invalid digest encoding")
	}
	var value string
	switch b[1] {
	case 1:
		value = hex.EncodeToString(b[2:])
	case 2:
		value = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[2:])
	default:
		return "", fmt.Errorf("invalid digest representation")
	}
	return names[b[0]-1] + value, nil
}
func RecordID(s string) []byte {
	for i, p := range []string{"urn:uuid:", "<urn:uuid:"} {
		v := strings.TrimPrefix(s, p)
		if i == 1 {
			v = strings.TrimSuffix(v, ">")
		}
		if len(v) != 36 {
			continue
		}
		raw, err := hex.DecodeString(strings.ReplaceAll(v, "-", ""))
		if err != nil || len(raw) != 16 {
			continue
		}
		encoded := append([]byte{byte(i + 1)}, raw...)
		if RecordIDString(encoded) == s {
			return encoded
		}
	}
	return append([]byte{0}, s...)
}
func RecordIDString(b []byte) string {
	if len(b) == 17 && (b[0] == 1 || b[0] == 2) {
		s := hex.EncodeToString(b[1:])
		s = "urn:uuid:" + s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
		if b[0] == 2 {
			s = "<" + s + ">"
		}
		return s
	}
	if len(b) > 0 && b[0] == 0 {
		return string(b[1:])
	}
	return ""
}
func field(b []byte, n protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, n, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

// Encode stores the ordered SURT in the key only. Full metadata remains optional.
func Encode(c *cdx.Capture) ([]byte, error) {
	v := proto.Clone(c).(*cdx.Capture)
	v.Ssu = ""
	v.Dig = ""
	v.Rid = ""
	derived := c.GetRef() == "warcfile:"+c.GetSource()+"#"+strconv.FormatInt(c.GetOffset(), 10)
	if derived {
		v.Ref = ""
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		return nil, err
	}
	b := []byte{8, Version}
	b = field(b, 2, raw)
	if c.GetDig() != "" {
		b = field(b, 3, Digest(c.GetDig()))
	}
	if c.GetRid() != "" {
		b = field(b, 4, RecordID(c.GetRid()))
	}
	if derived {
		b = append(b, 40, 1)
	}
	return b, nil
}
func Decode(b []byte, key string) (*cdx.Capture, error) {
	c := &cdx.Capture{}
	version := uint64(0)
	derived := false
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, protowire.ParseError(k)
		}
		b = b[k:]
		if t == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return nil, protowire.ParseError(k)
			}
			b = b[k:]
			switch n {
			case 1:
				version = v
			case 5:
				derived = v == 1
			}
			continue
		}
		if t != protowire.BytesType {
			return nil, fmt.Errorf("invalid storage field")
		}
		v, k := protowire.ConsumeBytes(b)
		if k < 0 {
			return nil, protowire.ParseError(k)
		}
		b = b[k:]
		var err error
		switch n {
		case 2:
			err = proto.Unmarshal(v, c)
		case 3:
			c.Dig, err = DigestString(v)
		case 4:
			c.Rid = RecordIDString(v)
			if c.GetRid() == "" && !bytes.Equal(v, []byte{0}) {
				err = fmt.Errorf("invalid record ID")
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if version != Version {
		return nil, fmt.Errorf("unsupported storage codec %d", version)
	}
	c.Ssu = key
	if derived {
		c.Ref = "warcfile:" + c.GetSource() + "#" + strconv.FormatInt(c.GetOffset(), 10)
	}
	return c, nil
}
