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

// blobLRU is an LRU of blob contents bounded by stored bytes. When compress is
// set, blobs are kept flate-compressed and inflated on every hit, so the same
// budget holds several times more source code at the cost of microseconds per read.
type blobLRU struct {
	mu       sync.Mutex
	max      int64
	compress bool
	size     int64 // stored bytes
	raw      int64 // uncompressed bytes of what is stored
	ll       *list.List
	items    map[object.Hash]*list.Element
}

type blobItem struct {
	h      object.Hash
	data   []byte // flate stream when packed, else the content itself
	packed bool
	rawLen int
}

var flateWriters = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(nil, flate.BestSpeed)
	return w
}}

var flateReaders = sync.Pool{New: func() any { return flate.NewReader(bytes.NewReader(nil)) }}

func newBlobLRU(max int64, compress bool) *blobLRU {
	return &blobLRU{max: max, compress: compress, ll: list.New(), items: make(map[object.Hash]*list.Element)}
}

func (c *blobLRU) get(h object.Hash) ([]byte, bool) {
	c.mu.Lock()
	e, ok := c.items[h]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	c.ll.MoveToFront(e)
	it := e.Value.(*blobItem)
	c.mu.Unlock()
	if !it.packed {
		return it.data, true
	}
	out := make([]byte, 0, it.rawLen)
	fr := flateReaders.Get().(io.ReadCloser)
	fr.(flate.Resetter).Reset(bytes.NewReader(it.data), nil)
	out, err := readAllInto(out, fr)
	flateReaders.Put(fr)
	if err != nil || len(out) != it.rawLen {
		return nil, false // treat a bad entry as a miss
	}
	return out, true
}

func (c *blobLRU) put(h object.Hash, data []byte) {
	if c.max <= 0 || int64(len(data)) > c.max/4 {
		return // disabled, or too big to be worth evicting everything else for
	}
	c.mu.Lock()
	if e, ok := c.items[h]; ok {
		c.ll.MoveToFront(e)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	it := &blobItem{h: h, data: data, rawLen: len(data)}
	if c.compress && len(data) >= compressMin {
		var buf bytes.Buffer
		buf.Grow(len(data) / 3)
		w := flateWriters.Get().(*flate.Writer)
		w.Reset(&buf)
		w.Write(data)
		w.Close()
		flateWriters.Put(w)
		if buf.Len() < len(data)*9/10 { // keep it only if it saves >10%
			it.data, it.packed = buf.Bytes(), true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[h]; ok { // raced with another put
		c.ll.MoveToFront(e)
		return
	}
	c.items[h] = c.ll.PushFront(it)
	c.size += int64(len(it.data))
	c.raw += int64(it.rawLen)
	for c.size > c.max {
		e := c.ll.Back()
		old := e.Value.(*blobItem)
		c.ll.Remove(e)
		delete(c.items, old.h)
		c.size -= int64(len(old.data))
		c.raw -= int64(old.rawLen)
	}
}

func (c *blobLRU) stats() (n int, stored, raw int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.size, c.raw
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
