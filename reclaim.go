package flob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	_ Reclaimer = OsStores{}
	_ Reclaimer = (*MemStores)(nil)
	_ Reclaimer = (*S3Stores)(nil)
)

// Reclaim implements [Reclaimer]. There is nothing to reclaim: a blob's bytes
// are released with its last reference.
func (s *MemStores) Reclaim(ctx context.Context, grace time.Duration) (int, error) {
	if grace <= 0 {
		return 0, errNoGrace
	}

	return 0, ctx.Err()
}

// Reclaim implements [Reclaimer]: it removes a blob in share/ that no store
// links any more -- what a crash between an Erase and its cleanup, or an Add
// that stopped after linking it, leaves -- once it is older than grace.
//
// Under the blob's lock, which every path that links a blob holds while it
// does, so it never takes one an Add is linking. And it cannot lose content a
// store still has: each store's entry is a hard link of its own, and only the
// shared link is removed.
//
// It needs the link count, which is read on Linux only; elsewhere it answers
// [ErrUnimplemented] rather than take blobs it cannot tell from shared ones.
func (i OsStores) Reclaim(ctx context.Context, grace time.Duration) (int, error) {
	if grace <= 0 {
		return 0, errNoGrace
	}
	if !nlinkKnown {
		return 0, fmt.Errorf("%w: link counts are not read on this platform", ErrUnimplemented)
	}

	before := i.stage.clock().Add(-grace)
	s := i.Use("").(OsStore)
	root := filepath.Join(i.root, "share")

	n := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
			return nil
		}
		d := Digest(parts[0] + ":" + parts[1] + parts[2] + parts[3])
		if clean, err := d.Sanitize(); err != nil || clean != d {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.ModTime().Before(before) {
			return nil
		}

		removed, err := s.tryCleanup(ctx, d)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if removed {
			n++
		}
		return nil
	})

	return n, err
}

// trashKey is where [S3Stores.Reclaim] puts a blob aside before it goes.
func (s *S3Stores) trashKey(d Digest) string {
	return s.prefix + "trash/" + d.Algorithm().String() + "/" + d.Encoded()
}

// parseKey reads the digest back out of a blob/ or trash/ key.
func (s *S3Stores) parseKey(key, under string) (Digest, bool) {
	rest, ok := strings.CutPrefix(key, s.prefix+under)
	if !ok {
		return "", false
	}
	algo, encoded, ok := strings.Cut(rest, "/")
	if !ok {
		return "", false
	}
	d, err := Digest(algo + ":" + encoded).Sanitize()
	if err != nil || string(d) != algo+":"+encoded {
		return "", false
	}
	return d, true
}

// Reclaim implements [Reclaimer]: it removes the bytes of a blob no store
// references, in two sweeps at least grace apart.
//
// # Why a blob is put aside before it goes
//
// An Add that finds the blob already in the bucket skips its upload and then
// writes its reference. A sweep that listed the references before that
// reference was written and deleted the blob after would leave a committed
// reference with no bytes -- the race [S3Store.Erase] gives as the reason not to
// delete inline -- and nothing S3 offers deletes an object only if nothing
// references it.
//
// So a sweep never deletes a blob it has just found unreferenced. It **puts it
// aside**: copies blob/ to trash/ inside the bucket, and removes blob/. A read
// of a reference whose blob/ is gone finds the bytes in trash/, so a reference
// an Add wrote in the meantime still reads. A later sweep, once trash/ has been
// there for grace, removes it if nothing references the digest by then, and
// puts it back if something does: every Add that saw the blob before it was put
// aside has written its reference by then, since none takes longer than grace,
// and an Add after it found no blob/ and uploaded its own.
//
// A blob is put aside only once it has itself been in the bucket for grace, so
// that one a slow Add has uploaded and not yet referenced is left to it.
func (s *S3Stores) Reclaim(ctx context.Context, grace time.Duration) (int, error) {
	if grace <= 0 {
		return 0, errNoGrace
	}

	before := s.stage.clock().Add(-grace)

	// Every reference, of every store, in one listing.
	refs := map[Digest]bool{}
	for entry, err := range s.listRefs(ctx, s.prefix+"refs/", "") {
		if err != nil {
			return 0, err
		}
		if d, _, ok := s.parseRefKey(entry.Key); ok {
			refs[d] = true
		}
	}

	// What an earlier sweep put aside: gone for good, or back.
	n := 0
	for entry, err := range s.listRefs(ctx, s.prefix+"trash/", "") {
		if err != nil {
			return n, err
		}
		d, ok := s.parseKey(entry.Key, "trash/")
		if !ok || !entry.LastModified.Before(before) {
			continue
		}

		if refs[d] {
			if ok, err := s.exists(ctx, s.blobKey(d)); err != nil {
				return n, err
			} else if !ok {
				if err := s.copyObject(ctx, s.blobKey(d), entry.Key, entry.Size); err != nil {
					return n, err
				}
			}
			if err := s.deleteKey(ctx, entry.Key); err != nil {
				return n, err
			}
			continue
		}

		if err := s.deleteKey(ctx, entry.Key); err != nil {
			return n, err
		}
		n++
	}

	// What nothing references now: aside.
	for entry, err := range s.listRefs(ctx, s.prefix+"blob/", "") {
		if err != nil {
			return n, err
		}
		d, ok := s.parseKey(entry.Key, "blob/")
		if !ok || refs[d] || !entry.LastModified.Before(before) {
			continue
		}

		if err := s.copyObject(ctx, s.trashKey(d), entry.Key, entry.Size); err != nil {
			return n, err
		}
		if err := s.deleteKey(ctx, entry.Key); err != nil {
			return n, err
		}
	}

	return n, nil
}

