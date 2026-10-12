// Package pack reads and writes git packfiles.
package pack

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/facets-cloud/go-gitq/internal/object"
)

// Lookup returns an already-known object, used to resolve REF_DELTA bases that
// are not inside the pack itself (thin packs). It may be nil.
type Lookup func(object.Hash) (object.Object, bool)

const (
	typeOfsDelta = 6
	typeRefDelta = 7
)

// entry is one object as read from the pack, before delta resolution.
type entry struct {
	offset   int64
	typ      object.Type // resolved type (0 until resolved, for deltas)
	data     []byte      // resolved data, or delta instructions until resolved
	isDelta  bool
	resolved bool
	baseOff  int64       // OFS_DELTA base offset
	baseHash object.Hash // REF_DELTA base id
	isRef    bool
	hash     object.Hash
}

// Limits bounds the memory a pack from an untrusted server can make Decode
// allocate. Zero fields mean no limit.
type Limits struct {
	// MaxObject is the largest object (or delta) size accepted, checked against
	// the size declared in the pack before anything is allocated.
	MaxObject int64
	// MaxTotal caps the sum of all inflated objects and delta results.
	MaxTotal int64
}

// ErrTooLarge is returned when a pack exceeds Limits.
var ErrTooLarge = errors.New("pack: exceeds size limit")

// Decode reads a complete packfile ("PACK" header .. trailing checksum) and
// returns every object fully resolved (OFS_DELTA and REF_DELTA applied), keyed by id.
// The trailing checksum is verified.
func Decode(r io.Reader, lookup Lookup) (map[object.Hash]object.Object, error) {
	return DecodeLimited(r, lookup, Limits{})
}

