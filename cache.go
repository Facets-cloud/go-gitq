package gitq

import (
	"bytes"
	"compress/flate"
	"container/list"
	"io"
	"sync"

	"github.com/facets-cloud/go-gitq/internal/object"
)

// compressMin is the smallest blob worth compressing; below it the flate
// overhead outweighs the saving.
const compressMin = 512

// itemOverhead approximates the memory an entry costs beyond its data (list
// element, map slot, item struct), so budgets hold for many small trees too.
const itemOverhead = 128

// objLRU is an LRU of git objects (commits, trees and blobs) bounded by stored
// bytes. Objects are keyed by hash, so one objLRU can serve every repository.
// When compress is set, blobs are kept flate-compressed and inflated on every
// hit, so the same budget holds several times more source code at the cost of
// microseconds per read. Commits and trees are stored raw: they are small or
// mostly hashes, which do not compress.
type objLRU struct {
	mu       sync.Mutex
	max      int64
	compress bool
	size     int64 // stored bytes including itemOverhead
	ll       *list.List
	items    map[object.Hash]*list.Element
	counts   [object.TypeTag + 1]int   // items per type
	raw      [object.TypeTag + 1]int64 // uncompressed bytes per type
}

type objItem struct {
	h      object.Hash
	typ    object.Type
	data   []byte // flate stream when packed, else the content itself
	packed bool
	rawLen int
}

func (it *objItem) cost() int64 { return int64(len(it.data)) + itemOverhead }

var flateWriters = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(nil, flate.BestSpeed)
	return w
}}

var flateReaders = sync.Pool{New: func() any { return flate.NewReader(bytes.NewReader(nil)) }}

func newObjLRU(max int64, compress bool) *objLRU {
	return &objLRU{max: max, compress: compress, ll: list.New(), items: make(map[object.Hash]*list.Element)}
}

// get returns a cached object, inflating it if stored compressed.
func (c *objLRU) get(h object.Hash) (object.Object, bool) {
	c.mu.Lock()
	e, ok := c.items[h]
	if !ok {
		c.mu.Unlock()
		return object.Object{}, false
	}
	c.ll.MoveToFront(e)
	it := e.Value.(*objItem)
	c.mu.Unlock()
	if !it.packed {
		return object.Object{Type: it.typ, Data: it.data}, true
	}
	out := make([]byte, 0, it.rawLen)
	fr := flateReaders.Get().(io.ReadCloser)
	fr.(flate.Resetter).Reset(bytes.NewReader(it.data), nil)
	out, err := readAllInto(out, fr)
	flateReaders.Put(fr)
	if err != nil || len(out) != it.rawLen {
		return object.Object{}, false // treat a bad entry as a miss
	}
	return object.Object{Type: it.typ, Data: out}, true
}

func (c *objLRU) put(h object.Hash, o object.Object) {
	if c.max <= 0 || int64(len(o.Data))+itemOverhead > c.max/4 {
		return // disabled, or too big to be worth evicting everything else for
	}
	c.mu.Lock()
	if e, ok := c.items[h]; ok {
		c.ll.MoveToFront(e)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	it := &objItem{h: h, typ: o.Type, data: o.Data, rawLen: len(o.Data)}
	if c.compress && o.Type == object.TypeBlob && len(o.Data) >= compressMin {
		var buf bytes.Buffer
		buf.Grow(len(o.Data) / 3)
		w := flateWriters.Get().(*flate.Writer)
		w.Reset(&buf)
		w.Write(o.Data)
		w.Close()
		flateWriters.Put(w)
		if buf.Len() < len(o.Data)*9/10 { // keep it only if it saves >10%
			it.data, it.packed = bytes.Clone(buf.Bytes()), true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[h]; ok { // raced with another put
		c.ll.MoveToFront(e)
		return
	}
	c.items[h] = c.ll.PushFront(it)
	c.size += it.cost()
	c.counts[it.typ]++
	c.raw[it.typ] += int64(it.rawLen)
	for c.size > c.max {
		e := c.ll.Back()
		old := e.Value.(*objItem)
		c.ll.Remove(e)
		delete(c.items, old.h)
		c.size -= old.cost()
		c.counts[old.typ]--
		c.raw[old.typ] -= int64(old.rawLen)
	}
}

func (c *objLRU) stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Files:       c.counts[object.TypeBlob],
		Trees:       c.counts[object.TypeTree],
		Commits:     c.counts[object.TypeCommit],
		StoredBytes: c.size,
		FileBytes:   c.raw[object.TypeBlob],
	}
}

func readAllInto(b []byte, r io.Reader) ([]byte, error) {
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
	}
}
