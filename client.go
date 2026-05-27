package sori

import (
	"context"
	"net/http"
	"os"
	"time"
)

// Client is the preferred entrypoint for the core packaging, push, and fetch
// path.
//
// Client is the intended long-lived core surface for new code.
type Client struct {
	localStorePath string
	httpClient     *http.Client
	now            func() time.Time
}

// ClientOption configures the preferred Client-based core path.
type ClientOption func(*Client)

// NewClient constructs the preferred core client path for packaging, pushing,
// and fetching datasets.
//
// This constructor is part of the intended long-lived core surface.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		localStorePath: defaultOCIStore,
		httpClient:     nil,
		now:            time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithLocalStorePath configures the local OCI store path used by the preferred
// core client path.
func WithLocalStorePath(path string) ClientOption {
	return func(c *Client) {
		if path != "" {
			c.localStorePath = path
		}
	}
}

// WithHTTPClient injects the HTTP client used by the preferred core push path.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// WithClock injects the clock used by client flows that need timestamps.
func WithClock(now func() time.Time) ClientOption {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// LocalStorePath returns the local OCI store path used by the client.
func (c *Client) LocalStorePath() string {
	return c.localStorePath
}

// PackageVolume packages a dataset using the preferred client-based core path.
func (c *Client) PackageVolume(ctx context.Context, req PackageRequest) (*PackageResult, error) {
	return c.PackageVolumeWithOptions(ctx, req, PackageOptions{})
}

// PackageVolumeWithOptions packages a dataset using the preferred client-based
// core path with explicit core packaging options.
func (c *Client) PackageVolumeWithOptions(ctx context.Context, req PackageRequest, opts PackageOptions) (*PackageResult, error) {
	req.ConfigBlob = opts.ConfigBlob
	return packageVolumeToStoreWithOptions(ctx, c.localStorePath, req, opts, c.now)
}

// PushPackagedVolume pushes a packaged dataset using the preferred client-based
// core path.
func (c *Client) PushPackagedVolume(ctx context.Context, pkg *PackageResult, target RemoteTarget) (*PushResult, error) {
	return c.PushPackagedVolumeWithOptions(ctx, pkg, PushOptions{Target: target})
}

// PushPackagedVolumeWithOptions pushes a packaged dataset using the preferred
// client-based core path with explicit core push options.
func (c *Client) PushPackagedVolumeWithOptions(ctx context.Context, pkg *PackageResult, opts PushOptions) (*PushResult, error) {
	target := opts.Target
	if c.httpClient != nil {
		target.HTTPClient = c.httpClient
	}
	return PushPackagedVolume(ctx, c.localStorePath, pkg, target)
}

// FetchVolumeSequential fetches a packaged dataset using the preferred client
// path with sequential extraction.
func (c *Client) FetchVolumeSequential(ctx context.Context, destRoot, repo, tag string) (*VolumeIndex, error) {
	return c.FetchVolume(ctx, destRoot, repo, tag, FetchOptions{Concurrency: 1})
}

// FetchVolumeParallel fetches a packaged dataset using the preferred client
// path with explicit parallelism.
func (c *Client) FetchVolumeParallel(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	return c.FetchVolume(ctx, destRoot, repo, tag, FetchOptions{Concurrency: concurrency})
}

// FetchVolume fetches a packaged dataset using the preferred client-based core
// path and core fetch options.
//
// When RequireEmptyDestination is true, extraction uses a staging directory so
// that destRoot is either left untouched (on failure) or fully populated (on
// success), preventing partial-extraction states.
func (c *Client) FetchVolume(ctx context.Context, destRoot, repo, tag string, opts FetchOptions) (*VolumeIndex, error) {
	if opts.RequireEmptyDestination {
		if err := ensureEmptyDir(destRoot); err != nil {
			return nil, err
		}
		return fetchVolWithStaging(ctx, destRoot, repo, tag, opts.Concurrency)
	}
	if opts.Concurrency <= 1 {
		return FetchVolSeq(ctx, destRoot, repo, tag)
	}
	return FetchVolParallel(ctx, destRoot, repo, tag, opts.Concurrency)
}

func ensureEmptyDir(path string) error {
	if _, err := os.Stat(path); err == nil {
		return conflictError("FetchVolume", "destination already exists; it must not exist when RequireEmptyDestination is true", nil)
	} else if os.IsNotExist(err) {
		return nil
	} else {
		return transportError("FetchVolume", "stat destination directory", err)
	}
}

// PublishVolume publishes an already-built VolumeIndex through the client path.
//
// This method exists for callers that still operate at the VolumeIndex level,
// but the preferred core path for new code is PackageVolumeWithOptions.
func (c *Client) PublishVolume(ctx context.Context, vi *VolumeIndex, volPath, volName string, configBlob []byte) (*VolumeIndex, error) {
	return vi.publishVolumeToStore(ctx, c.localStorePath, volPath, volName, configBlob, c.now)
}

// PublishVolumeFromDir is a convenience wrapper over the preferred client
// packaging path for callers that start from a directory.
func (c *Client) PublishVolumeFromDir(ctx context.Context, volDir, displayName, tag string) (*PackageResult, error) {
	return c.PackageVolume(ctx, PackageRequest{
		SourceDir:   volDir,
		DisplayName: displayName,
		Tag:         tag,
	})
}
