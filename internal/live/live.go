// Package live holds the configuration IN EFFECT: the config file (base)
// plus the changes saved in the dashboard's Config tab (database revisions).
//
// Rules that keep this safe:
//   - the file is the base and is never changed by the dashboard: providers,
//     aliases and groups defined there are read-only in the UI;
//   - a change is validated against the whole merged config BEFORE it is
//     saved (same Validate as the startup), so a saved revision always works;
//   - every replica polls the last revision id and swaps the config
//     atomically; a request keeps the snapshot it started with;
//   - provider API keys live in AWS Secrets Manager: the database stores
//     only the ARN, the key stays in memory and is re-read periodically.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chwiee/nautilm/internal/config"
	"github.com/chwiee/nautilm/internal/pricing"
	"github.com/chwiee/nautilm/internal/provider"
	"github.com/chwiee/nautilm/internal/store"
	"github.com/chwiee/nautilm/pkg/openai"
)

// ProviderDef is a provider added from the dashboard.
type ProviderDef struct {
	Type    string `json:"type"`               // bedrock | openai
	Region  string `json:"region,omitempty"`   // bedrock
	BaseURL string `json:"base_url,omitempty"` // openai (or "base_url" inside the secret)
	// SecretARN (openai): Secrets Manager secret with {"api_key": "...",
	// "base_url": "..."} or the key as plain text. Empty = no key (an
	// internal endpoint without authentication).
	SecretARN      string `json:"secret_arn,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Note           string `json:"note,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
}

// ModelDef is an alias defined or overridden from the dashboard.
type ModelDef struct {
	Type       string   `json:"type"` // chat | embedding
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	MaxTokens  int      `json:"max_tokens,omitempty"`
	Dimensions int      `json:"dimensions,omitempty"`
	Fallback   []string `json:"fallback,omitempty"`
	// PromptCache: off | explicit | auto (see config.Model).
	PromptCache string `json:"prompt_cache,omitempty"`
}

// PriceDef is a price entry added from the dashboard (USD per 1M tokens).
type PriceDef struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read,omitempty"`
	CacheWrite    float64 `json:"cache_write,omitempty"`
	Free          bool    `json:"free,omitempty"`
	EffectiveFrom string  `json:"effective_from,omitempty"`
}

// Group is an Entra group mapped to a role (Name is only a label).
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Groups adds Entra groups to the roles. The groups of the config file are
// always kept (break-glass): the dashboard can add, never lock everyone out.
type Groups struct {
	Admin  []Group `json:"admin"`
	Viewer []Group `json:"viewer"`
}

// Overrides are the dashboard's changes, by kind.
type Overrides struct {
	Providers map[string]ProviderDef `json:"providers"`
	Models    map[string]ModelDef    `json:"models"`
	Pricing   []PriceDef             `json:"pricing"`
	Groups    Groups                 `json:"groups"`
	// Revisions in effect per kind (0 = nothing saved for that kind).
	Revisions map[string]int64 `json:"revisions"`
}

func (o Overrides) clone() Overrides {
	c := Overrides{Providers: maps.Clone(o.Providers), Models: maps.Clone(o.Models), Pricing: slices.Clone(o.Pricing),
		Groups: Groups{Admin: slices.Clone(o.Groups.Admin), Viewer: slices.Clone(o.Groups.Viewer)}, Revisions: maps.Clone(o.Revisions)}
	if c.Providers == nil {
		c.Providers = map[string]ProviderDef{}
	}
	if c.Models == nil {
		c.Models = map[string]ModelDef{}
	}
	if c.Revisions == nil {
		c.Revisions = map[string]int64{}
	}
	return c
}

// With returns a copy with one kind replaced by payload (a candidate change).
func (o Overrides) With(kind string, payload json.RawMessage) (Overrides, error) {
	c := o.clone()
	var err error
	switch kind {
	case "providers":
		c.Providers = map[string]ProviderDef{}
		err = json.Unmarshal(payload, &c.Providers)
	case "models":
		c.Models = map[string]ModelDef{}
		err = json.Unmarshal(payload, &c.Models)
	case "pricing":
		c.Pricing = nil
		err = json.Unmarshal(payload, &c.Pricing)
	case "groups":
		c.Groups = Groups{}
		err = json.Unmarshal(payload, &c.Groups)
	default:
		return c, fmt.Errorf("unknown kind %q", kind)
	}
	if err != nil {
		return c, fmt.Errorf("%s: invalid payload: %w", kind, err)
	}
	return c, nil
}

// Decode builds the overrides from the newest revision of each kind.
func Decode(latest map[string]store.Revision) (Overrides, error) {
	o := Overrides{}.clone()
	for _, kind := range store.RevisionKinds {
		r, ok := latest[kind]
		if !ok {
			continue
		}
		var err error
		if o, err = o.With(kind, r.Payload); err != nil {
			return o, fmt.Errorf("revision %d: %w", r.ID, err)
		}
		o.Revisions[kind] = r.ID
	}
	return o, nil
}

// Key is what a provider secret holds.
type Key struct {
	APIKey  string
	BaseURL string
}

