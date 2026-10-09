// Package gitq reads and writes remote git repositories without a clone or
// checkout. It talks the git smart-HTTP protocol (v2) directly: file contents,
// listings and history are fetched on demand and cached in memory.
//
//	cache := gitq.NewCache(256<<20, true) // one per process
//	repo, _ := gitq.Open(ctx, "https://github.com/hashicorp/terraform", cache)
//	data, _ := repo.GetContent(ctx, "main", "README.md")
//
// A ref is anywhere a branch name, tag name or full commit SHA.
package gitq

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrNotFound: the ref, file or directory does not exist.
	ErrNotFound = errors.New("gitq: not found")
	// ErrConflict: Save's branch is no longer at the expected SHA.
	ErrConflict = errors.New("gitq: branch moved")
	// ErrUnsupported: the server lacks protocol v2 or partial-clone filters.
	ErrUnsupported = errors.New("gitq: server does not support protocol v2 with filters")
	// ErrAuth: credentials missing or rejected.
	ErrAuth = errors.New("gitq: authentication failed")
)

// File is one file in a listing.
type File struct {
	Path string // relative to the repo root
	SHA  string // blob id; changes only when the content changes
	Mode uint32 // git mode: 0o100644, 0o100755, 0o120000 (symlink)
}

// Commit is one entry of a file's history.
type Commit struct {
	SHA     string
	Author  string
	Email   string
	Date    time.Time
	Message string
}

// Change is one file changed between two refs.
type Change struct {
	Status string // "A" added, "M" modified, "D" deleted
	Path   string
}

// Author identifies who a Save is attributed to.
type Author struct {
	Name  string
	Email string
}

// Option configures Open.
type Option func(*config)

type config struct {
	httpClient   *http.Client
	auth         func(*http.Request)
	refTTL       time.Duration
	historyDepth int
}

// Token authenticates with an access token over HTTP basic auth, using the
// username each host expects alongside a token: "x-token-auth" on bitbucket.org
// (repository/workspace access tokens), "x-access-token" on github.com and
// GitHub Enterprise Cloud (*.ghe.com), and "oauth2" elsewhere, which GitLab
// (gitlab.com and self-hosted), Gitea and GitHub Enterprise Server accept.
// Use BasicAuth to choose the username yourself (e.g. Bitbucket app passwords).
func Token(token string) Option {
	return func(c *config) {
		c.auth = func(r *http.Request) { r.SetBasicAuth(tokenUser(r.URL.Hostname()), token) }
	}
}

func tokenUser(host string) string {
	host = strings.ToLower(host)
	switch {
	case host == "bitbucket.org":
		return "x-token-auth"
	case host == "github.com" || strings.HasSuffix(host, ".ghe.com"):
		return "x-access-token"
	default:
		return "oauth2"
	}
}

// BasicAuth authenticates with a username and password or app password.
func BasicAuth(user, password string) Option {
	return func(c *config) {
		c.auth = func(r *http.Request) { r.SetBasicAuth(user, password) }
	}
}

// HTTPClient sets the http.Client used for every request.
func HTTPClient(h *http.Client) Option { return func(c *config) { c.httpClient = h } }

// RefTTL sets how long a resolved branch or tag is reused before asking the
// server again. Default 0: always ask. Commit SHAs never expire.
func RefTTL(d time.Duration) Option { return func(c *config) { c.refTTL = d } }

// HistoryDepth sets how many commits GetHistory and GetChanges fetch per call
// when the history is not cached yet. Default 100.
func HistoryDepth(n int) Option { return func(c *config) { c.historyDepth = n } }

// Cache holds fetched file contents in memory, compressed, up to a fixed
// budget; least recently used files are evicted first. Contents are keyed by
// their git hash, so one Cache can and should be shared by every Repo in the
// process: the budget is then process-wide and identical files are stored once.
type Cache struct{ blobs *blobLRU }

