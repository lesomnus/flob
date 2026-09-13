package flob

import (
	"errors"
)

var (
	ErrIncompatibleStore = errors.New("incompatible store")
	ErrUnimplemented     = errors.New("unimplemented")
	ErrNotExist          = errors.New("not exist")
	ErrAlreadyExists     = errors.New("already exists")
	ErrInvalidDigest     = errors.New("invalid digest")
	ErrDigestMismatch    = errors.New("digest mismatch")
)
