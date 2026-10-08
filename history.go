package gitq

import (
	"context"
	"sort"
	"strings"

	"github.com/facets-cloud/go-gitq/internal/object"
	"github.com/facets-cloud/go-gitq/internal/wire"
)

// historyCommit returns a commit, fetching historyDepth commits of history
// starting at h if it is not cached.
func (r *repo) historyCommit(ctx context.Context, h object.Hash) (*object.Commit, error) {
	c, ok, err := r.cachedCommit(h)
	if err != nil || ok {
		return c, err
	}
	_, err = r.sf.do(ctx, "hist:"+h.String(), func() (any, error) {
		return r.fetch(ctx, wire.FetchRequest{Wants: []object.Hash{h}, Depth: r.cfg.historyDepth, Filter: "blob:none"})
	})
	if err != nil {
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

func (r *repo) getHistory(ctx context.Context, ref, p string, limit int) ([]Commit, error) {
	p = cleanPath(p)
	h, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []Commit
	c, err := r.historyCommit(ctx, h)
	if err != nil {
		return nil, err
	}
	cur, curOK, err := r.entryAt(ctx, c.Tree, p)
	if err != nil {
		return nil, err
	}
	for limit <= 0 || len(out) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var (
			parent       *object.Commit
			prev         object.TreeEntry
			prevOK       bool
			parentHash   object.Hash
			parentExists = len(c.Parents) > 0
		)
		if parentExists {
			parentHash = c.Parents[0]
			if parent, err = r.historyCommit(ctx, parentHash); err != nil {
				return nil, err
			}
			if prev, prevOK, err = r.entryAt(ctx, parent.Tree, p); err != nil {
				return nil, err
			}
		}
		touched := curOK != prevOK || (curOK && (cur.Hash != prev.Hash || cur.Mode != prev.Mode))
		if touched {
			out = append(out, Commit{
				SHA:     h.String(),
				Author:  c.Author.Name,
				Email:   c.Author.Email,
				Date:    c.Author.When,
				Message: strings.TrimRight(c.Message, "\n"),
			})
		}
		if !parentExists {
			break
		}
		h, c, cur, curOK = parentHash, parent, prev, prevOK
	}
	return out, nil
}

func (r *repo) getChanges(ctx context.Context, fromRef, toRef string) ([]Change, error) {
	_, from, err := r.rootOf(ctx, fromRef)
	if err != nil {
		return nil, err
	}
	_, to, err := r.rootOf(ctx, toRef)
	if err != nil {
		return nil, err
	}
	var out []Change
	if err := r.diffTrees(ctx, "", from.Tree, to.Tree, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Status < out[j].Status
	})
	return out, nil
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// diffTrees appends file changes between trees a and b (either may be zero = absent).
func (r *repo) diffTrees(ctx context.Context, prefix string, a, b object.Hash, out *[]Change) error {
	if a == b {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var ea, eb []object.TreeEntry
	var err error
	if !a.IsZero() {
		if ea, err = r.tree(ctx, a); err != nil {
			return err
		}
	}
	if !b.IsZero() {
		if eb, err = r.tree(ctx, b); err != nil {
			return err
		}
	}
	ma := make(map[string]object.TreeEntry, len(ea))
	for _, e := range ea {
		ma[e.Name] = e
	}
	mb := make(map[string]object.TreeEntry, len(eb))
	for _, e := range eb {
		mb[e.Name] = e
	}
	// side splits an entry into (tree hash, file entry) halves.
	side := func(e object.TreeEntry, ok bool) (tree object.Hash, file *object.TreeEntry) {
		switch {
		case !ok || e.Mode == object.ModeSubmodule:
		case isDir(e.Mode):
			tree = e.Hash
		default:
			file = &e
		}
		return
	}
	names := make(map[string]struct{}, len(ma)+len(mb))
	for n := range ma {
		names[n] = struct{}{}
	}
	for n := range mb {
		names[n] = struct{}{}
	}
	for n := range names {
		x, okA := ma[n]
		y, okB := mb[n]
		p := joinPath(prefix, n)
		ta, fa := side(x, okA)
		tb, fb := side(y, okB)
		switch {
		case fa != nil && fb != nil:
			if fa.Hash != fb.Hash || fa.Mode != fb.Mode {
				*out = append(*out, Change{Status: "M", Path: p})
			}
		case fa != nil:
			*out = append(*out, Change{Status: "D", Path: p})
		case fb != nil:
			*out = append(*out, Change{Status: "A", Path: p})
		}
		if err := r.diffTrees(ctx, p, ta, tb, out); err != nil {
			return err
		}
	}
	return nil
}
