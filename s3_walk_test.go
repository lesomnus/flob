package flob

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func walkS3Server(t *testing.T, handler http.HandlerFunc) *S3Stores {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, err := NewS3Stores(S3Config{Endpoint: srv.URL, Bucket: "bucket", Region: "us-east-1", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3WalkPages(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	deleted := DigestFromBytes([]byte("deleted"))
	ref := func(d Digest, id string) string {
		return "refs/" + d.Algorithm().String() + "/" + d.Encoded() + "/" + namespaceSegment(id)
	}
	lists, heads := 0, 0
	s := walkS3Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			lists++
			if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("encoding-type") != "url" {
				t.Error("not URL-encoded ListObjectsV2")
			}
			var keys []string
			next := ""
			switch r.URL.Query().Get("continuation-token") {
			case "":
				keys = []string{ref(d, "other"), ref(deleted, "a/b"), "refs/sha256/bad/a", "refs/sha256/" + d.Encoded() + "/~YQ"}
				next = "next+/=&"
			case "next+/=&":
				keys = []string{ref(d, "a/b")}
			default:
				t.Error("wrong token")
			}
			page := s3ListPage{EncodingType: "url", IsTruncated: next != "", NextContinuationToken: next}
			for _, key := range keys {
				page.Contents = append(page.Contents, struct{ Key string }{url.PathEscape(key)})
			}
			xml.NewEncoder(w).Encode(page)
		case http.MethodHead:
			heads++
			if strings.Contains(r.URL.Path, deleted.Encoded()) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set(metaPrefix+metaSizeKey, "5")
			w.Header().Set(metaPrefix+"owner", "value")
		default:
			t.Errorf("unexpected blob I/O: %s %s", r.Method, r.URL)
		}
	})
	count := 0
	for info, err := range s.Use("a/b").(*S3Store).Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if info.Digest() != d || info.Size() != 5 {
			t.Fatalf("Info = %v", info)
		}
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("owner") != "value" {
			t.Fatalf("labels = %v, %v", labels, err)
		}
	}
	if count != 1 || lists != 2 || heads != 2 {
		t.Fatalf("count/list/head = %d/%d/%d", count, lists, heads)
	}
	lists, heads = 0, 0
	got := map[string]bool{}
	for id, err := range s.Namespaces(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if len(got) != 2 || !got["a/b"] || !got["other"] || lists != 2 || heads != 0 {
		t.Fatalf("namespaces %v list/head %d/%d", got, lists, heads)
	}
}

func TestS3EnumerationStops(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	key := "refs/sha256/" + d.Encoded() + "/a"
	for _, mode := range []string{"break", "cancel", "missing-token", "repeated-token", "bad-xml", "http-error", "bad-escape"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s := walkS3Server(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "http-error" {
					w.WriteHeader(503)
					return
				}
				if mode == "bad-xml" {
					fmt.Fprint(w, "<broken>")
					return
				}
				page := s3ListPage{IsTruncated: true, NextContinuationToken: "same"}
				if mode == "missing-token" {
					page.NextContinuationToken = ""
				}
				if mode == "bad-escape" {
					page.EncodingType = "url"
					page.Contents = append(page.Contents, struct{ Key string }{"%bad%"})
				} else {
					page.Contents = append(page.Contents, struct{ Key string }{key})
				}
				xml.NewEncoder(w).Encode(page)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			values, errs := 0, 0
			for _, err := range s.Namespaces(ctx) {
				if err != nil {
					errs++
					continue
				}
				values++
				if mode == "break" {
					break
				}
				if mode == "cancel" {
					cancel()
				}
			}
			if mode == "break" {
				if values != 1 || errs != 0 || calls != 1 {
					t.Fatalf("break: %d/%d/%d", values, errs, calls)
				}
			} else if errs != 1 {
				t.Fatalf("errors=%d calls=%d", errs, calls)
			}
			if mode == "cancel" && calls != 1 {
				t.Fatalf("I/O after cancel: %d", calls)
			}
			if mode == "repeated-token" && calls != 2 {
				t.Fatalf("token loop: %d", calls)
			}
		})
	}
}
