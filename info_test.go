package flob

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestInfoLazyLabels(t *testing.T) {
	calls := 0
	source := Labels{"Test": {"original"}}
	d := DigestFromBytes([]byte("hello"))
	info := NewInfo(d, 5, time.Time{}, func(context.Context) (Labels, error) {
		calls++
		return source, nil
	})
	if info.Digest() != d || mustSize(t, info) != 5 || calls != 0 {
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
	info := NewInfo("", 0, time.Time{}, func(ctx context.Context) (Labels, error) {
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
		info := NewInfo("", 0, time.Time{}, loader)
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
	info := NewInfo("", 0, time.Time{}, func(context.Context) (Labels, error) {
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

func mustSize(t *testing.T, info Info) int64 {
	t.Helper()
	size, err := info.Size(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return size
}

func TestLazyInfoLoadsSizeOnDemand(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	sizes, labels := 0, 0
	info := NewLazyInfo(d, func(context.Context) (int64, error) {
		sizes++
		if sizes == 1 {
			return 0, errors.New("unavailable")
		}
		return 5, nil
	}, nil, func(context.Context) (Labels, error) {
		labels++
		return nil, nil
	})
	if info.Digest() != d || sizes != 0 || labels != 0 {
		t.Fatal("construction loaded size or labels")
	}
	if _, err := info.Size(t.Context()); err == nil {
		t.Fatal("size failure was hidden")
	}
	for range 2 {
		if size := mustSize(t, info); size != 5 {
			t.Fatalf("size = %d", size)
		}
	}
	if sizes != 2 || labels != 0 {
		t.Fatalf("loads = %d sizes, %d labels", sizes, labels)
	}
}

func TestLazyInfoSerializesLoads(t *testing.T) {
	shared := 0 // A race is a test failure if the loaders run concurrently.
	info := NewLazyInfo("", func(context.Context) (int64, error) {
		shared++
		return int64(shared), nil
	}, func(context.Context) (time.Time, error) {
		shared++
		return time.Unix(int64(shared), 0), nil
	}, func(context.Context) (Labels, error) {
		shared++
		return nil, nil
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { info.Size(t.Context()) })
		wg.Go(func() { info.Added(t.Context()) })
		wg.Go(func() { info.Labels(t.Context()) })
	}
	wg.Wait()
	if shared != 3 {
		t.Fatalf("loaded %d times", shared)
	}
}

func TestInfoAdded(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if added, err := NewInfo("", 0, at, nil).Added(t.Context()); err != nil || !added.Equal(at) {
		t.Fatalf("known Added = %v, %v", added, err)
	}
	for _, info := range []Info{
		NewInfo("", 0, time.Time{}, nil),
		NewLazyInfo("", nil, nil, nil),
		NewLazyInfo("", nil, func(context.Context) (time.Time, error) { return time.Time{}, nil }, nil),
	} {
		if _, err := info.Added(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("unknown Added = %v", err)
		}
	}
	calls := 0
	lazy := NewLazyInfo("", nil, func(context.Context) (time.Time, error) {
		calls++
		if calls == 1 {
			return time.Time{}, errors.New("unavailable")
		}
		return at, nil
	}, nil)
	if calls != 0 {
		t.Fatal("construction loaded the time")
	}
	if _, err := lazy.Added(t.Context()); err == nil || errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("failed Added = %v", err)
	}
	for range 2 {
		if added, err := lazy.Added(t.Context()); err != nil || !added.Equal(at) {
			t.Fatalf("retried Added = %v, %v", added, err)
		}
	}
	if calls != 2 {
		t.Fatalf("loaded %d times", calls)
	}
}
