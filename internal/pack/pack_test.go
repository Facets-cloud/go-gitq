package pack

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/facets-cloud/go-gitq/internal/object"
)

func git(t testing.TB, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return out
}

func needGit(t testing.TB) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// deltaRepo builds a repo whose history makes git produce long delta chains.
func deltaRepo(t testing.TB, dir string) {
	git(t, dir, nil, "init", "-q", "-b", "main")
	lines := make([]string, 3000)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d of a reasonably large file that deltas well", i)
	}
	for c := 0; c < 40; c++ {
		for k := 0; k < 5; k++ {
			lines[(c*37+k*101)%len(lines)] = fmt.Sprintf("changed in commit %d (%d)", c, k)
		}
		if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a/b", fmt.Sprintf("f%d.txt", c%7)), []byte(strings.Repeat(fmt.Sprintf("x%d\n", c), 200)), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, nil, "add", "-A")
		git(t, dir, nil, "commit", "-q", "-m", fmt.Sprintf("commit %d", c))
	}
	git(t, dir, nil, "repack", "-adfq", "--window=50", "--depth=50")
}

func packFile(t testing.TB, dir string) string {
	m, err := filepath.Glob(filepath.Join(dir, ".git/objects/pack/*.pack"))
	if err != nil || len(m) != 1 {
		t.Fatalf("want one pack, got %v %v", m, err)
	}
	return m[0]
}

// verifyPack returns id->type from `git verify-pack -v` plus the number of deltified entries.
func verifyPack(t testing.TB, dir, pack string) (map[string]string, int) {
	out := git(t, dir, nil, "verify-pack", "-v", strings.TrimSuffix(pack, ".pack")+".idx")
	ids := map[string]string{}
	deltas := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || len(f[0]) != 40 {
			continue
		}
		ids[f[0]] = f[1]
		if len(f) >= 7 {
			deltas++
		}
	}
	return ids, deltas
}

// catAll returns every object in the repo by id.
func catAll(t testing.TB, dir string) map[object.Hash]object.Object {
	out := git(t, dir, nil, "cat-file", "--batch-all-objects", "--batch")
	br := bufio.NewReader(bytes.NewReader(out))
	res := map[object.Hash]object.Object{}
	for {
		hdr, err := br.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(hdr)
		size, _ := strconv.Atoi(f[2])
		data := make([]byte, size+1)
		if _, err := io.ReadFull(br, data); err != nil {
			t.Fatal(err)
		}
		h, _ := object.ParseHash(f[0])
		res[h] = object.Object{Type: typeOf(t, f[1]), Data: data[:size]}
	}
	return res
}

func typeOf(t testing.TB, s string) object.Type {
	switch s {
	case "commit":
		return object.TypeCommit
	case "tree":
		return object.TypeTree
	case "blob":
		return object.TypeBlob
	case "tag":
		return object.TypeTag
	}
	t.Fatalf("bad type %q", s)
	return 0
}

func TestDecodeDeltaPack(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	deltaRepo(t, dir)
	pf := packFile(t, dir)
	want, deltas := verifyPack(t, dir, pf)
	if deltas < 20 {
		t.Fatalf("expected a delta-heavy pack, got %d deltas", deltas)
	}
	raw, err := os.ReadFile(pf)
	if err != nil {
		t.Fatal(err)
	}
	// Use a reader that returns tiny chunks to stress buffer refills.
	got, err := Decode(&chunkReader{r: bytes.NewReader(raw), n: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("decoded %d objects, verify-pack lists %d", len(got), len(want))
	}
	all := catAll(t, dir)
	for id, typ := range want {
		h, _ := object.ParseHash(id)
		o, ok := got[h]
		if !ok {
			t.Errorf("missing %s", id)
			continue
		}
		if o.Type.String() != typ {
			t.Errorf("%s type %s, want %s", id, o.Type, typ)
		}
		if exp := all[h]; exp.Type != o.Type || !bytes.Equal(exp.Data, o.Data) {
			t.Errorf("%s content differs from git cat-file", id)
		}
		if o.Hash() != h {
			t.Errorf("%s rehashes to %s", id, o.Hash())
		}
	}
	t.Logf("pack: %d bytes, %d objects, %d deltas", len(raw), len(got), deltas)

	// Corrupt checksum must be rejected.
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 0xff
	if _, err := Decode(bytes.NewReader(bad), nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("corrupt trailer: err = %v", err)
	}
	// Truncated pack must fail.
	if _, err := Decode(bytes.NewReader(raw[:len(raw)-30]), nil); err == nil {
		t.Error("truncated pack accepted")
	}
}

type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

func TestDecodeThinPack(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	deltaRepo(t, dir)
	thin := git(t, dir, []byte("HEAD\n^HEAD~1\n"), "pack-objects", "--revs", "--thin", "--stdout", "-q")
	all := catAll(t, dir)
	var lookups int
	lookup := func(h object.Hash) (object.Object, bool) {
		lookups++
		o, ok := all[h]
		return o, ok
	}
	got, err := Decode(bytes.NewReader(thin), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if lookups == 0 {
		t.Fatal("thin pack did not need any external base")
	}
	if _, err := Decode(bytes.NewReader(thin), nil); err == nil {
		t.Error("thin pack decoded without lookup")
	}
	revs := git(t, dir, nil, "rev-list", "--objects", "HEAD", "^HEAD~1")
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(revs)), "\n") {
		h, _ := object.ParseHash(strings.Fields(line)[0])
		n++
		o, ok := got[h]
		if !ok {
			t.Errorf("missing %s", h)
			continue
		}
		if !bytes.Equal(o.Data, all[h].Data) {
			t.Errorf("%s content mismatch", h)
		}
	}
	if len(got) != n {
		t.Errorf("decoded %d, rev-list lists %d", len(got), n)
	}
	t.Logf("thin pack: %d bytes, %d objects, %d external lookups", len(thin), len(got), lookups)
}

