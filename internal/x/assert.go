package x

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type X struct {
	t *testing.T
}

func New(t *testing.T) (context.Context, X) {
	t.Helper()
	return t.Context(), X{t: t}
}

func (a X) Eq(expected, actual any) {
	a.t.Helper()
	if reflect.DeepEqual(expected, actual) {
		return
	}

	a.t.Fatalf("assert.Eq failed: got=%s want=%s", formatValue(actual), formatValue(expected))
}

func (a X) NoError(err error) {
	a.t.Helper()
	if err == nil {
		return
	}

	a.t.Fatalf("assert.NotError failed: err=%v", err)
}

func (a X) ErrorIs(err error, target error) {
	a.t.Helper()
	if errors.Is(err, target) {
		return
	}

	a.t.Fatalf("assert.ErrorIs failed: err=%v target=%v", err, target)
}

func formatValue(v any) string {
	return fmt.Sprintf("%#v", v)
}