// DecodeLimited is Decode with size limits.
func DecodeLimited(r io.Reader, lookup Lookup, lim Limits) (map[object.Hash]object.Object, error) {
	pr := newPackReader(r)
	var total int64
	charge := func(n uint64) error {
		if n > 1<<40 {
			return fmt.Errorf("%w: object of %d bytes", ErrTooLarge, n)
		}
		if lim.MaxObject > 0 && n > uint64(lim.MaxObject) {
			return fmt.Errorf("%w: object of %d bytes, limit %d", ErrTooLarge, n, lim.MaxObject)
		}
		total += int64(n)
		if lim.MaxTotal > 0 && total > lim.MaxTotal {
			return fmt.Errorf("%w: more than %d bytes of objects", ErrTooLarge, lim.MaxTotal)
		}
		return nil
	}

	var hdr [12]byte
	if _, err := io.ReadFull(pr, hdr[:]); err != nil {
		return nil, fmt.Errorf("pack: reading header: %w", err)
	}
	if string(hdr[:4]) != "PACK" {
		return nil, errors.New("pack: bad signature")
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return nil, fmt.Errorf("pack: unsupported version %d", v)
	}
	count := binary.BigEndian.Uint32(hdr[8:12])

	// count comes from the server: grow towards it rather than trusting it.
	hint := int(min(count, 1<<14))
	entries := make([]entry, 0, hint)
	byOffset := make(map[int64]int, hint)
	var zr io.ReadCloser

	for i := 0; i < int(count); i++ {
		entries = append(entries, entry{})
		e := &entries[i]
		e.offset = pr.offset()
		byOffset[e.offset] = i

		b, err := pr.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("pack: object %d header: %w", i, err)
		}
		typ := (b >> 4) & 7
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if b, err = pr.ReadByte(); err != nil {
				return nil, fmt.Errorf("pack: object %d header: %w", i, err)
			}
			if shift > 63 {
				return nil, errors.New("pack: object size overflow")
			}
			size |= uint64(b&0x7f) << shift
			shift += 7
		}

		switch typ {
		case 1, 2, 3, 4:
			e.typ = object.Type(typ)
		case typeOfsDelta:
			e.isDelta = true
			b, err := pr.ReadByte()
			if err != nil {
				return nil, err
			}
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				if b, err = pr.ReadByte(); err != nil {
					return nil, err
				}
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			if off <= 0 || off > e.offset {
				return nil, fmt.Errorf("pack: object %d bad OFS_DELTA offset %d", i, off)
			}
			e.baseOff = e.offset - off
		case typeRefDelta:
			e.isDelta, e.isRef = true, true
			if _, err := io.ReadFull(pr, e.baseHash[:]); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pack: object %d has invalid type %d", i, typ)
		}

		if zr == nil {
			zr, err = zlib.NewReader(pr)
		} else {
			err = zr.(zlib.Resetter).Reset(pr, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("pack: object %d zlib: %w", i, err)
		}
		if err := charge(size); err != nil {
			return nil, fmt.Errorf("pack: object %d: %w", i, err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(zr, data); err != nil {
			return nil, fmt.Errorf("pack: object %d inflate: %w", i, err)
		}
		// Drain to EOF so the zlib trailer is consumed and verified.
		var one [1]byte
		if n, err := zr.Read(one[:]); n != 0 || err != io.EOF {
			if err == nil || n != 0 {
				err = errors.New("object larger than declared size")
			}
			return nil, fmt.Errorf("pack: object %d inflate: %w", i, err)
		}
		e.data = data
		if !e.isDelta {
			e.resolved = true
			e.hash = object.HashOf(e.typ, e.data)
		}
	}

	sum := pr.sum()
	var trailer [20]byte
	if _, err := io.ReadFull(pr, trailer[:]); err != nil {
		return nil, fmt.Errorf("pack: reading checksum: %w", err)
	}
	if !bytes.Equal(sum, trailer[:]) {
		return nil, errors.New("pack: checksum mismatch")
	}

	// Delta resolution: index dependents by base, then propagate from every
	// resolved object (linear in the number of objects).
	ofsChildren := make(map[int][]int)
	refChildren := make(map[object.Hash][]int)
	var queue []int
	for i := range entries {
		e := &entries[i]
		switch {
		case !e.isDelta:
			queue = append(queue, i)
		case e.isRef:
			refChildren[e.baseHash] = append(refChildren[e.baseHash], i)
		default:
			bi, ok := byOffset[e.baseOff]
			if !ok {
				return nil, fmt.Errorf("pack: OFS_DELTA at %d points to unknown offset %d", e.offset, e.baseOff)
			}
			ofsChildren[bi] = append(ofsChildren[bi], i)
		}
	}

	out := make(map[object.Hash]object.Object, count)
	resolveFrom := func(baseTyp object.Type, baseData []byte, children []int) error {
		for _, ci := range children {
			c := &entries[ci]
			if c.resolved {
				continue
			}
			res, err := applyDelta(baseData, c.data, charge)
			if err != nil {
				return fmt.Errorf("pack: delta at offset %d: %w", c.offset, err)
			}
			c.typ, c.data, c.resolved = baseTyp, res, true
			c.hash = object.HashOf(c.typ, c.data)
			queue = append(queue, ci)
		}
		return nil
	}
	drain := func() error {
		for len(queue) > 0 {
			i := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			e := &entries[i]
			out[e.hash] = object.Object{Type: e.typ, Data: e.data}
			if err := resolveFrom(e.typ, e.data, ofsChildren[i]); err != nil {
				return err
			}
			if kids, ok := refChildren[e.hash]; ok {
				delete(refChildren, e.hash)
				if err := resolveFrom(e.typ, e.data, kids); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := drain(); err != nil {
		return nil, err
	}
	// Remaining REF_DELTAs need bases from outside the pack (thin pack).
	for len(refChildren) > 0 {
		progressed := false
		for h, kids := range refChildren {
			if lookup == nil {
				break
			}
			base, ok := lookup(h)
			if !ok {
				continue
			}
			delete(refChildren, h)
			progressed = true
			if err := resolveFrom(base.Type, base.Data, kids); err != nil {
				return nil, err
			}
			if err := drain(); err != nil {
				return nil, err
			}
		}
		if !progressed {
			for h := range refChildren {
				return nil, fmt.Errorf("pack: missing REF_DELTA base %s", h)
			}
		}
	}
	for i := range entries {
		if !entries[i].resolved {
			return nil, fmt.Errorf("pack: unresolved delta at offset %d", entries[i].offset)
		}
	}
	return out, nil
}

// applyDelta applies git delta instructions to base. charge, if set, approves
// the declared result size before it is allocated.
func applyDelta(base, delta []byte, charge func(uint64) error) ([]byte, error) {
	srcSize, delta, err := deltaVarint(delta)
	if err != nil {
		return nil, err
	}
	if srcSize != uint64(len(base)) {
		return nil, fmt.Errorf("delta base size %d, have %d", srcSize, len(base))
	}
	dstSize, delta, err := deltaVarint(delta)
	if err != nil {
		return nil, err
	}
	if charge != nil {
		if err := charge(dstSize); err != nil {
			return nil, err
		}
	}
	out := make([]byte, 0, dstSize)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0:
			var off, sz uint64
			for k := uint(0); k < 4; k++ {
				if op&(1<<k) != 0 {
					if len(delta) == 0 {
						return nil, errors.New("truncated copy op")
					}
					off |= uint64(delta[0]) << (8 * k)
					delta = delta[1:]
				}
			}
			for k := uint(0); k < 3; k++ {
				if op&(0x10<<k) != 0 {
					if len(delta) == 0 {
						return nil, errors.New("truncated copy op")
					}
					sz |= uint64(delta[0]) << (8 * k)
					delta = delta[1:]
				}
			}
			if sz == 0 {
				sz = 0x10000
			}
			if off+sz > uint64(len(base)) || off+sz < off {
				return nil, errors.New("copy op out of range")
			}
			out = append(out, base[off:off+sz]...)
		case op != 0:
			n := int(op)
			if n > len(delta) {
				return nil, errors.New("truncated insert op")
			}
			out = append(out, delta[:n]...)
			delta = delta[n:]
		default:
			return nil, errors.New("reserved delta opcode 0")
		}
	}
	if uint64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta result size %d, want %d", len(out), dstSize)
	}
	return out, nil
}

func deltaVarint(b []byte) (uint64, []byte, error) {
	var v uint64
	var shift uint
	for i, c := range b {
		if shift > 63 {
			break
		}
		v |= uint64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return v, b[i+1:], nil
		}
	}
	return 0, nil, errors.New("bad delta size varint")
}

