package gitq

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/facets-cloud/go-gitq/internal/object"
)

func TestBlobLRUCompressAndEvict(t *testing.T) {
	for _, compress := range []bool{true, false} {
		t.Run(fmt.Sprint("compress=", compress), func(t *testing.T) {
			c := newBlobLRU(64<<10, compress)
			blob := func(i int) []byte { return bytes.Repeat([]byte(fmt.Sprintf("line %d of some go source\n", i)), 400) } // ~10KB
			for i := range 50 {
				d := blob(i)
				c.put(object.HashOf(object.TypeBlob, d), d)
			}
			n, stored, raw := c.stats()
			if stored > 64<<10 {
				t.Fatalf("stored %d exceeds cap", stored)
			}
			if compress && raw <= stored*3 {
				t.Fatalf("expected compression: raw %d stored %d", raw, stored)
			}
			if !compress && n > 6 {
				t.Fatalf("uncompressed cache holds %d 10KB blobs in 64KB", n)
			}
			last := blob(49)
			got, ok := c.get(object.HashOf(object.TypeBlob, last))
			if !ok || !bytes.Equal(got, last) {
				t.Fatal("most recent blob missing or corrupted")
			}
			if _, ok := c.get(object.HashOf(object.TypeBlob, blob(0))); ok && !compress {
				t.Fatal("oldest blob should have been evicted")
			}
			t.Logf("held %d blobs, %d KB stored for %d KB of content", n, stored>>10, raw>>10)
		})
	}
}

func TestBlobLRUDisabled(t *testing.T) {
	c := newBlobLRU(0, true)
	c.put(object.Hash{1}, []byte("x"))
	if n, _, _ := c.stats(); n != 0 {
		t.Fatal("CacheSize(0) should cache nothing")
	}
}
