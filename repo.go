package gitq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/facets-cloud/go-gitq/internal/object"
	"github.com/facets-cloud/go-gitq/internal/pack"
	"github.com/facets-cloud/go-gitq/internal/wire"
)

// repo is the implementation behind Repo.
type repo struct {
	cfg config
	wc  *wire.Client

	objs *objLRU // shared, bounded; the only object store

	refMu sync.Mutex
	refs  map[string]refEntry

	sf flight
}

// maxRefEntries bounds the per-repo ref cache used with RefTTL.
const maxRefEntries = 1024

type refEntry struct {
	hash object.Hash
	exp  time.Time
}

func open(ctx context.Context, url string, cfg config, objs *objLRU) (*repo, error) {
	u, err := normalizeURL(url)
	if err != nil {
		return nil, err
	}
	if cfg.historyDepth <= 0 {
		cfg.historyDepth = 100
	}
	hc := cfg.httpClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	r := &repo{
		cfg:  cfg,
		wc:   wire.NewClient(u, hc, cfg.auth),
		objs: objs,
		refs: make(map[string]refEntry),
	}
	caps, err := r.wc.Capabilities(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	if !caps.Version2 || !caps.HasFetchFeature("filter") {
		return nil, ErrUnsupported
	}
	return r, nil
}

func defaultHTTPClient() *http.Client {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: t}
}

// normalizeURL turns the accepted URL spellings into an http(s) URL ending in .git.
func normalizeURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	var scheme, host, p string
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		scheme, rest, _ := strings.Cut(u, "://")
		rest = strings.TrimRight(rest, "/")
		if !strings.HasSuffix(rest, ".git") {
			rest += ".git"
		}
		return scheme + "://" + rest, nil
	case strings.HasPrefix(u, "ssh://"), strings.HasPrefix(u, "git+ssh://"), strings.HasPrefix(u, "git://"):
		_, rest, _ := strings.Cut(u, "://")
		host, p, _ = strings.Cut(rest, "/")
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		scheme = "https"
	case !strings.Contains(u, "://") && strings.Contains(u, ":"):
		host, p, _ = strings.Cut(u, ":")
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
		scheme = "https"
	default:
		return "", fmt.Errorf("gitq: unsupported repository URL %q", u)
	}
	p = strings.Trim(p, "/")
	if host == "" || p == "" {
		return "", fmt.Errorf("gitq: unsupported repository URL %q", u)
	}
	if !strings.HasSuffix(p, ".git") {
		p += ".git"
	}
	return scheme + "://" + host + "/" + p, nil
}

// mapErr wraps lower-layer errors with the public sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, wire.ErrAuth):
		return fmt.Errorf("%w: %w", ErrAuth, err)
	case errors.Is(err, wire.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, wire.ErrUnsupported):
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	case errors.Is(err, wire.ErrStale):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, wire.ErrTooLarge), errors.Is(err, pack.ErrTooLarge):
		return fmt.Errorf("%w: %w", ErrTooLarge, err)
	}
	return err
}

func notFound(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
}

// ---- object cache ----
//
// Every object lives in the shared objLRU and may be evicted at any time, so
// each accessor falls back to fetching, and uses the objects a fetch returned
// directly rather than re-reading the cache (which may already have dropped
// them when the budget is smaller than the fetch).

func isDir(mode uint32) bool { return mode&0o170000 == object.ModeDir }

func (r *repo) lookup(h object.Hash) (object.Object, bool) { return r.objs.get(h) }

func (r *repo) addObjects(objs map[object.Hash]object.Object) {
	for h, o := range objs {
		r.objs.put(h, o)
	}
}

func parseTree(h object.Hash, o object.Object) ([]object.TreeEntry, error) {
	if o.Type != object.TypeTree {
		return nil, fmt.Errorf("gitq: %s is not a tree", h)
	}
	es, err := object.ParseTree(o.Data)
	if err != nil {
		return nil, fmt.Errorf("gitq: tree %s: %w", h, err)
	}
	return es, nil
}

func parseCommit(h object.Hash, o object.Object) (*object.Commit, error) {
	if o.Type != object.TypeCommit {
		return nil, fmt.Errorf("gitq: %s is not a commit", h)
	}
	c, err := object.ParseCommit(o.Data)
	if err != nil {
		return nil, fmt.Errorf("gitq: commit %s: %w", h, err)
	}
	return c, nil
}

// tree returns a tree's entries, fetching it (with all its subtrees) if not cached.
func (r *repo) tree(ctx context.Context, h object.Hash) ([]object.TreeEntry, error) {
	if o, ok := r.objs.get(h); ok {
		return parseTree(h, o)
	}
	v, err := r.sf.do(ctx, "tree:"+h.String(), func() (any, error) {
		return r.fetch(ctx, wire.FetchRequest{Wants: []object.Hash{h}, Filter: "blob:none"})
	})
	if err != nil {
		return nil, err
	}
	o, ok := v.(map[object.Hash]object.Object)[h]
	if !ok {
		return nil, fmt.Errorf("gitq: server did not send tree %s", h)
	}
	return parseTree(h, o)
}

