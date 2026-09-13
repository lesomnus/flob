package flob

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMemWalkLazyLabelsAndMutation(t *testing.T) {
	s := NewMemStores().Use("a").(*MemStore)
	m, err := s.Add(t.Context(), Meta{Labels: Labels{"Version": {"old"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for info, err := range s.Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if err := s.Label(t.Context(), m.Digest, Labels{"Version": {"new"}}); err != nil {
			t.Fatal(err)
		}
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("Version") != "new" {
			t.Fatalf("lazy labels = %v, %v", labels, err)
		}
		if err := s.Erase(t.Context(), m.Digest); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("entries = %d", count)
	}
}

func TestMemEnumerationCancelLastItem(t *testing.T) {
	stores := NewMemStores()
	s := stores.Use("a").(*MemStore)
	if _, err := s.Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errs := 0
	for _, err := range s.Walk(ctx) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			errs++
		} else {
			cancel()
		}
	}
	if errs != 1 {
		t.Fatalf("Walk errors = %d", errs)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	errs = 0
	for _, err := range stores.Namespaces(ctx) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			errs++
		} else {
			cancel()
		}
	}
	if errs != 1 {
		t.Fatalf("Namespaces errors = %d", errs)
	}
}