// NewCache returns a cache that uses at most maxBytes of memory for file
// contents; maxBytes 0 caches nothing. With compress, contents are stored
// compressed (source code typically shrinks 3-5x, so the budget holds that many
// more files) and each cache hit pays a decompression (roughly 20us per 10KB).
// Without it, hits are near-free but the budget holds fewer files.
func NewCache(maxBytes int64, compress bool) *Cache {
	return &Cache{blobs: newBlobLRU(maxBytes, compress)}
}

// CacheStats describes what a Cache holds.
type CacheStats struct {
	Files       int   // cached file contents
	StoredBytes int64 // memory they use (compressed, if enabled)
	FileBytes   int64 // their uncompressed size
}

// Stats reports the current cache contents.
func (c *Cache) Stats() CacheStats {
	n, stored, raw := c.blobs.stats()
	return CacheStats{Files: n, StoredBytes: stored, FileBytes: raw}
}

// Repo is a handle to one remote repository. It is safe for concurrent use.
type Repo struct {
	r *repo
}

// Open connects to a repository. url may be https://host/owner/repo(.git) or
// an scp/ssh style git@host:owner/repo.git, which is rewritten to HTTPS.
// It checks the server supports protocol v2 with filters.
func Open(ctx context.Context, url string, cache *Cache, opts ...Option) (*Repo, error) {
	if cache == nil {
		return nil, errors.New("gitq: Open needs a cache; create one with gitq.NewCache and share it")
	}
	cfg := config{historyDepth: 100}
	for _, o := range opts {
		o(&cfg)
	}
	r, err := open(ctx, url, cfg, cache.blobs)
	if err != nil {
		return nil, err
	}
	return &Repo{r: r}, nil
}

// GetSHA resolves a branch, tag or SHA to a commit SHA.
func (r *Repo) GetSHA(ctx context.Context, ref string) (string, error) {
	return r.r.getSHA(ctx, ref)
}

// ListBranches returns branch names (without "refs/heads/").
func (r *Repo) ListBranches(ctx context.Context) ([]string, error) {
	return r.r.listBranches(ctx)
}

// GetContent returns one file's content at ref.
func (r *Repo) GetContent(ctx context.Context, ref, path string) ([]byte, error) {
	return r.r.getContent(ctx, ref, path)
}

// GetContents returns several files at ref, fetched in one round trip.
// Paths that do not exist are absent from the map.
func (r *Repo) GetContents(ctx context.Context, ref string, paths []string) (map[string][]byte, error) {
	return r.r.getContents(ctx, ref, paths)
}

// ListFiles returns every file under dir (recursively) at ref. dir "" or "/"
// means the whole repo.
func (r *Repo) ListFiles(ctx context.Context, ref, dir string) ([]File, error) {
	return r.r.listFiles(ctx, ref, dir)
}

// GetHistory returns up to limit commits reachable from ref that changed path
// (a file or directory), newest first, following first parents.
func (r *Repo) GetHistory(ctx context.Context, ref, path string, limit int) ([]Commit, error) {
	return r.r.getHistory(ctx, ref, path, limit)
}

// GetChanges returns files that differ between fromRef and toRef.
func (r *Repo) GetChanges(ctx context.Context, fromRef, toRef string) ([]Change, error) {
	return r.r.getChanges(ctx, fromRef, toRef)
}

// Download writes every file under dir at ref to w as a tar archive, with paths
// relative to dir.
func (r *Repo) Download(ctx context.Context, ref, dir string, w io.Writer) error {
	return r.r.download(ctx, ref, dir, w)
}

// Save commits files to branch and pushes, in one step. A nil value deletes
// that path. expectedSHA is the commit the branch must currently point at; ""
// creates the branch (or the first commit of an empty repo). If the branch
// moved, Save returns ErrConflict. It returns the new commit SHA.
func (r *Repo) Save(ctx context.Context, branch, expectedSHA, message string, author Author, files map[string][]byte) (string, error) {
	return r.r.save(ctx, branch, expectedSHA, message, author, files)
}
