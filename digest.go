package flob

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"strings"
)

type Digest string

func (d Digest) Sanitize() (Digest, error) {
	const L = len("41c3411cb61b93fcac02a048636286551fe3b3c6a6718ee2601d3ac73ef82c84") // 64
	if len(d) != L {
		return "", fmt.Errorf("length must be 64: %w", ErrInvalidDigest)
	}

	has_upper := false
	for i, r := range d {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			if r >= 'A' && r <= 'F' {
				has_upper = true
			} else {
				return "", fmt.Errorf("[%d] %c: invalid character: %w", i, r, ErrInvalidDigest)
			}
		}
	}
	if has_upper {
		return Digest(strings.ToLower(string(d))), nil
	}
	return d, nil
}

func Hash() hash.Hash {
	return sha256.New()
}
