package flob

import (
	"errors"
)

var (
	ErrUnimplemented  = errors.New("unimplemented")
	ErrNotExist       = errors.New("not exist")
	ErrAlreadyExists  = errors.New("already exists")
	ErrInvalidDigest  = errors.New("invalid digest")
	ErrDigestMismatch = errors.New("digest mismatch")
)
