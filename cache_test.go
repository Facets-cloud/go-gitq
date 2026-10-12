package gitq

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/facets-cloud/go-gitq/internal/object"
)

func blobObj(d []byte) object.Object { return object.Object{Type: object.TypeBlob, Data: d} }

func TestObjLRUCompressAndEvict(t *testing.T) {
	for _, compress := range []bool{true, false} {
		t.Run(fmt.Sprint("compress=", compress), func(t *testing.T) {
			c := newObjLRU(64<<10, compress)
			blob := func(i int) []byte { return bytes.Repeat([]byte(fmt.Sprintf("line %d of some go source\n", i)), 400) } // ~10KB
			for i := range 50 {
				d := blob(i)
				c.put(object.HashOf(object.TypeBlob, d), blobObj(d))
			}
			st := c.stats()
			if st.StoredBytes > 64<<10 {
				t.Fatalf("stored %d exceeds cap", st.StoredBytes)
			}
			if compress && st.FileBytes <= st.StoredBytes*3 {
				t.Fatalf("expected compression: raw %d stored %d", st.FileBytes, st.StoredBytes)
			}
			if !compress && st.Files > 6 {
				t.Fatalf("uncompressed cache holds %d 10KB blobs in 64KB", st.Files)
			}
			last := blob(49)
			got, ok := c.get(object.HashOf(object.TypeBlob, last))
			if !ok || got.Type != object.TypeBlob || !bytes.Equal(got.Data, last) {
				t.Fatal("most recent blob missing or corrupted")
			}
			if _, ok := c.get(object.HashOf(object.TypeBlob, blob(0))); ok && !compress {
				t.Fatal("oldest blob should have been evicted")
			}
			t.Logf("held %d blobs, %d KB stored for %d KB of content", st.Files, st.StoredBytes>>10, st.FileBytes>>10)
		})
	}
}

// Trees and commits share the budget with blobs and are evicted like them.
func TestObjLRUBoundsTreesAndCommits(t *testing.T) {
	const budget = 32 << 10
	c := newObjLRU(budget, true)
	for i := range 2000 {
		tree := object.EncodeTree([]object.TreeEntry{{Mode: object.ModeFile, Name: fmt.Sprintf("f%d", i), Hash: object.Hash{byte(i), byte(i >> 8)}}})
		c.put(object.HashOf(object.TypeTree, tree), object.Object{Type: object.TypeTree, Data: tree})
		commit := []byte(fmt.Sprintf("tree %040x\nauthor a <a> 0 +0000\ncommitter a <a> 0 +0000\n\nc%d\n", i, i))
		c.put(object.HashOf(object.TypeCommit, commit), object.Object{Type: object.TypeCommit, Data: commit})
	}
	st := c.stats()
	if st.StoredBytes > budget {
		t.Fatalf("stored %d exceeds budget %d", st.StoredBytes, budget)
	}
	if st.Trees == 0 || st.Commits == 0 || st.Trees+st.Commits > budget/itemOverhead {
		t.Fatalf("unexpected counts %+v", st)
	}
}

func TestObjLRUDisabled(t *testing.T) {
	c := newObjLRU(0, true)
	c.put(object.Hash{1}, blobObj([]byte("x")))
	if st := c.stats(); st.Files != 0 {
		t.Fatal("NewCache(0) should cache nothing")
	}
}