// commit returns a commit, fetching it (depth 1, with all its trees) if not cached.
func (r *repo) commit(ctx context.Context, h object.Hash) (*object.Commit, error) {
	if o, ok := r.objs.get(h); ok {
		return parseCommit(h, o)
	}
	return r.fetchCommit(ctx, h, 1)
}

// fetchCommit fetches depth commits of history from h, with their trees, and
// returns h itself.
func (r *repo) fetchCommit(ctx context.Context, h object.Hash, depth int) (*object.Commit, error) {
	v, err := r.sf.do(ctx, fmt.Sprintf("commit:%d:%s", depth, h), func() (any, error) {
		return r.fetch(ctx, wire.FetchRequest{Wants: []object.Hash{h}, Depth: depth, Filter: "blob:none"})
	})
	if err != nil {
		return nil, err
	}
	o, ok := v.(map[object.Hash]object.Object)[h]
	if !ok {
		return nil, notFound("commit %s", h)
	}
	return parseCommit(h, o)
}

// fetch runs one fetch, decoding the pack as it streams in, and caches every
// object in it.
func (r *repo) fetch(ctx context.Context, req wire.FetchRequest) (map[object.Hash]object.Object, error) {
	rc, err := r.wc.FetchStream(ctx, req, r.cfg.maxFetch)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rc.Close()
	objs, err := pack.DecodeLimited(rc, r.lookup, pack.Limits{MaxObject: r.cfg.maxFile, MaxTotal: r.cfg.maxFetch})
	if err != nil {
		return nil, mapErr(fmt.Errorf("gitq: fetching pack: %w", err))
	}
	r.addObjects(objs)
	return objs, nil
}

// ---- ref resolution ----

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (r *repo) resolve(ctx context.Context, ref string) (object.Hash, error) {
	if isHexSHA(ref) {
		return object.ParseHash(strings.ToLower(ref))
	}
	if ref == "" {
		return object.ZeroHash, notFound("empty ref")
	}
	if r.cfg.refTTL > 0 {
		r.refMu.Lock()
		e, ok := r.refs[ref]
		r.refMu.Unlock()
		if ok && time.Now().Before(e.exp) {
			return e.hash, nil
		}
	}
	v, err := r.sf.do(ctx, "ref:"+ref, func() (any, error) {
		return r.lsRef(ctx, ref)
	})
	if err != nil {
		return object.ZeroHash, err
	}
	h := v.(object.Hash)
	if r.cfg.refTTL > 0 {
		now := time.Now()
		r.refMu.Lock()
		if len(r.refs) >= maxRefEntries {
			for k, e := range r.refs {
				if now.After(e.exp) {
					delete(r.refs, k)
				}
			}
			if len(r.refs) >= maxRefEntries {
				clear(r.refs)
			}
		}
		r.refs[ref] = refEntry{hash: h, exp: now.Add(r.cfg.refTTL)}
		r.refMu.Unlock()
	}
	return h, nil
}

func (r *repo) lsRef(ctx context.Context, ref string) (object.Hash, error) {
	candidates := []string{"refs/heads/" + ref, "refs/tags/" + ref, ref}
	refs, err := r.wc.LsRefs(ctx, candidates)
	if err != nil {
		return object.ZeroHash, mapErr(err)
	}
	for _, want := range candidates {
		for _, rf := range refs {
			if rf.Name != want {
				continue
			}
			if !rf.Peeled.IsZero() {
				return rf.Peeled, nil
			}
			return rf.Hash, nil
		}
	}
	return object.ZeroHash, notFound("ref %q", ref)
}

func (r *repo) forgetRef(ref string) {
	r.refMu.Lock()
	delete(r.refs, ref)
	r.refMu.Unlock()
}

func (r *repo) getSHA(ctx context.Context, ref string) (string, error) {
	h, err := r.resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (r *repo) listBranches(ctx context.Context) ([]string, error) {
	refs, err := r.wc.LsRefs(ctx, []string{"refs/heads/"})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]string, 0, len(refs))
	for _, rf := range refs {
		if name, ok := strings.CutPrefix(rf.Name, "refs/heads/"); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- singleflight ----

type flight struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

// do runs fn once for concurrent callers with the same key. A waiter whose own
// context is still live retries if the leader failed only because its context ended.
func (g *flight) do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	for {
		g.mu.Lock()
		if g.m == nil {
			g.m = make(map[string]*flightCall)
		}
		if c, ok := g.m[key]; ok {
			g.mu.Unlock()
			select {
			case <-c.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if c.err != nil && (errors.Is(c.err, context.Canceled) || errors.Is(c.err, context.DeadlineExceeded)) && ctx.Err() == nil {
				continue
			}
			return c.val, c.err
		}
		c := &flightCall{done: make(chan struct{})}
		g.m[key] = c
		g.mu.Unlock()
		c.val, c.err = fn()
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		close(c.done)
		return c.val, c.err
	}
}
