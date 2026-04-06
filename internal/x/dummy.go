package x

import "bytes"

func (X) Data() []byte {
	return []byte("Royale with Cheese")
}

func (x X) Reader() *bytes.Reader {
	return bytes.NewReader(x.Data())
}
