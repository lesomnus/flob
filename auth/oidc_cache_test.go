package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/flob"
)

func TestOIDCCanceledLookupDoesNotPoisonNextRequest(t *testing.T) {
	issuer := newTestIssuer(t)
	policy := issuer.policy(t)
	// A syntactically valid token with an unknown kid reaches lookup before
	// its signature can be authenticated. The caller disconnects immediately.
	invalid := sign(t, issuer.key, "unknown", issuer.claims())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := bearer(invalid).WithContext(ctx)
	if err := policy.Authorize(r, "_", flob.ActionRead); err == nil {
		t.Fatal("canceled request was authorized")
	}
	valid := sign(t, issuer.key, "k1", issuer.claims())
	if err := policy.Authorize(bearer(valid), "_", flob.ActionRead); err != nil {
		issuer.mu.Lock()
		requests := issuer.keyRequests
		issuer.mu.Unlock()
		t.Fatalf("healthy issuer's valid request rejected after canceled unauthenticated lookup: %v (JWKS requests=%d)", err, requests)
	}
}

type cacheTestTransport func(*http.Request) (*http.Response, error)

func (f cacheTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOIDCRefreshSurvivesCancellationAndDoesNotBlockCachedKeys(t *testing.T) {
	issuer := newTestIssuer(t)
	client := *issuer.server.Client()
	base := client.Transport
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	block := false
	client.Transport = cacheTestTransport(func(r *http.Request) (*http.Response, error) {
		if block && strings.HasSuffix(r.URL.Path, "/jwks") {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base.RoundTrip(r)
	})
	policy, err := New(issuer.config(), &client)
	if err != nil {
		t.Fatal(err)
	}
	v := policy.providers[0].verifier
	now := time.Now()
	v.now = func() time.Time { return now }
	valid := sign(t, issuer.key, "k1", issuer.claims())
	if err := policy.Authorize(bearer(valid), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
	now = now.Add(refreshInterval + time.Second)
	block = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unknown := sign(t, issuer.key, "unknown", issuer.claims())
	result := make(chan error, 1)
	go func() {
		result <- policy.Authorize(bearer(unknown).WithContext(ctx), "_", flob.ActionRead)
	}()
	t.Cleanup(func() { close(release) })
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not start")
	}
	known := make(chan error, 1)
	go func() { known <- policy.Authorize(bearer(valid), "_", flob.ActionRead) }()
	select {
	case err := <-known:
		if err != nil {
			t.Fatalf("fresh cached key stopped working during refresh: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cached key blocked behind unknown-key refresh")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled caller waited for shared refresh")
	}
}

func TestOIDCInFlightCancellationKeepsDiscoveryAvailable(t *testing.T) {
	issuer := newTestIssuer(t)
	client := *issuer.server.Client()
	base := client.Transport
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	client.Transport = cacheTestTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base.RoundTrip(r)
	})
	policy, err := New(issuer.config(), &client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unknown := sign(t, issuer.key, "unknown", issuer.claims())
	first := make(chan error, 1)
	go func() { first <- policy.Authorize(bearer(unknown).WithContext(ctx), "_", flob.ActionRead) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery did not start")
	}
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled caller did not return")
	}
	valid := sign(t, issuer.key, "k1", issuer.claims())
	second := make(chan error, 1)
	go func() { second <- policy.Authorize(bearer(valid), "_", flob.ActionRead) }()
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("valid request after caller cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid request did not recover")
	}
}

func TestOIDCSlowFailedRefreshReturnsWithoutRetryLoop(t *testing.T) {
	issuer := newTestIssuer(t)
	client := *issuer.server.Client()
	now := time.Now()
	var calls atomic.Int32
	client.Transport = cacheTestTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			now = now.Add(refreshInterval + time.Second)
		}
		return nil, errors.New("provider unavailable")
	})
	policy, err := New(issuer.config(), &client)
	if err != nil {
		t.Fatal(err)
	}
	policy.providers[0].verifier.now = func() time.Time { return now }
	if err := policy.Authorize(bearer(sign(t, issuer.key, "k1", issuer.claims())), "_", flob.ActionRead); err == nil || errors.Is(err, flob.ErrUnauthenticated) {
		t.Fatalf("metadata outage = %v", err)
	}
	if count := calls.Load(); count != 1 {
		t.Fatalf("slow refresh retried %d times, want 1", count)
	}
}
