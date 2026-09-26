package flob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestFill(t *testing.T) {
	stores := map[string]func(t *testing.T) (Store, string){
		"mem": func(t *testing.T) (Store, string) { return NewMemStores().Use("t"), "" },
		"os": func(t *testing.T) (Store, string) {
			root := t.TempDir()
			return NewOsStores(root).Use("t"), filepath.Join(root, "stage")
		},
	}
	const content = "followed while it is written"
	d := DigestFromBytes([]byte(content))
	m := Meta{Digest: d, Size: int64(len(content))}

	begin := func(t *testing.T, store Store, m Meta) Fill {
		t.Helper()
		filler, ok := AsFiller(store)
		if !ok {
			t.Fatal("not a Filler")
		}
		f, err := filler.Fill(t.Context(), m)
		if err != nil {
			t.Fatalf("fill: %v", err)
		}
		return f
	}
	follow := func(t *testing.T, f Fill, ctx context.Context) io.ReadSeekCloser {
		t.Helper()
		r, err := f.Follow(ctx)
		if err != nil {
			t.Fatalf("follow: %v", err)
		}
		return r
	}
	write := func(t *testing.T, f Fill, s string) {
		t.Helper()
		if n, err := f.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	// read starts reading r into a channel.
	read := func(r io.Reader, n int) <-chan string {
		done := make(chan string, 1)
		go func() {
			b := make([]byte, n)
			_, err := io.ReadFull(r, b)
			if err != nil {
				done <- "error: " + err.Error()
				return
			}
			done <- string(b)
		}()
		return done
	}
	temps := func(t *testing.T, dir string) int {
		t.Helper()
		if dir == "" {
			return 0
		}
		es, _ := filepath.Glob(filepath.Join(dir, "flob-*"))
		return len(es)
	}

	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			t.Run("readers follow the writes", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, stage := open(t)
					f := begin(t, store, m)
					early := follow(t, f, t.Context())
					write(t, f, content[:10])

					first := read(early, 10)
					synctest.Wait()
					if got := <-first; got != content[:10] {
						t.Fatalf("read %q", got)
					}
					rest := read(early, len(content)-10)
					synctest.Wait()
					select {
					case got := <-rest:
						t.Fatalf("read %q before it was written", got)
					default:
					}

					late := follow(t, f, t.Context())
					write(t, f, content[10:])
					synctest.Wait()
					if got := <-rest; got != content[10:] {
						t.Fatalf("read %q", got)
					}
					if n, err := early.Read(make([]byte, 1)); n != 0 || err != io.EOF {
						t.Fatalf("read at the end = %d, %v; want EOF", n, err)
					}
					flightRead(t, late, content)

					if _, err := f.Commit(t.Context()); err != nil {
						t.Fatalf("commit: %v", err)
					}
					early.Close()
					f.Abort(nil)
					r, _, err := store.Open(t.Context(), d)
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					flightRead(t, r, content)
					if _, err := f.Follow(t.Context()); err == nil {
						t.Fatal("followed a released fill")
					}
					if n := temps(t, stage); n != 0 {
						t.Fatalf("%d temp files left", n)
					}
				})
			})

			t.Run("a reader keeps a committed fill", func(t *testing.T) {
				store, stage := open(t)
				f := begin(t, store, m)
				r := follow(t, f, t.Context())
				write(t, f, content)
				if _, err := f.Commit(t.Context()); err != nil {
					t.Fatalf("commit: %v", err)
				}
				f.Abort(nil)
				if got, err := io.ReadAll(r); err != nil || string(got) != content {
					t.Fatalf("read %q, %v", got, err)
				}
				if stage != "" && temps(t, stage) != 1 {
					t.Fatal("the temp file went before its reader closed")
				}
				r.Close()
				if n := temps(t, stage); n != 0 {
					t.Fatalf("%d temp files left", n)
				}
			})

			t.Run("wrong bytes are not read to the end or kept", func(t *testing.T) {
				store, stage := open(t)
				f := begin(t, store, m)
				r := follow(t, f, t.Context())
				write(t, f, strings.ToUpper(content))
				if _, err := io.ReadAll(r); !errors.Is(err, ErrDigestMismatch) {
					t.Fatalf("read: %v; want %v", err, ErrDigestMismatch)
				}
				if _, err := f.Commit(t.Context()); !errors.Is(err, ErrDigestMismatch) {
					t.Fatalf("commit: %v; want %v", err, ErrDigestMismatch)
				}
				r.Close()
				f.Abort(nil)
				if _, err := store.Stat(t.Context(), d); !errors.Is(err, ErrNotExist) {
					t.Fatalf("stat: %v; want %v", err, ErrNotExist)
				}
				if n := temps(t, stage); n != 0 {
					t.Fatalf("%d temp files left", n)
				}
			})

			t.Run("an abort reaches a waiting reader", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, _ := open(t)
					f := begin(t, store, m)
					r := follow(t, f, t.Context())
					write(t, f, content[:4])
					done := read(r, len(content))
					synctest.Wait()
					cause := errors.New("origin went away")
					f.Abort(cause)
					synctest.Wait()
					if got := <-done; got != "error: "+cause.Error() {
						t.Fatalf("read %q", got)
					}
					r.Close()
					if _, err := f.Commit(t.Context()); err == nil {
						t.Fatal("committed an aborted fill")
					}
				})
			})

			t.Run("a waiting reader stops with its context", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, _ := open(t)
					f := begin(t, store, m)
					ctx, cancel := context.WithCancel(t.Context())
					r := follow(t, f, ctx)
					done := read(r, 1)
					synctest.Wait()
					cancel()
					synctest.Wait()
					if got := <-done; got != "error: "+context.Canceled.Error() {
						t.Fatalf("read %q", got)
					}
					r.Close()
					f.Abort(nil)
				})
			})

			t.Run("writing past the size fails", func(t *testing.T) {
				store, _ := open(t)
				f := begin(t, store, m)
				if _, err := f.Write([]byte(content + "!")); err == nil {
					t.Fatal("want an error")
				}
				f.Abort(nil)
			})

			t.Run("an empty blob is complete at once", func(t *testing.T) {
				store, _ := open(t)
				f := begin(t, store, Meta{Digest: DigestFromBytes(nil)})
				flightRead(t, follow(t, f, t.Context()), "")
				if _, err := f.Commit(t.Context()); err != nil {
					t.Fatalf("commit: %v", err)
				}
				f.Abort(nil)
			})

			t.Run("a blob the store has is not filled", func(t *testing.T) {
				store, _ := open(t)
				if _, err := store.Add(t.Context(), Meta{}, strings.NewReader(content)); err != nil {
					t.Fatalf("add: %v", err)
				}
				filler, _ := AsFiller(store)
				if _, err := filler.Fill(t.Context(), m); !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("fill: %v; want %v", err, ErrAlreadyExists)
				}
			})
		})
	}
}