// Encode writes a version-2 packfile containing objs, undeltified, with trailer.
func Encode(w io.Writer, objs []object.Object) error {
	h := sha1.New()
	bw := bufio.NewWriter(io.MultiWriter(w, h))
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objs)))
	bw.Write(hdr[:])
	zw := zlib.NewWriter(bw)
	for _, o := range objs {
		if o.Type < object.TypeCommit || o.Type > object.TypeTag {
			return fmt.Errorf("pack: cannot encode object type %d", o.Type)
		}
		size := uint64(len(o.Data))
		var buf [16]byte
		n := 0
		c := byte(o.Type)<<4 | byte(size&0x0f)
		size >>= 4
		for size != 0 {
			buf[n] = c | 0x80
			n++
			c = byte(size & 0x7f)
			size >>= 7
		}
		buf[n] = c
		n++
		bw.Write(buf[:n])
		zw.Reset(bw)
		if _, err := zw.Write(o.Data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, err := w.Write(h.Sum(nil))
	return err
}

// packReader is a minimal buffered reader implementing io.ByteReader (so
// flate never reads past an object's compressed stream), tracking the absolute
// offset and hashing every consumed byte for the trailer check.
type packReader struct {
	r      io.Reader
	buf    []byte
	pos    int
	end    int
	base   int64 // absolute offset of buf[0]
	hashed int   // buf[:hashed] already fed to h
	h      interface {
		io.Writer
		Sum([]byte) []byte
	}
	err error
}

func newPackReader(r io.Reader) *packReader {
	return &packReader{r: r, buf: make([]byte, 64<<10), h: sha1.New()}
}

func (p *packReader) offset() int64 { return p.base + int64(p.pos) }

func (p *packReader) fill() error {
	if p.err != nil {
		return p.err
	}
	p.h.Write(p.buf[p.hashed:p.pos])
	n := copy(p.buf, p.buf[p.pos:p.end])
	p.base += int64(p.pos)
	p.pos, p.end, p.hashed = 0, n, 0
	for p.end == n {
		m, err := p.r.Read(p.buf[p.end:])
		p.end += m
		if err != nil {
			if err == io.EOF && p.end > n {
				p.err = err
				return nil
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			p.err = err
			if p.end > n {
				return nil
			}
			return err
		}
	}
	return nil
}

func (p *packReader) ReadByte() (byte, error) {
	if p.pos == p.end {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	b := p.buf[p.pos]
	p.pos++
	return b, nil
}

func (p *packReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if p.pos == p.end {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf[p.pos:p.end])
	p.pos += n
	return n, nil
}

// sum returns the SHA-1 of every byte consumed so far.
func (p *packReader) sum() []byte {
	p.h.Write(p.buf[p.hashed:p.pos])
	p.hashed = p.pos
	return p.h.Sum(nil)
}
