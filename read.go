package gitq

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/facets-cloud/go-gitq/internal/object"
	"github.com/facets-cloud/go-gitq/internal/wire"
)

const (
	blobBatch    = 500 // wants per fetch
	blobParallel = 4   // fetches in flight
)

// cleanPath normalises a repo path: no leading/trailing slashes, "" for the root.
func cleanPath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

// rootOf resolves ref and returns the commit hash and the commit. Fetching a
// commit brings all its trees in the same request.
func (r *repo) rootOf(ctx context.Context, ref string) (object.Hash, *object.Commit, error) {
	h, err := r.resolve(ctx, ref)
	if err != nil {
		return h, nil, err
	}
	c, err := r.commit(ctx, h)
	return h, c, err
}

// entryAt returns the tree entry at p under root ("" yields a synthetic dir entry
// for root). found is false when the path does not exist.
func (r *repo) entryAt(ctx context.Context, root object.Hash, p string) (object.TreeEntry, bool, error) {
	cur := object.TreeEntry{Mode: object.ModeDir, Hash: root}
	if p == "" {
		return cur, true, nil
	}
	for _, part := range strings.Split(p, "/") {
		if !isDir(cur.Mode) {
			return object.TreeEntry{}, false, nil
		}
		es, err := r.tree(ctx, cur.Hash)
		if err != nil {
			return object.TreeEntry{}, false, err
		}
		i, ok := findEntry(es, part)
		if !ok {
			return object.TreeEntry{}, false, nil
		}
		cur = es[i]
	}
	return cur, true, nil
}

func findEntry(es []object.TreeEntry, name string) (int, bool) {
	for i := range es {
		if es[i].Name == name {
			return i, true
		}
	}
	return -1, false
}

// fetchBlobs returns the content of every hash, from the cache or the server.
func (r *repo) fetchBlobs(ctx context.Context, hashes []object.Hash) (map[object.Hash][]byte, error) {
	out := make(map[object.Hash][]byte, len(hashes))
	var need []object.Hash
	for _, h := range hashes {
		if _, ok := out[h]; ok {
			continue
		}
		if o, ok := r.objs.get(h); ok && o.Type == object.TypeBlob {
			out[h] = o.Data
			continue
		}
		out[h] = nil
		need = append(need, h)
	}
	if len(need) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, blobParallel)
	)
	for i := 0; i < len(need); i += blobBatch {
		batch := need[i:min(i+blobBatch, len(need))]
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			objs, err := r.fetch(ctx, wire.FetchRequest{Wants: batch, Filter: "blob:none"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				for _, h := range batch {
					o, ok := objs[h]
					if !ok || o.Type != object.TypeBlob {
						err = fmt.Errorf("gitq: server did not send blob %s", h)
						break
					}
					out[h] = o.Data
				}
			}
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repo) getContent(ctx context.Context, ref, p string) ([]byte, error) {
	p = cleanPath(p)
	_, c, err := r.rootOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	e, ok, err := r.entryAt(ctx, c.Tree, p)
	if err != nil {
		return nil, err
	}
	if !ok || isDir(e.Mode) || e.Mode == object.ModeSubmodule {
		return nil, notFound("file %q at %s", p, ref)
	}
	m, err := r.fetchBlobs(ctx, []object.Hash{e.Hash})
	if err != nil {
		return nil, err
	}
	return m[e.Hash], nil
}

func (r *repo) getContents(ctx context.Context, ref string, paths []string) (map[string][]byte, error) {
	_, c, err := r.rootOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	want := make(map[string]object.Hash, len(paths))
	hashes := make([]object.Hash, 0, len(paths))
	for _, orig := range paths {
		e, ok, err := r.entryAt(ctx, c.Tree, cleanPath(orig))
		if err != nil {
			return nil, err
		}
		if !ok || isDir(e.Mode) || e.Mode == object.ModeSubmodule {
			continue
		}
		want[orig] = e.Hash
		hashes = append(hashes, e.Hash)
	}
	blobs, err := r.fetchBlobs(ctx, hashes)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(want))
	for p, h := range want {
		out[p] = blobs[h]
	}
	return out, nil
}

// files lists blobs and symlinks under dir of the given root tree, sorted by path.
func (r *repo) files(ctx context.Context, root object.Hash, dir string) ([]File, error) {
	dir = cleanPath(dir)
	e, ok, err := r.entryAt(ctx, root, dir)
	if err != nil {
		return nil, err
	}
	if !ok || e.Mode == object.ModeSubmodule {
		return nil, notFound("path %q", dir)
	}
	if !isDir(e.Mode) {
		return []File{{Path: dir, SHA: e.Hash.String(), Mode: e.Mode}}, nil
	}
	var out []File
	var walk func(h object.Hash, prefix string) error
	walk = func(h object.Hash, prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		es, err := r.tree(ctx, h)
		if err != nil {
			return err
		}
		for _, e := range es {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			switch {
			case isDir(e.Mode):
				if err := walk(e.Hash, p); err != nil {
					return err
				}
			case e.Mode == object.ModeSubmodule:
			default:
				out = append(out, File{Path: p, SHA: e.Hash.String(), Mode: e.Mode})
			}
		}
		return nil
	}
	if err := walk(e.Hash, dir); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r *repo) listFiles(ctx context.Context, ref, dir string) ([]File, error) {
	_, c, err := r.rootOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	return r.files(ctx, c.Tree, dir)
}
