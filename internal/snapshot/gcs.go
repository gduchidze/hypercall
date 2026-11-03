package snapshot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// GCSConfig injects a host-only OAuth credential source, scoped through IAM to
// the configured bucket/prefix. No guest or manifest receives these credentials.
// Endpoint is for protocol emulators; production uses the default Google URL.
type GCSConfig struct {
	Bucket         string
	Prefix         string
	TokenSource    func(context.Context) (string, error)
	Client         *http.Client
	Endpoint       string
	TempDir        string
	RequestTimeout time.Duration
}
type GCSObjects struct {
	cfg      GCSConfig
	client   *http.Client
	endpoint string
}

func NewGCSObjects(cfg GCSConfig) (*GCSObjects, error) {
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\?#\r\n\x00") || len(cfg.Bucket) > 222 || !validKey(cfg.Prefix) || cfg.TokenSource == nil {
		return nil, ErrInvalid
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://storage.googleapis.com"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, ErrInvalid
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return nil, ErrInvalid
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Minute
	}
	if cfg.RequestTimeout < time.Millisecond {
		return nil, ErrInvalid
	}
	c := http.Client{}
	if cfg.Client != nil {
		c = *cfg.Client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &GCSObjects{cfg: cfg, client: &c, endpoint: cfg.Endpoint}, nil
}
func (g *GCSObjects) objectName(key string) string { return g.cfg.Prefix + "/" + key }
func (g *GCSObjects) objectURL(key string) string {
	return g.endpoint + "/storage/v1/b/" + url.PathEscape(g.cfg.Bucket) + "/o/" + url.PathEscape(g.objectName(key))
}
func (g *GCSObjects) request(ctx context.Context, method, address string, body io.Reader, contentType string, length int64) (*http.Response, error) {
	token, err := g.cfg.TokenSource(ctx)
	if err != nil {
		return nil, errors.New("GCS host credentials unavailable")
	}
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("GCS host credentials invalid")
	}
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept-Encoding", "identity")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if body != nil {
		req.ContentLength = length
	}
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("GCS transport failed")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case 404:
		return nil, ErrNotFound
	case 412:
		return nil, ErrPrecondition
	default:
		return nil, fmt.Errorf("GCS request rejected (HTTP %d)", resp.StatusCode)
	}
}

type gcsMetadata struct {
	Name       string `json:"name,omitempty"`
	Generation string `json:"generation,omitempty"`
	Size       string `json:"size,omitempty"`
	CRC32C     string `json:"crc32c,omitempty"`
}

