package gitq

import (
	"bytes"
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

	mu       sync.RWMutex
	objs     map[object.Hash]object.Object // commits and trees (raw)
	trees    map[object.Hash][]object.TreeEntry
	commits  map[object.Hash]*object.Commit
	complete map[object.Hash]bool // trees whose whole subtree closure is cached
	blobs    *blobLRU

	refMu sync.Mutex
	refs  map[string]refEntry

	sf flight
}

type refEntry struct {
	hash object.Hash
	exp  time.Time
}

func open(ctx context.Context, url string, cfg config, blobs *blobLRU) (*repo, error) {
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
		cfg:      cfg,
		wc:       wire.NewClient(u, hc, cfg.auth),
		objs:     make(map[object.Hash]object.Object),
		trees:    make(map[object.Hash][]object.TreeEntry),
		commits:  make(map[object.Hash]*object.Commit),
		complete: make(map[object.Hash]bool),
		blobs:    blobs,
		refs:     make(map[string]refEntry),
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
	}
	return err
}

func notFound(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
}

// ---- object cache ----

func isDir(mode uint32) bool { return mode&0o170000 == object.ModeDir }

func (r *repo) lookup(h object.Hash) (object.Object, bool) {
	r.mu.RLock()
	o, ok := r.objs[h]
	r.mu.RUnlock()
	if ok {
		return o, true
	}
	if d, ok := r.blobs.get(h); ok {
		return object.Object{Type: object.TypeBlob, Data: d}, true
	}
	return object.Object{}, false
}

// addObjects stores decoded objects: commits and trees permanently, blobs in the LRU.
func (r *repo) addObjects(objs map[object.Hash]object.Object) {
	r.mu.Lock()
	for h, o := range objs {
		if o.Type == object.TypeCommit || o.Type == object.TypeTree {
			r.objs[h] = o
		}
	}
	r.mu.Unlock()
	for h, o := range objs {
		if o.Type == object.TypeBlob {
			r.blobs.put(h, o.Data)
		}
	}
}

// cachedTree returns parsed tree entries if the tree is cached. The slice is shared; do not modify.
func (r *repo) cachedTree(h object.Hash) ([]object.TreeEntry, bool, error) {
	r.mu.RLock()
	es, ok := r.trees[h]
	o, raw := r.objs[h]
	r.mu.RUnlock()
	if ok {
		return es, true, nil
	}
	if !raw || o.Type != object.TypeTree {
		return nil, false, nil
	}
	es, err := object.ParseTree(o.Data)
	if err != nil {
		return nil, false, fmt.Errorf("gitq: tree %s: %w", h, err)
	}
	r.mu.Lock()
	r.trees[h] = es
	r.mu.Unlock()
	return es, true, nil
}

// tree returns a tree, fetching it (with its subtrees) if not cached.
func (r *repo) tree(ctx context.Context, h object.Hash) ([]object.TreeEntry, error) {
	es, ok, err := r.cachedTree(h)
	if err != nil || ok {
		return es, err
	}
	if err := r.fetchTrees(ctx, []object.Hash{h}); err != nil {
		return nil, err
	}
	es, ok, err = r.cachedTree(h)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("gitq: server did not send tree %s", h)
	}
	return es, nil
}

func (r *repo) cachedCommit(h object.Hash) (*object.Commit, bool, error) {
	r.mu.RLock()
	c, ok := r.commits[h]
	o, raw := r.objs[h]
	r.mu.RUnlock()
	if ok {
		return c, true, nil
	}
	if !raw || o.Type != object.TypeCommit {
		return nil, false, nil
	}
	c, err := object.ParseCommit(o.Data)
	if err != nil {
		return nil, false, fmt.Errorf("gitq: commit %s: %w", h, err)
	}
	r.mu.Lock()
	r.commits[h] = c
	r.mu.Unlock()
	return c, true, nil
}

