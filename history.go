package gitq

import (
	"container/heap"
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/facets-cloud/go-gitq/internal/object"
)

// historyCommit returns a commit, fetching historyDepth commits of history
// starting at h if it is not cached.
func (r *repo) historyCommit(ctx context.Context, h object.Hash) (*object.Commit, error) {
	if o, ok := r.objs.get(h); ok {
		return parseCommit(h, o)
	}
	return r.fetchCommit(ctx, h, r.cfg.historyDepth)
}

// histItem is a commit waiting in the walk queue.
type histItem struct {
	h   object.Hash
	c   *object.Commit
	seq int // insertion order, to break date ties the way git does
}

// histQueue pops the newest commit by committer date first, like git log's
// default (non-topological) walk.
type histQueue []histItem

func (q histQueue) Len() int { return len(q) }
func (q histQueue) Less(i, j int) bool {
	ti, tj := q[i].c.Committer.When, q[j].c.Committer.When
	if !ti.Equal(tj) {
		return ti.After(tj)
	}
	return q[i].seq < q[j].seq
}
func (q histQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *histQueue) Push(x any)   { *q = append(*q, x.(histItem)) }
func (q *histQueue) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}

// getHistory walks history the way `git log ref -- path` does by default
// (history simplification without --full-history):
//   - a commit is listed when the path differs from every parent (a root
//     commit counts as differing when the path exists in it);
//   - at a merge where the path is unchanged from some parent, only the first
//     such parent is followed, and the merge is not listed;
//   - otherwise every parent is followed.
//
// With path "" every commit is listed and every parent followed, like `git log ref`.
func (r *repo) getHistory(ctx context.Context, ref, p string, limit int) ([]Commit, error) {
	p = cleanPath(p)
	h, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	c, err := r.historyCommit(ctx, h)
	if err != nil {
		return nil, err
	}
	var (
		out  []Commit
		q    histQueue
		seq  int
		seen = map[object.Hash]bool{h: true}
	)
	push := func(h object.Hash, c *object.Commit) {
		heap.Push(&q, histItem{h: h, c: c, seq: seq})
		seq++
	}
	push(h, c)
	for q.Len() > 0 && (limit <= 0 || len(out) < limit) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		it := heap.Pop(&q).(histItem)
		parents := make([]*object.Commit, len(it.c.Parents))
		for i, ph := range it.c.Parents {
			if parents[i], err = r.historyCommit(ctx, ph); err != nil {
				return nil, err
			}
		}
		show, follow := true, it.c.Parents
		if p != "" {
			if show, follow, err = r.simplify(ctx, it.c, parents, p); err != nil {
				return nil, err
			}
		}
		if show {
			out = append(out, Commit{
				SHA:     it.h.String(),
				Author:  it.c.Author.Name,
				Email:   it.c.Author.Email,
				Date:    it.c.Author.When,
				Message: strings.TrimRight(it.c.Message, "\n"),
			})
		}
		for i, ph := range it.c.Parents {
			if !slices.Contains(follow, ph) || seen[ph] {
				continue
			}
			seen[ph] = true
			push(ph, parents[i])
		}
	}
	return out, nil
}

// simplify decides, for path p, whether commit c is listed and which parents
// the walk follows, per git's default history simplification.
func (r *repo) simplify(ctx context.Context, c *object.Commit, parents []*object.Commit, p string) (show bool, follow []object.Hash, err error) {
	cur, curOK, err := r.entryAt(ctx, c.Tree, p)
	if err != nil {
		return false, nil, err
	}
	if len(parents) == 0 {
		return curOK, nil, nil
	}
	for i, pc := range parents {
		prev, prevOK, err := r.entryAt(ctx, pc.Tree, p)
		if err != nil {
			return false, nil, err
		}
		same := curOK == prevOK && (!curOK || cur.Hash == prev.Hash && cur.Mode == prev.Mode)
		if same {
			return false, c.Parents[i : i+1], nil
		}
	}
	return true, c.Parents, nil
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
