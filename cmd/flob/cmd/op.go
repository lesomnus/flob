package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdHash() *xli.Command {
	return &xli.Command{
		Name: "hash",

		Args: arg.Args{
			&ArgReader{Name: "FILE"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			r, err := arg.MustGet[ReaderResolver](cmd, "FILE").Resolve(cmd)
			if err != nil {
				return z.Err(err, "resolve file")
			}
			defer r.Close()

			h := sha256.New()
			if _, err := io.Copy(h, r); err != nil {
				return z.Err(err, "hash")
			}

			cmd.Println(hex.EncodeToString(h.Sum(nil)))
			return next(ctx)
		}),
	}
}

func NewCmdAdd() *xli.Command {
	return &xli.Command{
		Name:  "add",
		Brief: "add one or more blobs to a store",

		Flags: flg.Flags{
			&flg.Int{
				Name:  "parallel",
				Alias: 'p',
				Brief: "how many blobs to add at once",
			},
		},

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&ArgReaders{Name: "FILE"},
		},

		Handler: useClientStore(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			files, _ := arg.Get[[]ReaderResolver](cmd, "FILE")
			if len(files) == 0 {
				return errors.New("at least one FILE is required")
			}

			parallel, _ := flg.Get[int](cmd, "parallel")
			if parallel < 1 {
				parallel = 1
			}
			// Standard input can only be read once, and reading it from two
			// goroutines would interleave two blobs into one.
			for _, f := range files {
				if f == "-" {
					parallel = 1
					break
				}
			}

			rs := addAll(ctx, s.Use(id), cmd, files, parallel)

			// In the order they were given, whatever order they finished in: a
			// caller pairing digests with file names reads them positionally.
			//
			// A file that failed prints nothing, even though the digest was
			// computed on the way to failing — printing it would say the store
			// has bytes it does not. That breaks the positional pairing, which
			// is the honest outcome: the command is about to exit non-zero.
			for _, r := range rs {
				if r.digest == "" {
					continue
				}
				if r.err != nil && !errors.Is(r.err, flob.ErrAlreadyExists) {
					continue
				}
				cmd.Println(r.digest)
			}

			return addOutcome(rs)
		}),
	}
}

// addOutcome reduces one result per file to the single error the process exits
// with.
//
// It reports *every* failure, not the first one: [addAll] deliberately keeps
// going after a file fails, and stopping here would throw that away — a run
// that uploads hundreds of blobs would name one bad file per re-run instead of
// all of them in one pass.
func addOutcome(rs []addResult) error {
	existed := 0
	var errs []error
	for _, r := range rs {
		switch {
		case r.err == nil:
		case errors.Is(r.err, flob.ErrAlreadyExists):
			// Not a failure. The store is content-addressed, so a blob that is
			// already there is the end state this asked for, which is what
			// makes re-running a publish cheap.
			existed++
		default:
			errs = append(errs, r.err)
		}
	}
	if len(errs) > 0 {
		return z.Err(errors.Join(errs...), "op")
	}
	if len(rs) > 0 && existed == len(rs) {
		// Every input was already there. Reported as such so a single add
		// still exits with ExitAlreadyExists, and so a batch can tell
		// "nothing to do" from "something was published".
		return flob.ErrAlreadyExists
	}
	return nil
}

type addResult struct {
	digest flob.Digest
	err    error
}

// addAll adds each file, at most parallel at a time, and returns one result per
// input in input order.
//
// It does not stop at the first failure. A publish run wants to know everything
// that went wrong in one pass rather than one failure per re-run, and the
// caller decides what the collection means.
func addAll(ctx context.Context, store flob.Store, stdin io.Reader, files []ReaderResolver, parallel int) []addResult {
	rs := make([]addResult, len(files))

	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			r, err := f.Resolve(stdin)
			if err != nil {
				rs[i].err = fmt.Errorf("resolve file %q: %w", string(f), err)
				return
			}
			defer r.Close()

			m, err := store.Add(ctx, flob.Meta{}, r)
			rs[i].digest = m.Digest
			if err != nil {
				rs[i].err = fmt.Errorf("%s: %w", string(f), err)
			}
		}()
	}
	wg.Wait()

	return rs
}

func NewCmdGet() *xli.Command {
	return &xli.Command{
		Name: "get",

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&ArgDigest{Name: "DIGEST"},
		},

		Handler: useClientStore(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			d, err := arg.MustGet[DigestResolver](cmd, "DIGEST").Resolve(cmd)
			if err != nil {
				return z.Err(err, "resolve digest")
			}

			info, err := s.Use(id).Stat(ctx, d)
			if err != nil {
				return z.Err(err, "op")
			}

			size, err := info.Size(ctx)
			if err != nil {
				return z.Err(err, "size")
			}

			labels, err := info.Labels(ctx)
			if err != nil {
				return z.Err(err, "labels")
			}

			cmd.Println("Digest:", info.Digest())
			cmd.Println("Size:", size)
			if len(labels) > 0 {
				cmd.Println("Labels:")
				for k, v := range labels {
					cmd.Println("  ", k, "=", v)
				}
			}
			return nil
		}),
	}
}

func NewCmdRead() *xli.Command {
	return &xli.Command{
		Name: "read",

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&ArgDigest{Name: "DIGEST"},
		},

		Handler: useClientStore(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			d, err := arg.MustGet[DigestResolver](cmd, "DIGEST").Resolve(cmd)
			if err != nil {
				return z.Err(err, "resolve digest")
			}

			f, _, err := s.Use(id).Open(ctx, d)
			if err != nil {
				return z.Err(err, "op")
			}
			defer f.Close()

			_, err = io.Copy(cmd, f)
			return err
		}),
	}
}

func NewCmdErase() *xli.Command {
	return &xli.Command{
		Name: "erase",
		Aliases: []string{
			"remove", "rm",
		},

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&ArgDigest{Name: "DIGEST"},
		},

		Handler: useClientStore(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			d, err := arg.MustGet[DigestResolver](cmd, "DIGEST").Resolve(cmd)
			if err != nil {
				return z.Err(err, "resolve digest")
			}

			if err = s.Use(id).Erase(ctx, d); err != nil {
				return z.Err(err, "op")
			}
			return err
		}),
	}
}