// The largest object one CopyObject copies, and the part a larger one is copied
// in. Variables so that a test can copy in parts without five gigabytes.
var (
	s3CopyLimit int64 = 5 << 30
	s3CopyPart  int64 = 1 << 30
)

// copyObject copies src to dst inside the bucket, on the service's side.
func (s *S3Stores) copyObject(ctx context.Context, dst, src string, size int64) error {
	if size > s3CopyLimit {
		return s.copyParts(ctx, dst, src, size)
	}

	req, err := s.newRequest(ctx, http.MethodPut, dst, nil, nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("X-Amz-Copy-Source", awsURIEncode("/"+s.bucket+"/"+src, false))
	res, err := s.send(req, emptyPayloadHash)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return statusError("copy", res)
	}

	// A copy can fail after the service has answered 200; what it says then
	// is in the body.
	return copyResult(res.Body)
}

// copyParts copies an object too large for one CopyObject, a range at a time.
func (s *S3Stores) copyParts(ctx context.Context, dst, src string, size int64) error {
	req, err := s.newRequest(ctx, http.MethodPost, dst, url.Values{"uploads": {""}}, nil)
	if err != nil {
		return err
	}
	res, err := s.send(req, emptyPayloadHash)
	if err != nil {
		return err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if res.StatusCode/100 != 2 {
		err = statusError("create copy multipart", res)
	} else {
		err = xml.NewDecoder(res.Body).Decode(&created)
	}
	res.Body.Close()
	if err != nil {
		return err
	}
	if created.UploadID == "" {
		return errors.New("copy multipart: no upload id")
	}

	abort := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.stage.OperationTimeout)
		defer cancel()
		if req, err := s.newRequest(cleanup, http.MethodDelete, dst, url.Values{"uploadId": {created.UploadID}}, nil); err == nil {
			if res, err := s.send(req, emptyPayloadHash); err == nil {
				res.Body.Close()
			}
		}
	}

	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	completed := struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{}
	for i, offset := 0, int64(0); offset < size; i, offset = i+1, offset+s3CopyPart {
		end := min(offset+s3CopyPart, size) - 1
		req, err := s.newRequest(ctx, http.MethodPut, dst, url.Values{"uploadId": {created.UploadID}, "partNumber": {strconv.Itoa(i + 1)}}, nil)
		if err != nil {
			abort()
			return err
		}
		req.ContentLength = 0
		req.Header.Set("X-Amz-Copy-Source", awsURIEncode("/"+s.bucket+"/"+src, false))
		req.Header.Set("X-Amz-Copy-Source-Range", fmt.Sprintf("bytes=%d-%d", offset, end))
		res, err := s.send(req, emptyPayloadHash)
		if err != nil {
			abort()
			return err
		}
		var copied struct {
			XMLName xml.Name `xml:"CopyPartResult"`
			ETag    string
		}
		if res.StatusCode/100 != 2 {
			err = statusError("copy part", res)
		} else {
			err = xml.NewDecoder(res.Body).Decode(&copied)
		}
		res.Body.Close()
		if err != nil {
			abort()
			return err
		}
		completed.Parts = append(completed.Parts, part{Number: i + 1, ETag: copied.ETag})
	}

	body, err := xml.Marshal(completed)
	if err != nil {
		abort()
		return err
	}
	req, err = s.newRequest(ctx, http.MethodPost, dst, url.Values{"uploadId": {created.UploadID}}, bytes.NewReader(body))
	if err != nil {
		abort()
		return err
	}
	req.ContentLength = int64(len(body))
	res, err = s.send(req, payloadHashOf(body))
	if err != nil {
		abort()
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		abort()
		return statusError("complete copy multipart", res)
	}
	return copyResult(res.Body)
}

// copyResult reads the body of a copy the service answered 200 for, and
// answers the error it may hold anyway.
func copyResult(r io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte("<Error>")) {
		return nil
	}

	var v struct {
		Code    string
		Message string
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("copy: %s", bytes.TrimSpace(b))
	}
	return fmt.Errorf("copy: %s: %s", v.Code, v.Message)
}

// payloadHashOf is the SigV4 payload hash of a body held in memory.
func payloadHashOf(b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
