// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcdigest

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

var ErrRead = errors.New("cannot read complete record")

func Parse(label string) (string, []byte, error) {
	algorithm, encoded, ok := strings.Cut(label, ":")
	if !ok {
		return "", nil, fmt.Errorf("invalid digest %q", label)
	}
	algorithm = strings.ToLower(algorithm)
	if strings.HasPrefix(algorithm, "sha-") {
		algorithm = "sha" + algorithm[4:]
	}
	var size int
	switch algorithm {
	case "md5":
		size = 16
	case "sha1":
		size = 20
	case "sha256":
		size = 32
	case "sha512":
		size = 64
	default:
		return "", nil, fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	if len(encoded) == size*2 {
		if b, err := hex.DecodeString(encoded); err == nil {
			return algorithm, b, nil
		}
	}
	if b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(strings.ToUpper(encoded), "=")); err == nil && len(b) == size {
		return algorithm, b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(b) == size {
		return algorithm, b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(encoded); err == nil && len(b) == size {
		return algorithm, b, nil
	}
	return "", nil, fmt.Errorf("invalid digest encoding %q", label)
}

func Equal(a, b string) bool {
	aa, ab, ae := Parse(a)
	ba, bb, be := Parse(b)
	return ae == nil && be == nil && aa == ba && string(ab) == string(bb)
}

func Verify(label string, r io.Reader) error {
	if label == "" {
		return nil
	}
	algorithm, expected, err := Parse(label)
	if err != nil {
		return err
	}
	var h hash.Hash
	switch algorithm {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	}
	if _, err = io.Copy(h, r); err != nil {
		return errors.Join(ErrRead, err)
	}
	if string(h.Sum(nil)) != string(expected) {
		return fmt.Errorf("digest mismatch: %s", label)
	}
	return nil
}
