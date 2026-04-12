package x

import (
	"bytes"
	"crypto/sha256"
	"fmt"
)

func (X) Data() []byte {
	return []byte("Royale with Cheese")
}

func (x X) Digest() string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(x.Data()))
}

func (x X) Reader() *bytes.Reader {
	return bytes.NewReader(x.Data())
}
