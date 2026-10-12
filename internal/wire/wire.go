// Package wire is a minimal git smart-HTTP client: protocol v2 for reads
// (ls-refs, fetch) and the receive-pack protocol for pushes.
package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/facets-cloud/go-gitq/internal/object"
)

var (
	ErrNotFound    = errors.New("wire: repository not found")
	ErrAuth        = errors.New("wire: authentication failed")
	ErrUnsupported = errors.New("wire: server does not support protocol v2 with filter")
	ErrStale       = errors.New("wire: ref moved since expected old value")
	ErrRateLimited = errors.New("wire: rate limited")
	ErrTooLarge    = errors.New("wire: response exceeds size limit")
)

// RejectedError is a push rejected by the server for a reason other than a stale
// ref (branch protection, hooks, ...).
type RejectedError struct{ Ref, Reason string }

func (e *RejectedError) Error() string {
	return fmt.Sprintf("wire: push of %s rejected: %s", e.Ref, e.Reason)
}

// Client talks to one repository. URL is the https repository URL ending in
// ".git" (the caller normalises it). Auth, if set, is called on every request
// to add credentials; an error from it aborts that request.
type Client struct {
	URL  string
	HTTP *http.Client
	Auth func(*http.Request) error
}

func NewClient(url string, httpc *http.Client, auth func(*http.Request) error) *Client {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &Client{URL: url, HTTP: httpc, Auth: auth}
}

// Caps is the v2 capability advertisement.
type Caps struct {
	Version2 bool
	// Fetch lists the features of the fetch command, e.g. "shallow", "filter".
	Fetch []string
	// Raw holds every advertised capability line.
	Raw []string
}

func (c Caps) HasFetchFeature(name string) bool {
	for _, f := range c.Fetch {
		if f == name {
			return true
		}
	}
	return false
}

// Capabilities does GET info/refs?service=git-upload-pack with Git-Protocol: version=2.
func (c *Client) Capabilities(ctx context.Context) (Caps, error) { return c.capabilities(ctx) }

type Ref struct {
	Name   string // e.g. "refs/heads/main" or "HEAD"
	Hash   object.Hash
	Peeled object.Hash // for annotated tags, the commit it points at; zero otherwise
	Symref string      // for HEAD, the target ref name if advertised
}

// LsRefs runs the v2 ls-refs command with symrefs and peel, filtered by ref-prefix
// lines (none = all refs).
func (c *Client) LsRefs(ctx context.Context, prefixes []string) ([]Ref, error) {
	return c.lsRefs(ctx, prefixes)
}

type FetchRequest struct {
	Wants  []object.Hash
	Haves  []object.Hash
	Depth  int    // >0 sends "deepen N"
	Filter string // e.g. "blob:none", "tree:0"; "" = no filter
}

// FetchStream runs the v2 fetch command (sending "done", no negotiation rounds)
// and returns the packfile as a stream, de-sidebanded. shallow-info and
// acknowledgments sections are skipped. Sideband channel 3 becomes a read error.
// Reading more than maxBytes of pack data (0 = no limit) fails with ErrTooLarge.
// The caller must Close the stream.
func (c *Client) FetchStream(ctx context.Context, req FetchRequest, maxBytes int64) (io.ReadCloser, error) {
	return c.fetchStream(ctx, req, maxBytes)
}

// Fetch is FetchStream read to the end, with no size limit.
func (c *Client) Fetch(ctx context.Context, req FetchRequest) ([]byte, error) {
	rc, err := c.fetchStream(ctx, req, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type RefUpdate struct {
	Name     string      // full ref name, e.g. "refs/heads/main"
	Old, New object.Hash // Old zero = create, New zero = delete
}

// Push updates refs via receive-pack (protocol v0/v1 over smart HTTP):
// GET info/refs?service=git-receive-pack for capabilities, then POST the
// commands + pack with report-status. Returns ErrStale when the server rejects
// because the ref is not at Old (non-fast-forward / "fetch first" / stale info),
// *RejectedError for other rejections. pack may be nil for deletes.
func (c *Client) Push(ctx context.Context, updates []RefUpdate, pack []byte) error {
	return c.push(ctx, updates, pack)
}
