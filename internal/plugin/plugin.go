// Package plugin wires the Soggl client into the Hashpoint plugin SDK.
package plugin

import (
	"context"
	"errors"
	"sync"

	sdk "github.com/dusthoff/hashpoint/plugin/sdk"

	"github.com/dusthoff/hashpoint-plugin-soggl/internal/soggl"
)

const (
	Name    = "hashpoint-plugin-soggl"
	Version = "1.0.0"

	cfgEntraScope = "entra_scope"
	cfgSogglHost  = "soggl_host"
)

// Plugin implements sdk.Plugin and sdk.TagProviderHandler.
type Plugin struct {
	mu     sync.RWMutex
	host   sdk.HostAPI
	client *soggl.Client
}

// New returns a Plugin ready to pass to sdk.Serve.
func New() *Plugin { return &Plugin{} }

func (p *Plugin) Init(_ context.Context, host sdk.HostAPI) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = host
	return nil
}

func (p *Plugin) Metadata(_ context.Context) (sdk.Metadata, error) {
	return sdk.Metadata{
		Name:         Name,
		Version:      Version,
		APIVersion:   sdk.HostAPIVersion,
		Capabilities: []sdk.Capability{sdk.CapTagProvider},
		Description:  "Tag provider bridging Hashpoint with the internal Soggl application.",
	}, nil
}

func (p *Plugin) Configure(_ context.Context, cfg sdk.PluginConfig) error {
	scope := cfg.Fields[cfgEntraScope]
	hostURL := cfg.Fields[cfgSogglHost]
	if scope == "" || hostURL == "" {
		return sdk.ErrConfigInvalid
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil {
		return errors.New("plugin: Configure called before Init")
	}
	p.client = soggl.NewClient(hostURL, tokenFunc(p.host, scope))
	return nil
}

func (p *Plugin) ListTags(_ context.Context) ([]sdk.ImportedTag, error) {
	if p.snapshot() == nil {
		return nil, sdk.ErrNotConfigured
	}
	return nil, errors.New("soggl: ListTags not implemented")
}

func (p *Plugin) ListOrders(_ context.Context) ([]sdk.Order, error) {
	if p.snapshot() == nil {
		return nil, sdk.ErrNotConfigured
	}
	return nil, errors.New("soggl: ListOrders not implemented")
}

func (p *Plugin) snapshot() *soggl.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.client
}

func tokenFunc(host sdk.HostAPI, scope string) soggl.TokenFunc {
	return func(ctx context.Context) (string, error) {
		token, _, err := host.RequestEntraToken(ctx, []string{scope})
		return token, err
	}
}
