package flob

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/textproto"
)

type Labels = http.Header

func readLabels(r io.Reader) (Labels, error) {
	tp := textproto.NewReader(bufio.NewReader(r))

	h, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return Labels(h), nil
}

func writeLabels(w io.Writer, labels Labels) error {
	return labels.Write(w)
}
