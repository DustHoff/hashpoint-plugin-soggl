// Package soggl is a thin HTTP client for the internal Soggl service.
package soggl

import (
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

// Rule mirrors the subset of GET /api/rules the plugin consumes. Soggl
// also returns score, enabled, nonBillable, and ignore — those are
// Soggl-internal billing logic and irrelevant for tag_provider.
type Rule struct {
	ID                int64  `json:"id"`
	Filter            string `json:"filter"`
	SoncosoAssignment struct {
		Fragment string `json:"fragment"`
	} `json:"soncosoAssignment"`
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
	if c.token == nil {
		return errors.New("token function is nil")
	}
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("request token: %w", err)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
