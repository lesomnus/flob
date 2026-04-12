package flob

import (
	"hash"
	"io"
	"strings"

	"github.com/opencontainers/go-digest"
)

type Digest string

func (d Digest) Sanitize() (Digest, error) {
	if err := digest.Digest(d).Validate(); err != nil {
		return "", err
	}

	return Digest(strings.ToLower(string(d))), nil
}

func (d Digest) Algorithm() digest.Algorithm {
	return digest.Digest(d).Algorithm()
}

func (d Digest) Encoded() string {
	return digest.Digest(d).Encoded()
}

func (d Digest) String() string {
	return digest.Digest(d).String()
}

func (d Digest) Verifier() digest.Verifier {
	return digest.Digest(d).Verifier()
}

const Canonical = digest.Canonical

func Hash() hash.Hash {
	return Canonical.Hash()
}

func DigestFromBytes(data []byte) Digest {
	return Digest(Canonical.FromBytes(data))
}

func DigestFromReader(r io.Reader) (Digest, error) {
	d, err := Canonical.FromReader(r)
	return Digest(d), err
}
