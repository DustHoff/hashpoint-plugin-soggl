// Package soggl is a thin HTTP client for the internal Soggl service.
package soggl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenFunc returns a fresh Entra access token for use against the Soggl API.
// It is invoked per request. Implementations typically delegate to
// sdk.HostAPI.RequestEntraToken from the Hashpoint host.
type TokenFunc func(ctx context.Context) (string, error)

// Client is a thin Soggl HTTP client.
type Client struct {
	baseURL string
	http    *http.Client
	token   TokenFunc
}

// NewClient returns a Client targeting baseURL, using token for Bearer auth.
// A trailing slash on baseURL is tolerated.
func NewClient(baseURL string, token TokenFunc) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
		token:   token,
	}
}

// NonBillable mirrors the Soggl rule.nonBillable sub-object. Pattern is
// the regex Soggl applies to mark records as non-billable; the SDK's
// defaultToNonBillable governs the opposite default. Both round-trip
// untouched when the plugin updates a rule.
type NonBillable struct {
	Pattern              string `json:"pattern"`
	DefaultToNonBillable bool   `json:"defaultToNonBillable"`
}

// Ignore mirrors the Soggl rule.ignore sub-object. Pattern is nullable
// in the wire format (most rules in the wild have it as JSON null) so it
// is modelled as *string — sending a non-nil empty string would be a
// behaviour change Soggl interprets as "match empty".
type Ignore struct {
	Pattern         *string `json:"pattern"`
	DefaultToIgnore bool    `json:"defaultToIgnore"`
}

// SoncosoAssignment mirrors the Soggl rule.soncosoAssignment sub-object.
// Fragment is the free-text order identifier the plugin overwrites from
// the Hashpoint TagOrderMapping.OrderName on every sync.
type SoncosoAssignment struct {
	Fragment string `json:"fragment"`
}

// Rule mirrors a Soggl rule as returned by GET /api/rules. Every field
// is round-tripped on Update so the "preserve all Soggl-owned fields"
// contract holds: only Filter and SoncosoAssignment.Fragment are
// authoritative on the Hashpoint side; the rest stays whatever the user
// or a previous sync left in Soggl.
//
// AutoIgnoreTimelessRecords is observed only on some PUT bodies in the
// Soggl UI's wire format; it is modelled as *bool so a rule that came
// from a GET without the field round-trips back without it (a literal
// false would be a wire-format change Soggl might treat differently).
type Rule struct {
	ID                        int64             `json:"id"`
	Enabled                   bool              `json:"enabled"`
	Score                     int               `json:"score"`
	Filter                    string            `json:"filter"`
	NonBillable               NonBillable       `json:"nonBillable"`
	Ignore                    Ignore            `json:"ignore"`
	SoncosoAssignment         SoncosoAssignment `json:"soncosoAssignment"`
	AutoIgnoreTimelessRecords *bool             `json:"autoIgnoreTimelessRecords,omitempty"`
}

// Job mirrors the subset of GET /api/soncoso/jobs the plugin consumes.
type Job struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Customer string `json:"customer"`
}

// ListRules returns every rule configured in Soggl.
func (c *Client) ListRules(ctx context.Context) ([]Rule, error) {
	var out []Rule
	if err := c.getJSON(ctx, "/api/rules", nil, &out); err != nil {
		return nil, fmt.Errorf("soggl: list rules: %w", err)
	}
	return out, nil
}

// CreateRule POSTs r as a new Soggl rule. r.ID is ignored — Soggl
// assigns the id server-side and returns it on the response, which is
// what the returned Rule carries. The wire body sends "id":null
// verbatim (mirroring the Soggl webapp), so a separate body type with a
// nullable id pointer is used instead of zero-value omission.
func (c *Client) CreateRule(ctx context.Context, r Rule) (Rule, error) {
	type createBody struct {
		ID                *int64            `json:"id"`
		Enabled           bool              `json:"enabled"`
		Score             int               `json:"score"`
		Filter            string            `json:"filter"`
		NonBillable       NonBillable       `json:"nonBillable"`
		Ignore            Ignore            `json:"ignore"`
		SoncosoAssignment SoncosoAssignment `json:"soncosoAssignment"`
	}
	body := createBody{
		ID:                nil,
		Enabled:           r.Enabled,
		Score:             r.Score,
		Filter:            r.Filter,
		NonBillable:       r.NonBillable,
		Ignore:            r.Ignore,
		SoncosoAssignment: r.SoncosoAssignment,
	}
	var out Rule
	if err := c.sendJSON(ctx, http.MethodPost, "/api/rules", body, &out); err != nil {
		return Rule{}, fmt.Errorf("soggl: create rule: %w", err)
	}
	return out, nil
}

// UpdateRule PUTs r against /api/rules/{r.ID}. The full Rule struct is
// sent; preserve Soggl-owned fields by reading the rule first and only
// overwriting the fields the plugin owns (Filter, fragment, Enabled).
func (c *Client) UpdateRule(ctx context.Context, r Rule) error {
	if r.ID == 0 {
		return errors.New("soggl: update rule: id is zero")
	}
	path := fmt.Sprintf("/api/rules/%d", r.ID)
	if err := c.sendJSON(ctx, http.MethodPut, path, r, nil); err != nil {
		return fmt.Errorf("soggl: update rule %d: %w", r.ID, err)
	}
	return nil
}

// ListJobs returns Soncoso jobs intersecting the [start, end] date range
// (inclusive on both bounds, matching the Soggl webapp's own query
// shape). Dates are formatted YYYY-MM-DD in the location of the
// supplied values; pass local-zone times for the Soggl convention.
func (c *Client) ListJobs(ctx context.Context, start, end time.Time) ([]Job, error) {
	q := url.Values{}
	q.Set("start", start.Format("2006-01-02"))
	q.Set("end", end.Format("2006-01-02"))
	var out []Job
	if err := c.getJSON(ctx, "/api/soncoso/jobs", q, &out); err != nil {
		return nil, fmt.Errorf("soggl: list jobs: %w", err)
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, dst any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.do(req, dst)
}

// sendJSON marshals body as JSON and issues an authenticated method
// request to path. If dst is non-nil the response body is decoded into
// it; otherwise the body is drained and discarded.
func (c *Client) sendJSON(ctx context.Context, method, path string, body, dst any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	req, err := c.newRequest(ctx, method, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, dst)
}

func (c *Client) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	if c.token == nil {
		return nil, errors.New("token function is nil")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("request token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (c *Client) do(req *http.Request, dst any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if dst == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
