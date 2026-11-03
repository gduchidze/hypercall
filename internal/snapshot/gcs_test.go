package snapshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gduchidze/hypercall/internal/snapshot"
)

func TestGCSPublicationUsesCreatePreconditionAndServerValidatedCRC32C(t *testing.T) {
	uploaded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-host-token" {
			t.Error("missing scoped service token")
			w.WriteHeader(401)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/upload/storage/v1/b/private-bucket/o" || r.URL.Query().Get("ifGenerationMatch") != "0" || r.URL.Query().Get("uploadType") != "multipart" {
			t.Errorf("unsafe upload request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		if uploaded {
			w.WriteHeader(412)
			return
		}
		med, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || med != "multipart/related" {
			t.Errorf("multipart: %s", med)
			w.WriteHeader(400)
			return
		}
		parts := multipart.NewReader(r.Body, params["boundary"])
		metadata, err := parts.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		var obj map[string]string
		if err = json.NewDecoder(metadata).Decode(&obj); err != nil {
			t.Error(err)
			return
		}
		if obj["name"] != "private-snapshots/tenants/tenant/artifact" || obj["crc32c"] != "Nks/tw==" {
			t.Errorf("unverified upload metadata: %+v", obj)
			w.WriteHeader(400)
			return
		}
		body, err := parts.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		data, err := io.ReadAll(body)
		if err != nil || string(data) != "abc" {
			t.Errorf("upload: %q %v", data, err)
			return
		}
		uploaded = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"generation":"123","size":"3","crc32c":"Nks/tw=="}`)
	}))
	defer server.Close()
	gcs, err := snapshot.NewGCSObjects(snapshot.GCSConfig{Bucket: "private-bucket", Prefix: "private-snapshots", Endpoint: server.URL, TempDir: t.TempDir(), TokenSource: func(context.Context) (string, error) { return "private-host-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := gcs.Put(context.Background(), "tenants/tenant/artifact", strings.NewReader("abc"), 3, "")
	if err != nil || generation != "123" {
		t.Fatalf("GCS publish: %s %v", generation, err)
	}
	if _, err = gcs.Put(context.Background(), "tenants/tenant/artifact", strings.NewReader("abc"), 3, ""); !errors.Is(err, snapshot.ErrPrecondition) {
		t.Fatalf("GCS overwrote immutable object: %v", err)
	}
}

func TestGCSReadsPinnedGenerationAndRejectsCorruptDownload(t *testing.T) {
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/storage/v1/b/private-bucket/o/private-snapshots/tenants/tenant/artifact" {
			t.Errorf("get URL: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			if r.URL.Query().Get("generation") != "123" {
				t.Error("unversioned download")
			}
			if corrupt {
				io.WriteString(w, "abd")
			} else {
				io.WriteString(w, "abc")
			}
			return
		}
		io.WriteString(w, `{"generation":"123","size":"3","crc32c":"Nks/tw=="}`)
	}))
	defer server.Close()
	gcs, err := snapshot.NewGCSObjects(snapshot.GCSConfig{Bucket: "private-bucket", Prefix: "private-snapshots", Endpoint: server.URL, TokenSource: func(context.Context) (string, error) { return "private-host-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	r, generation, err := gcs.Get(context.Background(), "tenants/tenant/artifact")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || generation != "123" || string(data) != "abc" {
		t.Fatalf("read: %s %q %v", generation, data, err)
	}
	corrupt = true
	r, _, err = gcs.Get(context.Background(), "tenants/tenant/artifact")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	if !errors.Is(err, snapshot.ErrIntegrity) {
		t.Fatalf("accepted corrupt GCS download: %v", err)
	}
}

func TestGCSRetentionUsesListedGenerationAndScopesListing(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if r.URL.Query().Get("prefix") != "private-snapshots/tenants/tenant/" || r.URL.Query().Get("maxResults") != "10" || r.URL.Query().Get("pageToken") != "cursor" {
				t.Errorf("unscoped list: %s", r.URL)
			}
			io.WriteString(w, `{"items":[{"name":"private-snapshots/tenants/tenant/artifact","generation":"123"}],"nextPageToken":"next"}`)
			return
		}
		if r.Method == "DELETE" {
			if r.URL.Query().Get("ifGenerationMatch") != "123" {
				t.Error("unconditional deletion")
			}
			deleted = true
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(400)
	}))
	defer server.Close()
	gcs, err := snapshot.NewGCSObjects(snapshot.GCSConfig{Bucket: "private-bucket", Prefix: "private-snapshots", Endpoint: server.URL, TokenSource: func(context.Context) (string, error) { return "private-host-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	objects, next, err := gcs.List(context.Background(), "tenants/tenant/", "cursor", 10)
	if err != nil || len(objects) != 1 || next != "next" {
		t.Fatalf("list: %+v %s %v", objects, next, err)
	}
	if err = gcs.Delete(context.Background(), objects[0].Key, objects[0].Generation); err != nil || !deleted {
		t.Fatalf("delete: %v", err)
	}
}