func crcEncoded(h hash.Hash32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], h.Sum32())
	return base64.StdEncoding.EncodeToString(b[:])
}
func generationValid(g string) bool {
	n, e := strconv.ParseUint(g, 10, 64)
	return e == nil && n > 0 && strconv.FormatUint(n, 10) == g
}
func decodeMetadata(r io.Reader) (gcsMetadata, error) {
	var m gcsMetadata
	err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&m)
	if err != nil || !generationValid(m.Generation) {
		return m, ErrIntegrity
	}
	if _, err = strconv.ParseInt(m.Size, 10, 64); err != nil {
		return m, ErrIntegrity
	}
	b, err := base64.StdEncoding.DecodeString(m.CRC32C)
	if err != nil || len(b) != 4 {
		return m, ErrIntegrity
	}
	return m, nil
}
func (g *GCSObjects) Put(ctx context.Context, key string, src io.Reader, size int64, expected string) (string, error) {
	if !validKey(key) || size < 0 || size == int64(^uint64(0)>>1) || (expected != "" && !generationValid(expected)) {
		return "", ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.RequestTimeout)
	defer cancel()
	// Spool once to bound memory and send the expected CRC in object metadata.
	// This is a full baseline upload, deliberately without multipart composition.
	f, err := os.CreateTemp(g.cfg.TempDir, "gcs-upload-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, err := io.Copy(io.MultiWriter(f, h), &contextReader{ctx: ctx, r: io.LimitReader(src, size+1)})
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrIntegrity
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	metadata, _ := json.Marshal(gcsMetadata{Name: g.objectName(key), CRC32C: crcEncoded(h)})
	var frame bytes.Buffer
	w := multipart.NewWriter(&frame)
	p, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": []string{"application/json; charset=UTF-8"}})
	if err != nil {
		return "", err
	}
	p.Write(metadata)
	_, err = w.CreatePart(textproto.MIMEHeader{"Content-Type": []string{"application/octet-stream"}})
	if err != nil {
		return "", err
	}
	prefix := append([]byte(nil), frame.Bytes()...)
	offset := frame.Len()
	if err = w.Close(); err != nil {
		return "", err
	}
	suffix := append([]byte(nil), frame.Bytes()[offset:]...)
	match := expected
	if match == "" {
		match = "0"
	}
	q := url.Values{"uploadType": []string{"multipart"}, "ifGenerationMatch": []string{match}}
	address := g.endpoint + "/upload/storage/v1/b/" + url.PathEscape(g.cfg.Bucket) + "/o?" + q.Encode()
	resp, err := g.request(ctx, http.MethodPost, address, io.MultiReader(bytes.NewReader(prefix), f, bytes.NewReader(suffix)), "multipart/related; boundary="+w.Boundary(), int64(len(prefix))+size+int64(len(suffix)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	m, err := decodeMetadata(resp.Body)
	if err != nil {
		return "", err
	}
	if m.Size != strconv.FormatInt(size, 10) || m.CRC32C != crcEncoded(h) {
		return "", ErrIntegrity
	}
	return m.Generation, nil
}

func (g *GCSObjects) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	if !validKey(key) {
		return nil, "", ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.RequestTimeout)
	resp, err := g.request(ctx, http.MethodGet, g.objectURL(key), nil, "", 0)
	if err != nil {
		cancel()
		return nil, "", err
	}
	m, err := decodeMetadata(resp.Body)
	resp.Body.Close()
	if err != nil {
		cancel()
		return nil, "", err
	}
	size, _ := strconv.ParseInt(m.Size, 10, 64)
	if size < 0 {
		cancel()
		return nil, "", ErrIntegrity
	}
	address := g.objectURL(key) + "?" + url.Values{"alt": []string{"media"}, "generation": []string{m.Generation}}.Encode()
	resp, err = g.request(ctx, http.MethodGet, address, nil, "", 0)
	if err != nil {
		cancel()
		return nil, "", err
	}
	return &crcReader{body: resp.Body, h: crc32.New(crc32.MakeTable(crc32.Castagnoli)), expected: m.CRC32C, size: size, cancel: cancel}, m.Generation, nil
}

type crcReader struct {
	body     io.ReadCloser
	h        hash.Hash32
	expected string
	size, n  int64
	cancel   context.CancelFunc
	done     bool
}

func (r *crcReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	n, err := r.body.Read(p)
	r.n += int64(n)
	r.h.Write(p[:n])
	if r.n > r.size {
		r.done = true
		return n, ErrIntegrity
	}
	if err == io.EOF {
		r.done = true
		if r.n != r.size || crcEncoded(r.h) != r.expected {
			return n, ErrIntegrity
		}
	}
	return n, err
}
func (r *crcReader) Close() error { r.cancel(); return r.body.Close() }

func (g *GCSObjects) Delete(ctx context.Context, key, expected string) error {
	if !validKey(key) || !generationValid(expected) {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.RequestTimeout)
	defer cancel()
	resp, err := g.request(ctx, http.MethodDelete, g.objectURL(key)+"?"+url.Values{"ifGenerationMatch": []string{expected}}.Encode(), nil, "", 0)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
func (g *GCSObjects) List(ctx context.Context, prefix, cursor string, limit int) ([]ObjectInfo, string, error) {
	if prefix != "" && !validKey(strings.TrimSuffix(prefix, "/")) {
		return nil, "", ErrInvalid
	}
	if limit < 1 || limit > 1000 || len(cursor) > 8192 {
		return nil, "", ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.RequestTimeout)
	defer cancel()
	scoped := g.cfg.Prefix + "/" + prefix
	q := url.Values{"prefix": []string{scoped}, "maxResults": []string{strconv.Itoa(limit)}, "fields": []string{"items(name,generation),nextPageToken"}}
	if cursor != "" {
		q.Set("pageToken", cursor)
	}
	resp, err := g.request(ctx, http.MethodGet, g.endpoint+"/storage/v1/b/"+url.PathEscape(g.cfg.Bucket)+"/o?"+q.Encode(), nil, "", 0)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var page struct {
		Items         []gcsMetadata `json:"items"`
		NextPageToken string        `json:"nextPageToken"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&page); err != nil {
		return nil, "", ErrIntegrity
	}
	if len(page.Items) > limit || len(page.NextPageToken) > 8192 {
		return nil, "", ErrIntegrity
	}
	result := make([]ObjectInfo, 0, len(page.Items))
	for _, item := range page.Items {
		if !strings.HasPrefix(item.Name, scoped) || !generationValid(item.Generation) {
			return nil, "", ErrIntegrity
		}
		key := strings.TrimPrefix(item.Name, g.cfg.Prefix+"/")
		if !validKey(key) {
			return nil, "", ErrIntegrity
		}
		result = append(result, ObjectInfo{Key: key, Generation: item.Generation})
	}
	return result, page.NextPageToken, nil
}

var _ Objects = (*GCSObjects)(nil)
var _ Objects = (*FileObjects)(nil)
