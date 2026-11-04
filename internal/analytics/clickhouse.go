// Package analytics maintains rebuildable projections of authoritative Postgres events.
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

// Event is the versioned envelope shared by branch events and evaluation reports.
// EventID includes a source prefix; scope remains part of the immutable identity.
type Event struct {
	EventID       string    `json:"event_id"`
	TenantID      string    `json:"tenant_id"`
	ProjectID     string    `json:"project_id"`
	Kind          string    `json:"kind"`
	ResourceID    string    `json:"resource_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	PayloadJSON   string    `json:"payload_json"`
	SchemaVersion uint32    `json:"schema_version"`
}

type Sink interface {
	Write(context.Context, Event) error
}

type ClickHouseConfig struct {
	Endpoint              string
	Database              string
	Username              string
	Password              string
	AllowInsecureLoopback bool
	Timeout               time.Duration
}

type ClickHouse struct {
	endpoint                     *url.URL
	database, username, password string
	client                       *http.Client
}

func NewClickHouse(c ClickHouseConfig) (*ClickHouse, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("ClickHouse endpoint must be an origin without credentials, query or path")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback && c.AllowInsecureLoopback) {
		return nil, errors.New("ClickHouse requires HTTPS; explicit loopback HTTP is allowed for local tests")
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`).MatchString(c.Database) {
		return nil, errors.New("invalid ClickHouse database")
	}
	if c.Username == "" || c.Password == "" {
		return nil, errors.New("ClickHouse username and password are required")
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Timeout < time.Millisecond || c.Timeout > 20*time.Second {
		return nil, errors.New("ClickHouse timeout must be 1ms..20s")
	}
	return &ClickHouse{u, c.Database, c.Username, c.Password, &http.Client{Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *ClickHouse) request(ctx context.Context, query string, params url.Values, body []byte) ([]byte, error) {
	u := *c.endpoint
	q := u.Query()
	q.Set("database", c.database)
	q.Set("wait_end_of_query", "1")
	q.Set("query", query)
	q.Set("date_time_input_format", "best_effort")
	q.Set("date_time_output_format", "iso")
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, errors.New("ClickHouse request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return nil, errors.New("ClickHouse response could not be read")
	}
	if len(data) > 4<<20 {
		return nil, errors.New("ClickHouse response exceeds 4 MiB")
	}
	// Server response text can contain SQL or event payloads; never return it in logs.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ClickHouse returned HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func (c *ClickHouse) Write(ctx context.Context, e Event) error {
	if e.EventID == "" || e.TenantID == "" || e.ProjectID == "" || e.Kind == "" || e.ResourceID == "" || e.OccurredAt.IsZero() || e.SchemaVersion != 1 || !json.Valid([]byte(e.PayloadJSON)) {
		return errors.New("invalid projection envelope")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("projection envelope exceeds 4 MiB")
	}
	_, err = c.request(ctx, "INSERT INTO hypercall_events FORMAT JSONEachRow", nil, append(b, '\n'))
	return err
}

// Events uses FINAL to collapse at-least-once retries before background merges.
// Callers must pass a scope derived from authentication, never an untrusted tenant.
func (c *ClickHouse) Events(ctx context.Context, scope cp.Scope, limit int) ([]Event, error) {
	if scope.TenantID == "" || scope.ProjectID == "" || limit < 1 || limit > 1000 {
		return nil, errors.New("scope and limit 1..1000 are required")
	}
	b, err := c.request(ctx, `SELECT event_id,tenant_id,project_id,kind,resource_id,occurred_at,payload_json,schema_version FROM hypercall_events FINAL WHERE tenant_id={tenant:String} AND project_id={project:String} ORDER BY occurred_at,event_id LIMIT {limit:UInt32} FORMAT JSONEachRow`, url.Values{"param_tenant": {scope.TenantID}, "param_project": {scope.ProjectID}, "param_limit": {fmt.Sprint(limit)}}, nil)
	if err != nil {
		return nil, err
	}
	result := []Event{}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var e Event
		if err = decoder.Decode(&e); errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("invalid ClickHouse event response: %w", err)
		}
		result = append(result, e)
	}
}
