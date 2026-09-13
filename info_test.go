package flob

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestInfoLazyLabels(t *testing.T) {
	calls := 0
	source := Labels{"Test": {"original"}}
	d := DigestFromBytes([]byte("hello"))
	info := NewInfo(d, 5, func(context.Context) (Labels, error) {
		calls++
		return source, nil
	})
	if info.Digest() != d || info.Size() != 5 || calls != 0 {
		t.Fatal("identity access loaded labels")
	}
	first, err := info.Labels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source["Test"][0] = "source mutation"
	first["Test"][0] = "caller mutation"
	first["New"] = []string{"added"}
	second, err := info.Labels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || second.Get("Test") != "original" || second.Get("New") != "" {
		t.Fatalf("cached labels were changed: %#v (%d loads)", second, calls)
	}
}

func TestInfoRetriesFailedLoadWithCallerContext(t *testing.T) {
	type key struct{}
	calls := 0
	info := NewInfo("", 0, func(ctx context.Context) (Labels, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return Labels{"Value": {ctx.Value(key{}).(string)}}, nil
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := info.Labels(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Labels: %v", err)
	}
	labels, err := info.Labels(context.WithValue(t.Context(), key{}, "later call"))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || labels.Get("Value") != "later call" {
		t.Fatalf("retry = %#v (%d loads)", labels, calls)
	}
}

func TestInfoMemoizesNilLabels(t *testing.T) {
	calls := 0
	for _, loader := range []func(context.Context) (Labels, error){nil, func(context.Context) (Labels, error) { calls++; return nil, nil }} {
		info := NewInfo("", 0, loader)
		for range 2 {
			labels, err := info.Labels(t.Context())
			if err != nil || labels != nil {
				t.Fatalf("Labels = %#v, %v", labels, err)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("nil result loaded %d times", calls)
	}
}

func TestInfoConcurrentLabels(t *testing.T) {
	calls := 0 // A race is a test failure if the loader is not serialized.
	info := NewInfo("", 0, func(context.Context) (Labels, error) {
		calls++
		return Labels{"Test": {"original"}}, nil
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			labels, err := info.Labels(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			if labels.Get("Test") != "original" {
				t.Errorf("labels changed: %#v", labels)
			}
			labels["Test"][0] = "changed"
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("loaded %d times", calls)
	}
}
