package flob

import (
	"errors"
)

var (
	ErrStageExpired      = errors.New("stage expired")
	ErrStageClosed       = errors.New("stage closed")
	ErrStageConflict     = errors.New("stage operation conflicts with stored state")
	ErrStageFormat       = errors.New("unsupported or corrupt stage format")
	ErrOffsetMismatch    = errors.New("stage offset mismatch")
	ErrIncompatibleStore = errors.New("incompatible store")
	ErrUnimplemented     = errors.New("unimplemented")
	ErrNotExist          = errors.New("not exist")
	ErrAlreadyExists     = errors.New("already exists")
	ErrInvalidDigest     = errors.New("invalid digest")
	ErrDigestMismatch    = errors.New("digest mismatch")
)