// rawPack assembles a pack from pre-built entries (header bytes + zlib body).
type rawPack struct {
	buf   bytes.Buffer
	count int
}

func (p *rawPack) entry(typ byte, size int, prefix, body []byte) int64 {
	off := int64(12 + p.buf.Len())
	c := typ<<4 | byte(size&0x0f)
	size >>= 4
	for size != 0 {
		p.buf.WriteByte(c | 0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	p.buf.WriteByte(c)
	p.buf.Write(prefix)
	zw := zlib.NewWriter(&p.buf)
	zw.Write(body)
	zw.Close()
	p.count++
	return off
}

func (p *rawPack) bytes() []byte {
	var out bytes.Buffer
	out.WriteString("PACK")
	binary.Write(&out, binary.BigEndian, uint32(2))
	binary.Write(&out, binary.BigEndian, uint32(p.count))
	out.Write(p.buf.Bytes())
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])
	return out.Bytes()
}

func varint(v int) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}

func ofsEnc(off int64) []byte {
	b := []byte{byte(off & 0x7f)}
	for off >>= 7; off != 0; off >>= 7 {
		off--
		b = append([]byte{byte(0x80 | off&0x7f)}, b...)
	}
	return b
}

// TestDecodeSynthetic covers a REF_DELTA whose base appears later in the pack,
// an OFS_DELTA chained on it, and the 0x10000 implicit copy size.
func TestDecodeSynthetic(t *testing.T) {
	base := bytes.Repeat([]byte("0123456789abcdef"), 0x10000/16+10) // > 64KiB
	baseID := object.HashOf(object.TypeBlob, base)

	// d1 = base[0:0x10000] + "TAIL"   (copy op with no size bytes => 0x10000)
	d1 := append(varint(len(base)), varint(0x10000+4)...)
	d1 = append(d1, 0x80, 4, 'T', 'A', 'I', 'L')
	want1 := append(append([]byte(nil), base[:0x10000]...), "TAIL"...)
	// d2 = "<" + d1[0x10:0x10+0x0102 ] + ">", offset/size given by bitmask bytes
	d2 := append(varint(len(want1)), varint(0x102+2)...)
	d2 = append(d2, 1, '<', 0x80|0x01|0x10|0x20, 0x10, 0x02, 0x01, 1, '>')
	want2 := append(append([]byte("<"), want1[0x10:0x10+0x102]...), '>')

	var p rawPack
	off1 := p.entry(typeRefDelta, len(d1), baseID[:], d1)
	off2 := p.entry(typeOfsDelta, len(d2), ofsEnc(int64(12+p.buf.Len())-off1), d2)
	_ = off2
	p.entry(byte(object.TypeBlob), len(base), nil, base)
	raw := p.bytes()

	got, err := Decode(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range [][]byte{base, want1, want2} {
		if o, ok := got[object.HashOf(object.TypeBlob, w)]; !ok || !bytes.Equal(o.Data, w) {
			t.Errorf("missing/incorrect object of len %d", len(w))
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d objects", len(got))
	}

	// git agrees the pack is valid and yields the same ids.
	needGit(t)
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q")
	pf := filepath.Join(dir, "syn.pack")
	if err := os.WriteFile(pf, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "index-pack", pf)
	ids, _ := verifyPack(t, dir, pf)
	for id := range ids {
		h, _ := object.ParseHash(id)
		if _, ok := got[h]; !ok {
			t.Errorf("git lists %s which Decode lacks", id)
		}
	}
}

func TestEncode(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	deltaRepo(t, dir)
	all := catAll(t, dir)
	objs := make([]object.Object, 0, len(all)+2)
	for _, o := range all {
		objs = append(objs, o)
	}
	objs = append(objs, object.Object{Type: object.TypeBlob, Data: nil},
		object.Object{Type: object.TypeBlob, Data: bytes.Repeat([]byte{0xff}, 100000)})
	var buf bytes.Buffer
	if err := Encode(&buf, objs); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	git(t, out, nil, "init", "-q")
	pf := filepath.Join(out, "enc.pack")
	if err := os.WriteFile(pf, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, out, nil, "index-pack", pf)
	ids, _ := verifyPack(t, out, pf)
	if len(ids) != len(objs) {
		t.Errorf("git sees %d objects, encoded %d", len(ids), len(objs))
	}
	for _, o := range objs {
		if _, ok := ids[o.Hash().String()]; !ok {
			t.Errorf("git index-pack lacks %s", o.Hash())
		}
	}
	// And our own decoder round-trips it.
	got, err := Decode(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(objs) {
		t.Errorf("round trip: %d objects", len(got))
	}
}

var (
	benchOnce sync.Once
	benchPack []byte
)

func BenchmarkDecode(b *testing.B) {
	needGit(b)
	benchOnce.Do(func() {
		dir := b.TempDir()
		deltaRepo(b, dir)
		var err error
		benchPack, err = os.ReadFile(packFile(b, dir))
		if err != nil {
			b.Fatal(err)
		}
	})
	b.SetBytes(int64(len(benchPack)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Decode(bytes.NewReader(benchPack), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDecodeExternalPack decodes an arbitrary pack file named by GITQ_PACK
// (e.g. a large real repository's pack) as a smoke/performance check.
func TestDecodeExternalPack(t *testing.T) {
	path := os.Getenv("GITQ_PACK")
	if path == "" {
		t.Skip("GITQ_PACK not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := Decode(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d objects", path, len(got))
}