// gatedReader serves content only as far as it has been let through, then
// fails with err if one is set.
type gatedReader struct {
	content string

	mu      sync.Mutex
	off     int
	allowed int
	err     error
	wake    chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newGatedReader(content string) *gatedReader {
	return &gatedReader{content: content, wake: make(chan struct{}), closed: make(chan struct{})}
}

func (g *gatedReader) let(n int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed = min(g.allowed+n, len(g.content))
	g.err = err
	close(g.wake)
	g.wake = make(chan struct{})
}

func (g *gatedReader) Read(b []byte) (int, error) {
	for {
		g.mu.Lock()
		off, allowed, err, wake := g.off, g.allowed, g.err, g.wake
		if off < allowed {
			n := copy(b, g.content[off:allowed])
			g.off += n
			g.mu.Unlock()
			return n, nil
		}
		g.mu.Unlock()
		if off == len(g.content) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		select {
		case <-wake:
		case <-g.closed:
			return 0, io.ErrClosedPipe
		}
	}
}

func (g *gatedReader) Seek(int64, int) (int64, error) { return 0, errors.ErrUnsupported }

func (g *gatedReader) Close() error {
	g.once.Do(func() { close(g.closed) })
	return nil
}

func TestCacheFollow(t *testing.T) {
	const content = "every caller streams from the first byte"
	type env struct {
		cache   *CacheStore
		primary Store
		gate    *gatedReader
		opens   *atomic.Int32
		d       Digest
	}
	// setup serves the first origin Open from served, gated, and the rest from
	// the origin itself.
	setup := func(t *testing.T, primary Store, served string) env {
		t.Helper()
		source := NewMemStores().Use("t")
		d := flightAdd(t, source, content)
		gate := newGatedReader(served)
		opens := &atomic.Int32{}
		origin := flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
			if opens.Add(1) == 1 {
				return gate, NewInfo(d, int64(len(content)), time.Time{}, nil), nil
			}
			return source.Open(ctx, d)
		}}
		return env{cache: NewCacheStore(primary, origin), primary: primary, gate: gate, opens: opens, d: d}
	}

	t.Run("callers do not wait for the fill", func(t *testing.T) {
		for name, primary := range map[string]func(t *testing.T) Store{
			"mem": func(t *testing.T) Store { return NewMemStores().Use("t") },
			"os":  func(t *testing.T) Store { return NewOsStores(t.TempDir()).Use("t") },
		} {
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					e := setup(t, primary(t), content)
					first := flightOpen(t.Context(), e.cache, e.d)
					second := flightOpen(t.Context(), e.cache, e.d)
					synctest.Wait()
					a, b := flightResult(t, first), flightResult(t, second)
					if a.err != nil || b.err != nil {
						t.Fatalf("open: %v, %v", a.err, b.err)
					}

					e.gate.let(5, nil)
					synctest.Wait()
					for _, r := range []io.Reader{a.reader, b.reader} {
						got := make([]byte, 5)
						if _, err := io.ReadFull(r, got); err != nil || string(got) != content[:5] {
							t.Fatalf("first bytes = %q, %v", got, err)
						}
					}
					third := flightOpen(t.Context(), e.cache, e.d)
					synctest.Wait()
					c := flightResult(t, third)

					e.gate.let(len(content), nil)
					synctest.Wait()
					for _, r := range []io.ReadSeekCloser{a.reader, b.reader} {
						flightRead(t, r, content[5:])
					}
					flightRead(t, c.reader, content)
					if got := e.opens.Load(); got != 1 {
						t.Fatalf("origin opens = %d; want 1", got)
					}
					flightActive(t, &e.cache.local, 0)
					r, _, err := e.primary.Open(t.Context(), e.d)
					if err != nil {
						t.Fatalf("primary open: %v", err)
					}
					flightRead(t, r, content)
				})
			})
		}
	})

	t.Run("callers leaving do not stop the fill", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t, NewMemStores().Use("t"), content)
			r, _, err := e.cache.Open(t.Context(), e.d)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			r.Close()
			e.gate.let(len(content), nil)
			synctest.Wait()
			if _, err := e.primary.Stat(t.Context(), e.d); err != nil {
				t.Fatalf("primary stat: %v", err)
			}
			flightActive(t, &e.cache.local, 0)
		})
	})

	t.Run("a failed fill is continued from the origin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t, NewMemStores().Use("t"), content)
			r, _, err := e.cache.Open(t.Context(), e.d)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			e.gate.let(7, errors.New("origin went away"))
			synctest.Wait()
			flightRead(t, r, content)
			if got := e.opens.Load(); got != 2 {
				t.Fatalf("origin opens = %d; want 2", got)
			}
			if _, err := e.primary.Stat(t.Context(), e.d); !errors.Is(err, ErrNotExist) {
				t.Fatalf("primary stat: %v; want %v", err, ErrNotExist)
			}
			flightActive(t, &e.cache.local, 0)
		})
	})

	t.Run("wrong bytes from the origin are not continued", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t, NewMemStores().Use("t"), strings.ToUpper(content))
			r, _, err := e.cache.Open(t.Context(), e.d)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			e.gate.let(len(content), nil)
			synctest.Wait()
			if _, err := io.ReadAll(r); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("read: %v; want %v", err, ErrDigestMismatch)
			}
			r.Close()
			if got := e.opens.Load(); got != 1 {
				t.Fatalf("origin opens = %d; want 1", got)
			}
			synctest.Wait()
			flightActive(t, &e.cache.local, 0)
		})
	})

	t.Run("a fill nobody reads ends after FillTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t, NewMemStores().Use("t"), content)
			e.cache.FillTimeout = time.Minute
			r, _, err := e.cache.Open(t.Context(), e.d)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			e.gate.let(3, nil)
			r.Close()
			time.Sleep(59 * time.Second)
			synctest.Wait()
			flightActive(t, &e.cache.local, 1)

			// Someone arriving resets the clock.
			r, _, err = e.cache.Open(t.Context(), e.d)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			time.Sleep(59 * time.Second)
			synctest.Wait()
			r.Close()
			time.Sleep(59 * time.Second)
			synctest.Wait()
			flightActive(t, &e.cache.local, 1)
			time.Sleep(2 * time.Second)
			synctest.Wait()
			flightActive(t, &e.cache.local, 0)
			if _, err := e.primary.Stat(t.Context(), e.d); !errors.Is(err, ErrNotExist) {
				t.Fatalf("primary stat: %v; want %v", err, ErrNotExist)
			}
		})
	})

	t.Run("a waiting caller can cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := NewMemStores().Use("t")
			d := flightAdd(t, source, content)
			release := make(chan struct{})
			origin := flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
				return source.Open(ctx, d)
			}}
			cache := NewCacheStore(NewMemStores().Use("t"), origin)
			ctx, cancel := context.WithCancel(t.Context())
			waiting := flightOpen(ctx, cache, d)
			other := flightOpen(t.Context(), cache, d)
			synctest.Wait()
			cancel()
			synctest.Wait()
			if got := flightResult(t, waiting).err; !errors.Is(got, context.Canceled) {
				t.Fatalf("open: %v; want %v", got, context.Canceled)
			}
			close(release)
			synctest.Wait()
			flightReadResult(t, other, content)
		})
	})

	t.Run("an origin error is shared", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cause := errors.New("upstream says no")
			var opens atomic.Int32
			release := make(chan struct{})
			origin := flightTestStore{Store: NewMemStores().Use("t"), open: func(context.Context, Digest) (io.ReadSeekCloser, Info, error) {
				opens.Add(1)
				<-release
				return nil, nil, cause
			}}
			cache := NewCacheStore(NewMemStores().Use("t"), origin)
			d := DigestFromBytes([]byte(content))
			a, b := flightOpen(t.Context(), cache, d), flightOpen(t.Context(), cache, d)
			synctest.Wait()
			close(release)
			synctest.Wait()
			for _, r := range []flightOpenResult{flightResult(t, a), flightResult(t, b)} {
				if !errors.Is(r.err, cause) {
					t.Fatalf("open: %v; want %v", r.err, cause)
				}
			}
			if got := opens.Load(); got != 1 {
				t.Fatalf("origin opens = %d; want 1", got)
			}
		})
	})
}

func TestCacheFollowOsLeavesNoTemp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	primary := NewOsStores(root).Use("t")
	origin := NewMemStores().Use("t")
	content := strings.Repeat("a layer ", 1<<14)
	m, err := origin.Add(ctx, Meta{}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCacheStore(primary, origin)
	r, _, err := cache.Open(ctx, m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	flightRead(t, r, content)
	deadline := time.Now().Add(5 * time.Second)
	for {
		es, _ := os.ReadDir(filepath.Join(root, "stage"))
		if _, err := primary.Stat(ctx, m.Digest); err == nil && len(es) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stage still holds %d entries, or the blob is not in the primary", len(es))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
