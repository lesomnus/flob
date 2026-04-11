package x

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func (a X) Contains(v, target any) {
	a.t.Helper()

	kind := reflect.ValueOf(v).Kind()
	switch kind {
	case reflect.String:
		if strings.Contains(reflect.ValueOf(v).String(), reflect.ValueOf(target).String()) {
			return
		}

	case reflect.Slice:
		for i := 0; i < reflect.ValueOf(v).Len(); i++ {
			if reflect.DeepEqual(reflect.ValueOf(v).Index(i).Interface(), target) {
				return
			}
		}

	case reflect.Map:
		iter := reflect.ValueOf(v).MapRange()
		for iter.Next() {
			if reflect.DeepEqual(iter.Key().Interface(), target) {
				return
			}
		}
	}

	a.t.Fatalf("assert.Contains failed: %v does not contain %v", formatValue(v), formatValue(target))
}

func (a X) Len(v any, expected int) {
	a.t.Helper()

	length := reflect.ValueOf(v).Len()
	if length == expected {
		return
	}

	a.t.Fatalf("assert.Len failed: got=%d want=%d", length, expected)
}

func formatValue(v any) string {
	return fmt.Sprintf("%#v", v)
}