// fetch runs one fetch and caches every object in the returned pack.
func (r *repo) fetch(ctx context.Context, req wire.FetchRequest) (map[object.Hash]object.Object, error) {
	data, err := r.wc.Fetch(ctx, req)
	if err != nil {
		return nil, mapErr(err)
	}
	objs, err := pack.Decode(bytes.NewReader(data), r.lookup)
	if err != nil {
		return nil, fmt.Errorf("gitq: decoding pack: %w", err)
	}
	r.addObjects(objs)
	return objs, nil
}

func hashesKey(prefix string, hs []object.Hash) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, h := range hs {
		b.WriteByte(':')
		b.WriteString(h.String())
	}
	return b.String()
}

func (r *repo) fetchTrees(ctx context.Context, hs []object.Hash) error {
	_, err := r.sf.do(ctx, hashesKey("trees", hs), func() (any, error) {
		return r.fetch(ctx, wire.FetchRequest{Wants: hs, Filter: "blob:none"})
	})
	return err
}

// commit returns a commit, fetching it (depth 1, with its trees) if not cached.
func (r *repo) commit(ctx context.Context, h object.Hash) (*object.Commit, error) {
	c, ok, err := r.cachedCommit(h)
	if err != nil || ok {
		return c, err
	}
	if err := r.ensureCommit(ctx, h); err != nil {
		return nil, err
	}
	c, ok, err = r.cachedCommit(h)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("commit %s", h)
	}
	return c, nil
}

// missingTrees walks the tree closure of h using only the cache, marks complete
// subtrees, and appends any trees not cached to missing.
func (r *repo) missingTrees(h object.Hash, missing *[]object.Hash, seen map[object.Hash]bool) (bool, error) {
	r.mu.RLock()
	done := r.complete[h]
	r.mu.RUnlock()
	if done {
		return true, nil
	}
	if seen[h] {
		return false, nil // already reported in this walk
	}
	seen[h] = true
	es, ok, err := r.cachedTree(h)
	if err != nil {
		return false, err
	}
	if !ok {
		*missing = append(*missing, h)
		return false, nil
	}
	all := true
	for _, e := range es {
		if !isDir(e.Mode) {
			continue
		}
		ok, err := r.missingTrees(e.Hash, missing, seen)
		if err != nil {
			return false, err
		}
		all = all && ok
	}
	if all {
		r.mu.Lock()
		r.complete[h] = true
		r.mu.Unlock()
	}
	return all, nil
}

// ensureCommit makes sure the commit and its full tree closure are cached.
func (r *repo) ensureCommit(ctx context.Context, h object.Hash) error {
	r.mu.RLock()
	if c, ok := r.commits[h]; ok && r.complete[c.Tree] {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()
	_, err := r.sf.do(ctx, "commit:"+h.String(), func() (any, error) {
		return nil, r.ensureCommitSlow(ctx, h)
	})
	return err
}

func (r *repo) ensureCommitSlow(ctx context.Context, h object.Hash) error {
	c, ok, err := r.cachedCommit(h)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := r.fetch(ctx, wire.FetchRequest{Wants: []object.Hash{h}, Depth: 1, Filter: "blob:none"}); err != nil {
			return err
		}
		if c, ok, err = r.cachedCommit(h); err != nil {
			return err
		} else if !ok {
			return notFound("commit %s", h)
		}
	}
	for attempt := 0; ; attempt++ {
		var missing []object.Hash
		done, err := r.missingTrees(c.Tree, &missing, map[object.Hash]bool{})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if attempt >= 3 {
			return fmt.Errorf("gitq: server did not send %d tree(s) of commit %s", len(missing), h)
		}
		if err := r.fetchTrees(ctx, missing); err != nil {
			return err
		}
	}
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
		r.refMu.Lock()
		r.refs[ref] = refEntry{hash: h, exp: time.Now().Add(r.cfg.refTTL)}
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
