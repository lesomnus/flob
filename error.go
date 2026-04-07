package flob

import (
	"errors"
)

var (
	ErrUnimplemented  error = errors.New("unimplemented")
	ErrNotExist       error = errors.New("not exist")
	ErrAlreadyExists  error = errors.New("already exists")
	ErrInvalidDigest  error = errors.New("invalid digest")
	ErrDigestMismatch error = errors.New("digest mismatch")
)
