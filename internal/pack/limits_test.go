package pack

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/facets-cloud/go-gitq/internal/object"
)

// rawHeader builds a pack header claiming count objects followed by body.
func rawHeader(count uint32, body []byte) []byte {
	var out bytes.Buffer
	out.WriteString("PACK")
	binary.Write(&out, binary.BigEndian, uint32(2))
	binary.Write(&out, binary.BigEndian, count)
	out.Write(body)
	return out.Bytes()
}

// A server declaring a huge object must be refused before anything is allocated.
func TestDecodeRefusesHugeDeclaredSize(t *testing.T) {
	var p rawPack
	p.entry(byte(object.TypeBlob), 1<<34, nil, []byte("tiny")) // claims 16 GiB
	_, err := DecodeLimited(bytes.NewReader(p.bytes()), nil, Limits{MaxObject: 1 << 20})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// Even without limits, absurd sizes are rejected instead of panicking.
	var q rawPack
	q.entry(byte(object.TypeBlob), 1<<50, nil, []byte("tiny"))
	if _, err := Decode(bytes.NewReader(q.bytes()), nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("unlimited err = %v, want ErrTooLarge", err)
	}
}

// A delta whose declared result is huge must be refused before it is allocated.
func TestDecodeRefusesHugeDeltaResult(t *testing.T) {
	base := []byte("base content")
	d := append(varint(len(base)), varint(1<<33)...)
	d = append(d, 0x90, 1) // copy 1 byte
	var p rawPack
	off := p.entry(byte(object.TypeBlob), len(base), nil, base)
	p.entry(typeOfsDelta, len(d), ofsEnc(int64(12+p.buf.Len())-off), d)
	_, err := DecodeLimited(bytes.NewReader(p.bytes()), nil, Limits{MaxObject: 1 << 20})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// Many objects that each pass MaxObject still hit MaxTotal (a zlib bomb split up).
func TestDecodeMaxTotal(t *testing.T) {
	var p rawPack
	big := bytes.Repeat([]byte{0}, 64<<10) // compresses to almost nothing
	for range 100 {
		p.entry(byte(object.TypeBlob), len(big), nil, big)
	}
	raw := p.bytes()
	if len(raw) > 64<<10 {
		t.Fatalf("pack unexpectedly large: %d", len(raw))
	}
	_, err := DecodeLimited(bytes.NewReader(raw), nil, Limits{MaxObject: 1 << 20, MaxTotal: 1 << 20})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := DecodeLimited(bytes.NewReader(raw), nil, Limits{MaxObject: 1 << 20, MaxTotal: 8 << 20}); err != nil {
		t.Fatalf("within limits: %v", err)
	}
}

// A header claiming billions of objects must not preallocate for them.
func TestDecodeHugeCountTruncated(t *testing.T) {
	raw := rawHeader(0xFFFFFFFF, nil)
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Decode(bytes.NewReader(raw), nil); err == nil {
				b.Fatal("decoded a truncated pack")
			}
		}
	})
	if res.AllocedBytesPerOp() > 8<<20 {
		t.Fatalf("truncated pack with huge count allocated %d bytes", res.AllocedBytesPerOp())
	}
}

// FuzzDecode feeds arbitrary bytes to the decoder: it must never panic, and
// must stay within its limits. Seeds are real git packs plus synthetic ones.
//
//	go test ./internal/pack -run '^$' -fuzz FuzzDecode -fuzztime 60s
func FuzzDecode(f *testing.F) {
	if _, err := exec.LookPath("git"); err == nil {
		dir := f.TempDir()
		git(f, dir, nil, "init", "-q", "-b", "main")
		for _, s := range []string{"hello world\n", "hello there\n"} {
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), bytes.Repeat([]byte(s), 50), 0o644); err != nil {
				f.Fatal(err)
			}
			git(f, dir, nil, "add", "-A")
			git(f, dir, nil, "commit", "-q", "-m", s)
		}
		revs := git(f, dir, nil, "rev-list", "--objects", "--all")
		f.Add(git(f, dir, revs, "pack-objects", "--stdout", "-q"))
	}
	base := []byte("base content for a delta")
	d := append(varint(len(base)), varint(5)...)
	d = append(d, 0x90, 5)
	var p rawPack
	off := p.entry(byte(object.TypeBlob), len(base), nil, base)
	p.entry(typeOfsDelta, len(d), ofsEnc(int64(12+p.buf.Len())-off), d)
	f.Add(p.bytes())
	var q rawPack
	baseID := object.HashOf(object.TypeBlob, base)
	q.entry(typeRefDelta, len(d), baseID[:], d)
	f.Add(q.bytes())
	f.Add(rawHeader(0, sha1Of(rawHeader(0, nil))))

	lim := Limits{MaxObject: 1 << 20, MaxTotal: 4 << 20}
	f.Fuzz(func(t *testing.T, data []byte) {
		objs, err := DecodeLimited(bytes.NewReader(data), func(h object.Hash) (object.Object, bool) {
			if h == baseID {
				return object.Object{Type: object.TypeBlob, Data: base}, true
			}
			return object.Object{}, false
		}, lim)
		if err != nil {
			return
		}
		var total int64
		for h, o := range objs {
			total += int64(len(o.Data))
			if int64(len(o.Data)) > lim.MaxObject {
				t.Fatalf("object of %d bytes passed MaxObject", len(o.Data))
			}
			if object.HashOf(o.Type, o.Data) != h {
				t.Fatalf("object %s keyed under the wrong id", h)
			}
		}
		if total > lim.MaxTotal {
			t.Fatalf("decoded %d bytes past MaxTotal", total)
		}
	})
}

func sha1Of(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}
