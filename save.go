package gitq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/facets-cloud/go-gitq/internal/object"
	"github.com/facets-cloud/go-gitq/internal/pack"
	"github.com/facets-cloud/go-gitq/internal/wire"
)

// editNode is one path component of the requested edits.
type editNode struct {
	leaf     bool
	data     []byte // nil with leaf = delete
	children map[string]*editNode
}

func buildEdits(files map[string][]byte) (*editNode, error) {
	root := &editNode{children: map[string]*editNode{}}
	for orig, data := range files {
		p := cleanPath(orig)
		if p == "" || strings.HasPrefix(p, "../") || p == ".." {
			return nil, fmt.Errorf("gitq: invalid path %q", orig)
		}
		parts := strings.Split(p, "/")
		n := root
		for i, part := range parts {
			if n.leaf {
				return nil, fmt.Errorf("gitq: path %q conflicts with another file in the same save", orig)
			}
			child, ok := n.children[part]
			if !ok {
				child = &editNode{}
				n.children[part] = child
			}
			if i == len(parts)-1 {
				if child.children != nil || child.leaf {
					return nil, fmt.Errorf("gitq: path %q conflicts with another file in the same save", orig)
				}
				child.leaf, child.data = true, data
			} else if child.children == nil {
				if child.leaf {
					return nil, fmt.Errorf("gitq: path %q conflicts with another file in the same save", orig)
				}
				child.children = map[string]*editNode{}
			}
			n = child
		}
	}
	return root, nil
}

// applyEdits rewrites tree h (zero = none) with the edits in n, appending newly
// created objects to objs. It returns the new tree hash, or zero if the tree is empty.
func (r *repo) applyEdits(ctx context.Context, h object.Hash, n *editNode, objs *[]object.Object) (object.Hash, error) {
	var entries []object.TreeEntry
	if !h.IsZero() {
		es, err := r.tree(ctx, h)
		if err != nil {
			return object.ZeroHash, err
		}
		entries = append(entries, es...)
	}
	for name, child := range n.children {
		i, exists := findEntry(entries, name)
		var cur object.TreeEntry
		if exists {
			cur = entries[i]
		}
		var ne object.TreeEntry
		keep := false
		if child.leaf {
			if child.data != nil {
				bh := object.HashOf(object.TypeBlob, child.data)
				mode := object.ModeFile
				if exists && !isDir(cur.Mode) && cur.Mode != object.ModeSubmodule {
					mode = cur.Mode
				}
				if !exists || cur.Hash != bh {
					*objs = append(*objs, object.Object{Type: object.TypeBlob, Data: child.data})
				}
				ne, keep = object.TreeEntry{Name: name, Mode: mode, Hash: bh}, true
			}
		} else {
			var sub object.Hash
			if exists && isDir(cur.Mode) {
				sub = cur.Hash
			}
			nh, err := r.applyEdits(ctx, sub, child, objs)
			if err != nil {
				return object.ZeroHash, err
			}
			if !nh.IsZero() {
				ne, keep = object.TreeEntry{Name: name, Mode: object.ModeDir, Hash: nh}, true
			}
		}
		switch {
		case keep && exists:
			entries[i] = ne
		case keep:
			entries = append(entries, ne)
		case exists:
			entries = append(entries[:i], entries[i+1:]...)
		}
	}
	if len(entries) == 0 {
		return object.ZeroHash, nil
	}
	data := object.EncodeTree(entries)
	th := object.HashOf(object.TypeTree, data)
	if th != h {
		*objs = append(*objs, object.Object{Type: object.TypeTree, Data: data})
	}
	return th, nil
}

func (r *repo) save(ctx context.Context, branch, expectedSHA, message string, author Author, files map[string][]byte) (string, error) {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" {
		return "", errors.New("gitq: empty branch name")
	}
	edits, err := buildEdits(files)
	if err != nil {
		return "", err
	}
	var parent object.Hash
	var baseTree object.Hash
	if expectedSHA != "" {
		if parent, err = object.ParseHash(strings.ToLower(expectedSHA)); err != nil {
			return "", fmt.Errorf("gitq: expectedSHA: %w", err)
		}
		c, err := r.commit(ctx, parent)
		if err != nil {
			return "", err
		}
		if err := r.ensureCommit(ctx, parent); err != nil {
			return "", err
		}
		baseTree = c.Tree
	}

	var objs []object.Object
	root, err := r.applyEdits(ctx, baseTree, edits, &objs)
	if err != nil {
		return "", err
	}
	if root.IsZero() { // everything deleted: commit the empty tree
		empty := object.Object{Type: object.TypeTree, Data: []byte{}}
		root = empty.Hash()
		objs = append(objs, empty)
	}
	if message != "" && !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	sig := object.Signature{Name: author.Name, Email: author.Email, When: time.Now()}
	commit := &object.Commit{Tree: root, Author: sig, Committer: sig, Message: message}
	if !parent.IsZero() {
		commit.Parents = []object.Hash{parent}
	}
	cobj := object.Object{Type: object.TypeCommit, Data: object.EncodeCommit(commit)}
	objs = append(objs, cobj)
	ch := cobj.Hash()

	var buf bytes.Buffer
	if err := pack.Encode(&buf, objs); err != nil {
		return "", fmt.Errorf("gitq: encoding pack: %w", err)
	}
	upd := wire.RefUpdate{Name: "refs/heads/" + branch, Old: parent, New: ch}
	if err := r.wc.Push(ctx, []wire.RefUpdate{upd}, buf.Bytes()); err != nil {
		r.forgetRef(branch)
		var rej *wire.RejectedError
		if errors.As(err, &rej) {
			return "", fmt.Errorf("gitq: push to %s rejected: %w", branch, err)
		}
		return "", mapErr(err)
	}
	r.forgetRef(branch)

	m := make(map[object.Hash]object.Object, len(objs))
	for _, o := range objs {
		m[o.Hash()] = o
	}
	r.addObjects(m)
	var missing []object.Hash
	if _, err := r.missingTrees(root, &missing, map[object.Hash]bool{}); err != nil {
		return "", err
	}
	return ch.String(), nil
}