// unavailable marks a provider whose secret could not be read: the config
// stays valid and only that provider fails (with a clear error).
const unavailable = "unavailable://secret"

// Merge applies the overrides on a copy of the base and validates the result.
func Merge(base *config.Config, o Overrides, keys map[string]Key, broken map[string]error) (*config.Config, error) {
	c := *base
	c.Providers = maps.Clone(base.Providers)
	if c.Providers == nil {
		c.Providers = map[string]config.Provider{}
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(o.Providers)) {
		d := o.Providers[name]
		if _, inFile := base.Providers[name]; inFile {
			errs = append(errs, fmt.Errorf("provider %s is defined in the config file (read-only in the dashboard)", name))
			continue
		}
		p := config.Provider{Type: d.Type, Region: d.Region, BaseURL: d.BaseURL, Timeout: time.Duration(d.TimeoutSeconds) * time.Second}
		if d.SecretARN != "" {
			if _, bad := broken[name]; bad {
				p.BaseURL = unavailable
			} else {
				k := keys[name]
				p.APIKey = k.APIKey
				if p.BaseURL == "" {
					p.BaseURL = k.BaseURL
				}
			}
		}
		c.Providers[name] = p
	}
	c.Models = maps.Clone(base.Models)
	if c.Models == nil {
		c.Models = map[string]config.Model{}
	}
	for alias, d := range o.Models {
		t := d.Type
		if t == "" {
			t = "chat"
		}
		c.Models[alias] = config.Model{Type: t, Provider: d.Provider, Model: d.Model, MaxTokens: d.MaxTokens,
			Dimensions: d.Dimensions, Fallback: slices.Clone(d.Fallback), PromptCache: d.PromptCache}
	}
	c.Pricing = slices.Clone(base.Pricing)
	for _, p := range o.Pricing {
		c.Pricing = append(c.Pricing, pricing.Entry{Provider: p.Provider, Model: p.Model, Input: p.Input, Output: p.Output,
			CacheRead: p.CacheRead, CacheWrite: p.CacheWrite, Free: p.Free, EffectiveFrom: p.EffectiveFrom})
	}
	if len(o.Groups.Admin)+len(o.Groups.Viewer) > 0 {
		if base.Dashboard.Entra == nil {
			errs = append(errs, errors.New("groups: the dashboard has no Entra sign-in (dashboard.entra)"))
		} else {
			e := *base.Dashboard.Entra
			e.AdminGroups = union(e.AdminGroups, o.Groups.Admin)
			e.ViewerGroups = union(e.ViewerGroups, o.Groups.Viewer)
			c.Dashboard.Entra = &e
		}
		for _, g := range append(slices.Clone(o.Groups.Admin), o.Groups.Viewer...) {
			if g.ID == "" {
				errs = append(errs, errors.New("groups: empty group object ID"))
			}
		}
	}
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &c, nil
}

func union(ids []string, groups []Group) []string {
	out := slices.Clone(ids)
	for _, g := range groups {
		if g.ID != "" && !slices.Contains(out, g.ID) {
			out = append(out, g.ID)
		}
	}
	return out
}

// State is one consistent snapshot: config + providers built from it.
type State struct {
	Cfg       *config.Config
	Providers map[string]provider.Provider
	Overrides Overrides
	// Broken: dashboard providers whose secret could not be read (name → error).
	Broken   map[string]string
	LoadedAt time.Time
}

// Static wraps a config without a database (no dashboard overrides).
func Static(cfg *config.Config, providers map[string]provider.Provider) *State {
	return &State{Cfg: cfg, Providers: providers, Overrides: Overrides{}.clone(), Broken: map[string]string{}, LoadedAt: time.Now()}
}

// SecretReader reads a Secrets Manager secret (as text).
type SecretReader interface {
	Read(ctx context.Context, arn string) (string, error)
}

// Builder creates a provider client from its definition.
type Builder func(ctx context.Context, name string, p config.Provider) (provider.Provider, error)

// Manager keeps the State in effect and reloads it when a revision is saved.
type Manager struct {
	Base          *config.Config
	BaseProviders map[string]provider.Provider
	Store         store.Store
	Secrets       SecretReader
	Build         Builder
	Log           *slog.Logger
	// Interval between checks for new revisions (other replicas). Default 5s.
	Interval time.Duration
	// KeyTTL: provider keys are re-read from Secrets Manager after this
	// (follows a secret rotation). Default 10 min.
	KeyTTL time.Duration

	cur     atomic.Pointer[State]
	mu      sync.Mutex
	lastID  int64
	lastErr string
	keys    map[string]cachedKey     // arn → value
	clients map[string]builtProvider // name → client (reused while the definition is the same)
}

type cachedKey struct {
	value string
	at    time.Time
}

type builtProvider struct {
	def config.Provider
	p   provider.Provider
}

// Current returns the snapshot in effect (never nil after Start).
func (m *Manager) Current() *State { return m.cur.Load() }

