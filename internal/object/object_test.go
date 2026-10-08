package object

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func git(t testing.TB, dir string, stdin []byte, env []string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Env = append(cmd.Env, env...)
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

func TestTypeAndHash(t *testing.T) {
	for typ, s := range map[Type]string{TypeCommit: "commit", TypeTree: "tree", TypeBlob: "blob", TypeTag: "tag"} {
		if typ.String() != s {
			t.Errorf("%d.String() = %q", typ, typ.String())
		}
	}
	// Well-known empty blob / tree ids.
	if got := HashOf(TypeBlob, nil).String(); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("empty blob = %s", got)
	}
	if got := HashOf(TypeTree, nil).String(); got != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("empty tree = %s", got)
	}
	h, err := ParseHash("E69DE29BB2D1D6434B8B29AE775AD8C2E48C5391")
	if err != nil || h.String() != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("ParseHash upper: %v %v", h, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 41)} {
		if _, err := ParseHash(bad); err == nil {
			t.Errorf("ParseHash(%q) accepted", bad)
		}
	}
	if !ZeroHash.IsZero() || h.IsZero() {
		t.Error("IsZero")
	}
}

func TestTreeSortOrder(t *testing.T) {
	var h Hash
	in := []TreeEntry{
		{Name: "foo.c", Mode: ModeFile, Hash: h},
		{Name: "foo", Mode: ModeDir, Hash: h},
		{Name: "foo-bar", Mode: ModeFile, Hash: h},
		{Name: "a", Mode: ModeExec, Hash: h},
	}
	got, err := ParseTree(EncodeTree(in))
	if err != nil {
		t.Fatal(err)
	}
	// "foo-" (0x2d) < "foo." (0x2e) < "foo/" (0x2f)
	want := []string{"a", "foo-bar", "foo.c", "foo"}
	for i, e := range got {
		if e.Name != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !bytes.Contains(EncodeTree(in), []byte("40000 foo\x00")) {
		t.Error("dir mode not written as 40000")
	}
}

func TestCommitRoundTripAndSkipHeaders(t *testing.T) {
	when := time.Unix(1700000000, 0).In(time.FixedZone("", -(3*3600 + 30*60)))
	c := &Commit{
		Tree:      HashOf(TypeTree, nil),
		Parents:   []Hash{HashOf(TypeBlob, []byte("a")), HashOf(TypeBlob, []byte("b"))},
		Author:    Signature{Name: "A U Thor", Email: "a@x.org", When: when},
		Committer: Signature{Name: "C O Mitter", Email: "c@x.org", When: when.In(time.FixedZone("", 5*3600+45*60))},
		Message:   "subject\n\nbody\n\n",
	}
	raw := EncodeCommit(c)
	if !bytes.Contains(raw, []byte("a@x.org> 1700000000 -0330\n")) || !bytes.Contains(raw, []byte(" +0545\n")) {
		t.Fatalf("bad encoding:\n%s", raw)
	}
	got, err := ParseCommit(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(EncodeCommit(got), raw) {
		t.Fatalf("round trip mismatch")
	}
	if _, off := got.Author.When.Zone(); off != -(3*3600 + 30*60) {
		t.Errorf("author offset %d", off)
	}

	// gpgsig / encoding headers with continuation lines are skipped.
	signed := strings.Replace(string(raw), "\n\n",
		"\nencoding UTF-8\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEz\n -----END PGP SIGNATURE-----\n\n", 1)
	got2, err := ParseCommit([]byte(signed))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(EncodeCommit(got2), raw) {
		t.Fatalf("signed commit parse mismatch:\n%s", EncodeCommit(got2))
	}
}

// TestAgainstGit cross-checks against objects written by the git binary.
func TestAgainstGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, nil, nil, "init", "-q", "-b", "main")
	write := func(p, c string, mode os.FileMode) {
		fp := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fp, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "hello\n", 0o644)
	write("dir/b.txt", "b\n", 0o644)
	write("dir/sub/c.txt", "c\n", 0o644)
	write("dir.txt", "sorts before dir/\n", 0o644)
	write("dir-x", "sorts before dir.txt\n", 0o644)
	write("run.sh", "#!/bin/sh\necho hi\n", 0o755)
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	commit := func(msg, adate, cdate string) {
		git(t, dir, nil, nil, "add", "-A")
		git(t, dir, nil, []string{"GIT_AUTHOR_DATE=" + adate, "GIT_COMMITTER_DATE=" + cdate},
			"-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "-q", "-m", msg)
	}
	commit("initial\n\nwith body", "1700000000 +0530", "1700000100 -0700")
	write("dir/sub/c.txt", "c2\n", 0o644)
	commit("second", "1700001000 +0000", "1700002000 +1245")

	list := git(t, dir, nil, nil, "rev-list", "--objects", "--all")
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(list)), "\n") {
		ids = append(ids, strings.Fields(line)[0])
	}
	batch := git(t, dir, []byte(strings.Join(ids, "\n")+"\n"), nil, "cat-file", "--batch")
	br := bufio.NewReader(bytes.NewReader(batch))
	var sawTree, sawCommit, sawExec, sawLink, sawDir int
	for range ids {
		hdr, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(hdr)
		size, _ := strconv.Atoi(f[2])
		data := make([]byte, size+1)
		if _, err := io.ReadFull(br, data); err != nil {
			t.Fatal(err)
		}
		data = data[:size]
		want, err := ParseHash(f[0])
		if err != nil {
			t.Fatal(err)
		}
		var typ Type
		switch f[1] {
		case "commit":
			typ = TypeCommit
		case "tree":
			typ = TypeTree
		case "blob":
			typ = TypeBlob
		default:
			t.Fatalf("unexpected type %s", f[1])
		}
		if got := HashOf(typ, data); got != want {
			t.Errorf("HashOf(%s) = %s, want %s", f[1], got, want)
		}
		switch typ {
		case TypeTree:
			sawTree++
			ents, err := ParseTree(data)
			if err != nil {
				t.Fatal(err)
			}
			// Feed shuffled-ish (reversed) input to exercise sorting.
			rev := make([]TreeEntry, len(ents))
			for i, e := range ents {
				rev[len(ents)-1-i] = e
				switch e.Mode {
				case ModeExec:
					sawExec++
				case ModeSymlink:
					sawLink++
				case ModeDir:
					sawDir++
				}
			}
			if enc := EncodeTree(rev); !bytes.Equal(enc, data) {
				t.Errorf("EncodeTree mismatch for %s:\n%q\n%q", want, enc, data)
			}
		case TypeCommit:
			sawCommit++
			c, err := ParseCommit(data)
			if err != nil {
				t.Fatal(err)
			}
			if enc := EncodeCommit(c); !bytes.Equal(enc, data) {
				t.Errorf("EncodeCommit mismatch:\n%s\n---\n%s", enc, data)
			}
			if c.Author.Name != "Test User" || c.Author.Email != "test@example.com" {
				t.Errorf("author = %+v", c.Author)
			}
		}
	}
	if sawTree < 4 || sawCommit != 2 || sawExec == 0 || sawLink == 0 || sawDir == 0 {
		t.Errorf("coverage: trees=%d commits=%d exec=%d link=%d dir=%d", sawTree, sawCommit, sawExec, sawLink, sawDir)
	}

	// Specific field checks on HEAD.
	head := git(t, dir, nil, nil, "cat-file", "commit", "HEAD")
	c, err := ParseCommit(head)
	if err != nil {
		t.Fatal(err)
	}
	parent := strings.TrimSpace(string(git(t, dir, nil, nil, "rev-parse", "HEAD~1")))
	tree := strings.TrimSpace(string(git(t, dir, nil, nil, "rev-parse", "HEAD^{tree}")))
	if len(c.Parents) != 1 || c.Parents[0].String() != parent || c.Tree.String() != tree {
		t.Errorf("HEAD parents/tree = %v %v", c.Parents, c.Tree)
	}
	if _, off := c.Committer.When.Zone(); off != 12*3600+45*60 || c.Committer.When.Unix() != 1700002000 {
		t.Errorf("committer when = %v", c.Committer.When)
	}
	if c.Message != "second\n" {
		t.Errorf("message = %q", c.Message)
	}
}
