// Package soggl is a thin HTTP client for the internal Soggl service.
package soggl

import (
	"context"
	"net/http"
	"time"
)

// TokenFunc returns a fresh Entra access token for use against the Soggl API.
// It is invoked per request. Implementations typically delegate to
// sdk.HostAPI.RequestEntraToken from the Hashpoint host.
type TokenFunc func(ctx context.Context) (string, error)

// Client is a thin Soggl HTTP client. Concrete API methods are added per
// feature PR.
type Client struct {
	baseURL string
	http    *http.Client
	token   TokenFunc
}

// NewClient returns a Client targeting baseURL, using token for Bearer auth.
func NewClient(baseURL string, token TokenFunc) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
		token:   token,
	}
}
