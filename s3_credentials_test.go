package flob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/flob/internal/x"
)

// TestS3CredentialsProvider runs the store against a provider whose set is
// replaced while it runs, as a mounted Secret or a minted STS token is.
func TestS3CredentialsProvider(t *testing.T) {
	ctx, x := x.New(t)

	mock := newMockS3("flob-test")
	var (
		mu   sync.Mutex
		seen []string // access key id of each request, in order
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := credentialOf(t, r.Header.Get("Authorization"))
		mu.Lock()
		seen = append(seen, id)
		mu.Unlock()
		mock.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	drain := func() []string {
		mu.Lock()
		defer mu.Unlock()
		s := seen
		seen = nil
		return s
	}

	var current atomic.Pointer[Credentials]
	current.Store(&Credentials{AccessKeyID: "AKIDOLD", SecretAccessKey: "old"})
	fail := errors.New("credentials source unavailable")
	var failing atomic.Bool
	provider := CredentialsFunc(func(context.Context) (Credentials, error) {
		if failing.Load() {
			return Credentials{}, fail
		}
		return *current.Load(), nil
	})

	stores, err := NewS3Stores(S3Config{
		Endpoint:     srv.URL,
		Bucket:       "flob-test",
		Credentials:  provider,
		UsePathStyle: true,
		Client:       srv.Client(),
		DirectAdd:    S3DirectAddNever,
	})
	x.NoError(err)
	store := stores.Use("t")

	added, err := store.Add(ctx, Meta{}, x.Reader())
	x.NoError(err)
	for _, id := range drain() {
		x.Eq("AKIDOLD", id)
	}

	// Replaced while running: the next requests are signed with the new set.
	current.Store(&Credentials{AccessKeyID: "AKIDNEW", SecretAccessKey: "new"})
	_, err = store.Stat(ctx, added.Digest)
	x.NoError(err)
	got := drain()
	if len(got) == 0 {
		t.Fatal("Stat sent no request")
	}
	for _, id := range got {
		x.Eq("AKIDNEW", id)
	}

	// So is a presigned redirect URL.
	loc, _, err := store.(Presigner).PresignOpen(ctx, added.Digest, time.Minute)
	x.NoError(err)
	if !strings.Contains(loc, "X-Amz-Credential=AKIDNEW%2F") {
		t.Fatalf("presigned url not signed with the new set: %q", loc)
	}
	drain()

	// A provider error fails the request before it is sent.
	failing.Store(true)
	_, err = store.Stat(ctx, added.Digest)
	x.ErrorIs(err, fail)
	_, _, err = store.(Presigner).PresignOpen(ctx, added.Digest, time.Minute)
	x.ErrorIs(err, fail)
	if got := drain(); len(got) != 0 {
		t.Fatalf("requests sent despite the provider error: %v", got)
	}
}

func TestS3PresignWithinCredentialsLifetime(t *testing.T) {
	ctx, x := x.New(t)
	mock := newMockS3("flob-test")
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stores, err := NewS3Stores(S3Config{
		Endpoint:     srv.URL,
		Bucket:       "flob-test",
		Credentials:  Credentials{AccessKeyID: "k", SecretAccessKey: "s", SessionToken: "t", Expires: now.Add(2 * time.Minute)},
		UsePathStyle: true,
		Client:       srv.Client(),
		now:          func() time.Time { return now },
	})
	x.NoError(err)

	added, err := stores.Use("t").Add(ctx, Meta{}, x.Reader())
	x.NoError(err)
	loc, _, err := stores.Use("t").(Presigner).PresignOpen(ctx, added.Digest, DefaultRedirectTTL)
	x.NoError(err)
	if !strings.Contains(loc, "X-Amz-Expires=120&") {
		t.Fatalf("presigned url outlives its credentials: %q", loc)
	}
}
