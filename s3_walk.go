package flob

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strings"
)

var _ Walker = (*S3Store)(nil)
var _ Namespacer = (*S3Stores)(nil)

type s3ListPage struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	EncodingType          string
	IsTruncated           bool
	NextContinuationToken string
	Contents              []struct{ Key string }
}

// refKeys lists reference objects without reading blob bodies or metadata.
func (s *S3Stores) refKeys(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		token := ""
		seen := map[string]bool{}
		for {
			if err := ctx.Err(); err != nil {
				yield("", err)
				return
			}
			q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + "refs/"}, "encoding-type": {"url"}}
			if token != "" {
				q.Set("continuation-token", token)
			}
			req, err := s.newRequest(ctx, http.MethodGet, "", q, nil)
			if err != nil {
				yield("", err)
				return
			}
			res, err := s.send(req, emptyPayloadHash)
			if err != nil {
				yield("", err)
				return
			}
			if res.StatusCode != http.StatusOK {
				err = statusError("list refs", res)
				res.Body.Close()
				yield("", err)
				return
			}
			var page s3ListPage
			err = xml.NewDecoder(res.Body).Decode(&page)
			res.Body.Close()
			if err != nil {
				yield("", fmt.Errorf("decode list refs: %w", err))
				return
			}
			if page.EncodingType != "" && page.EncodingType != "url" {
				yield("", fmt.Errorf("unsupported list encoding %q", page.EncodingType))
				return
			}
			for _, obj := range page.Contents {
				if err := ctx.Err(); err != nil {
					yield("", err)
					return
				}
				key := obj.Key
				if page.EncodingType == "url" {
					key, err = url.PathUnescape(key)
					if err != nil {
						yield("", fmt.Errorf("decode ref key: %w", err))
						return
					}
				}
				if !yield(key, nil) {
					return
				}
				if err := ctx.Err(); err != nil {
					yield("", err)
					return
				}
			}
			if !page.IsTruncated {
				return
			}
			token = page.NextContinuationToken
			if token == "" || seen[token] {
				yield("", errors.New("invalid list continuation token"))
				return
			}
			seen[token] = true
		}
	}
}

func (s *S3Stores) parseRefKey(key string) (Digest, string, bool) {
	rest, ok := strings.CutPrefix(key, s.prefix+"refs/")
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 3 {
		return "", "", false
	}
	d, err := Digest(parts[0] + ":" + parts[1]).Sanitize()
	if err != nil || string(d) != parts[0]+":"+parts[1] {
		return "", "", false
	}
	id, err := namespaceID(parts[2])
	if err != nil || namespaceSegment(id) != parts[2] {
		return "", "", false
	}
	return d, id, true
}

func (s *S3Store) Walk(ctx context.Context) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		for key, err := range s.stores.refKeys(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			d, id, ok := s.stores.parseRefKey(key)
			if !ok || id != s.id {
				continue
			}
			info, err := s.Stat(ctx, d)
			if errors.Is(err, ErrNotExist) {
				continue
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(info, nil) {
				return
			}
		}
	}
}

func (s *S3Stores) Namespaces(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		seen := map[string]bool{}
		for key, err := range s.refKeys(ctx) {
			if err != nil {
				yield("", err)
				return
			}
			_, id, ok := s.parseRefKey(key)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			if !yield(id, nil) {
				return
			}
		}
	}
}