// Status is shown in the Config tab.
type Status struct {
	Revision  int64     `json:"revision"`
	LoadedAt  time.Time `json:"loaded_at"`
	LoadError string    `json:"load_error,omitempty"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Revision: m.lastID, LoadError: m.lastErr}
	if s := m.cur.Load(); s != nil {
		st.LoadedAt = s.LoadedAt
	}
	return st
}

// Start loads the overrides once. A broken override never stops nautilm:
// it runs with the config file only and the Config tab shows the error.
func (m *Manager) Start(ctx context.Context) {
	if m.Interval == 0 {
		m.Interval = 5 * time.Second
	}
	if m.KeyTTL == 0 {
		m.KeyTTL = 10 * time.Minute
	}
	m.cur.Store(Static(m.Base, m.BaseProviders))
	if err := m.Reload(ctx, true); err != nil {
		m.Log.Error("config overrides from the dashboard NOT applied; running with the config file only", "error", err)
	}
}

// Run polls for new revisions and refreshes provider keys until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(m.Interval)
	defer tick.Stop()
	lastKeys := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		force := time.Since(lastKeys) >= m.KeyTTL
		if force {
			lastKeys = time.Now()
		}
		if err := m.Reload(ctx, force); err != nil {
			m.Log.Warn("config reload", "error", err)
		}
	}
}

// Reload applies the newest revisions (force = also re-read provider keys).
func (m *Manager) Reload(ctx context.Context, force bool) error {
	id, err := m.Store.LastRevisionID(ctx)
	if err != nil {
		return fmt.Errorf("reading revisions: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !force && id == m.lastID {
		return nil
	}
	if force {
		m.keys = nil
	}
	latest, err := m.Store.LatestRevisions(ctx)
	if err != nil {
		return fmt.Errorf("reading revisions: %w", err)
	}
	prev := m.lastID
	m.lastID = id
	ov, err := Decode(latest)
	if err == nil {
		var st *State
		if st, err = m.build(ctx, ov); err == nil {
			m.cur.Store(st)
			m.lastErr = ""
			if id != prev {
				m.Log.Info("config overrides applied", "revision", id, "providers", len(ov.Providers),
					"aliases", len(ov.Models), "prices", len(ov.Pricing), "broken_providers", st.Broken)
			}
			return nil
		}
	}
	m.lastErr = err.Error()
	return err
}

// Candidate builds the state that a change WOULD produce, without saving
// anything: used to validate and test before the save.
func (m *Manager) Candidate(ctx context.Context, kind string, payload json.RawMessage) (*State, error) {
	ov, err := m.Current().Overrides.With(kind, payload)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.build(ctx, ov)
}

// build must be called with m.mu held.
func (m *Manager) build(ctx context.Context, ov Overrides) (*State, error) {
	keys, broken := map[string]Key{}, map[string]error{}
	for name, d := range ov.Providers {
		if d.SecretARN == "" {
			continue
		}
		k, err := m.key(ctx, d.SecretARN)
		if err != nil {
			broken[name] = err
			continue
		}
		keys[name] = k
	}
	cfg, err := Merge(m.Base, ov, keys, broken)
	if err != nil {
		return nil, err
	}
	st := &State{Cfg: cfg, Providers: maps.Clone(m.BaseProviders), Overrides: ov, Broken: map[string]string{}, LoadedAt: time.Now()}
	if m.clients == nil {
		m.clients = map[string]builtProvider{}
	}
	for name := range ov.Providers {
		if err, bad := broken[name]; bad {
			st.Broken[name] = err.Error()
			st.Providers[name] = Unavailable(name, err)
			continue
		}
		def := cfg.Providers[name]
		if b, ok := m.clients[name]; ok && b.def == def {
			st.Providers[name] = b.p
			continue
		}
		p, err := m.Build(ctx, name, def)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		m.clients[name] = builtProvider{def: def, p: p}
		st.Providers[name] = p
	}
	return st, nil
}

// key reads (and caches) a provider secret. Must be called with m.mu held.
func (m *Manager) key(ctx context.Context, arn string) (Key, error) {
	if m.keys == nil {
		m.keys = map[string]cachedKey{}
	}
	c, ok := m.keys[arn]
	if !ok || time.Since(c.at) > m.KeyTTL {
		if m.Secrets == nil {
			return Key{}, errors.New("no secret reader configured")
		}
		v, err := m.Secrets.Read(ctx, arn)
		if err != nil {
			return Key{}, err
		}
		c = cachedKey{value: v, at: time.Now()}
		m.keys[arn] = c
	}
	return ParseKey(c.value)
}

// Unavailable is a provider whose secret could not be read.
func Unavailable(name string, cause error) provider.Provider { return unavailableProvider{name, cause} }

type unavailableProvider struct {
	name  string
	cause error
}

func (u unavailableProvider) err() error {
	return &provider.Error{Status: 503, Code: "provider_unavailable",
		Message: fmt.Sprintf("provider %s is unavailable: its secret could not be read (%v); check the Config tab", u.name, u.cause)}
}

func (u unavailableProvider) Chat(context.Context, provider.Call) (*openai.ChatResponse, error) {
	return nil, u.err()
}

func (u unavailableProvider) Embed(context.Context, provider.EmbedCall) ([][]float32, *openai.Usage, error) {
	return nil, nil, u.err()
}
