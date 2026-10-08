package gitq

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/facets-cloud/go-gitq/internal/gittest"
)

type fixture struct {
	srv  *gittest.Server
	url  string
	bare string
	work string
	repo *Repo
}

// newFixture builds a repo with nested files, an executable and a symlink, and
// a working clone for further commits.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := gittest.NewServer(t)
	url := srv.NewRepo(t, "proj", map[string]string{
		"README.md":       "hello\n",
		"a/one.txt":       "one\n",
		"a/b/two.txt":     "two\n",
		"a/b/c/three.txt": "three\n",
		"z.txt":           "zed\n",
	})
	f := &fixture{srv: srv, url: url, bare: filepath.Join(srv.Root, "proj.git"), work: t.TempDir()}
	gittest.Git(t, "", "clone", "-q", f.bare, f.work)
	if err := os.WriteFile(filepath.Join(f.work, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/one.txt", filepath.Join(f.work, "link")); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "add script and link")
	repo, err := Open(context.Background(), url, NewCache(64<<20, true))
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo
	return f
}

func (f *fixture) write(t *testing.T, p, content string) {
	t.Helper()
	fp := filepath.Join(f.work, p)
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) commit(t *testing.T, msg string) string {
	t.Helper()
	f.srv.Commit(t, f.work, msg)
	gittest.Git(t, f.work, "push", "-q", "origin", "main")
	return strings.TrimSpace(gittest.Git(t, f.work, "rev-parse", "HEAD"))
}

func (f *fixture) head(t *testing.T) string {
	return strings.TrimSpace(gittest.Git(t, f.bare, "rev-parse", "refs/heads/main"))
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r":          "https://github.com/o/r.git",
		"https://github.com/o/r/":         "https://github.com/o/r.git",
		"https://github.com/o/r.git":      "https://github.com/o/r.git",
		"git@github.com:o/r.git":          "https://github.com/o/r.git",
		"git@github.com:o/r":              "https://github.com/o/r.git",
		"ssh://git@github.com/o/r":        "https://github.com/o/r.git",
		"ssh://git@github.com:22/o/r.git": "https://github.com/o/r.git",
		"http://127.0.0.1:1234/proj.git":  "http://127.0.0.1:1234/proj.git",
	}
	for in, want := range cases {
		got, err := normalizeURL(in)
		if err != nil || got != want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestReads(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	r := f.repo
	head := f.head(t)

	sha, err := r.GetSHA(ctx, "main")
	if err != nil || sha != head {
		t.Fatalf("GetSHA(main) = %q, %v; want %s", sha, err, head)
	}
	if got, err := r.GetSHA(ctx, head); err != nil || got != head {
		t.Fatalf("GetSHA(sha) = %q, %v", got, err)
	}
	if _, err := r.GetSHA(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSHA(nope) err = %v", err)
	}
	gittest.Git(t, f.work, "tag", "-a", "v1", "-m", "v1")
	gittest.Git(t, f.work, "push", "-q", "origin", "v1")
	if got, err := r.GetSHA(ctx, "v1"); err != nil || got != head {
		t.Fatalf("GetSHA(v1) = %q, %v; want peeled %s", got, err, head)
	}

	gittest.Git(t, f.work, "push", "-q", "origin", "main:feature")
	br, err := r.ListBranches(ctx)
	if err != nil || !reflect.DeepEqual(br, []string{"feature", "main"}) {
		t.Fatalf("ListBranches = %v, %v", br, err)
	}

	data, err := r.GetContent(ctx, "main", "a/b/c/three.txt")
	if err != nil || string(data) != "three\n" {
		t.Fatalf("GetContent = %q, %v", data, err)
	}
	if data, err := r.GetContent(ctx, "main", "/link"); err != nil || string(data) != "a/one.txt" {
		t.Fatalf("GetContent(link) = %q, %v", data, err)
	}
	for _, p := range []string{"missing.txt", "a/b", "a/one.txt/x"} {
		if _, err := r.GetContent(ctx, "main", p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetContent(%s) err = %v", p, err)
		}
	}
	if _, err := r.GetContent(ctx, "nobranch", "README.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetContent(bad ref) err = %v", err)
	}

	m, err := r.GetContents(ctx, "main", []string{"README.md", "a/one.txt", "nope", "a", "z.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"README.md": []byte("hello\n"), "a/one.txt": []byte("one\n"), "z.txt": []byte("zed\n")}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("GetContents = %q", m)
	}

	files, err := r.ListFiles(ctx, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	modes := map[string]uint32{}
	for _, fl := range files {
		paths = append(paths, fl.Path)
		modes[fl.Path] = fl.Mode
	}
	wantPaths := []string{"README.md", "a/b/c/three.txt", "a/b/two.txt", "a/one.txt", "link", "run.sh", "z.txt"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("ListFiles paths = %v", paths)
	}
	if modes["run.sh"] != 0o100755 || modes["link"] != 0o120000 || modes["z.txt"] != 0o100644 {
		t.Fatalf("modes = %v", modes)
	}
	blob := strings.TrimSpace(gittest.Git(t, f.bare, "rev-parse", "main:a/one.txt"))
	if files[3].SHA != blob {
		t.Fatalf("SHA %s want %s", files[3].SHA, blob)
	}
	sub, err := r.ListFiles(ctx, "main", "/a/b/")
	if err != nil || len(sub) != 2 || sub[0].Path != "a/b/c/three.txt" || sub[1].Path != "a/b/two.txt" {
		t.Fatalf("ListFiles(a/b) = %v, %v", sub, err)
	}
	if one, err := r.ListFiles(ctx, "main", "z.txt"); err != nil || len(one) != 1 || one[0].Path != "z.txt" {
		t.Fatalf("ListFiles(file) = %v, %v", one, err)
	}
	if _, err := r.ListFiles(ctx, "main", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListFiles(nope) err = %v", err)
	}
}

func TestDownload(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	check := func(dir string, want map[string]string) {
		t.Helper()
		var buf bytes.Buffer
		if err := f.repo.Download(ctx, "main", dir, &buf); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		var order []string
		tr := tar.NewReader(&buf)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			order = append(order, h.Name)
			if h.ModTime.IsZero() || time.Since(h.ModTime) > time.Hour {
				t.Errorf("%s: modtime %v", h.Name, h.ModTime)
			}
			switch h.Typeflag {
			case tar.TypeSymlink:
				got[h.Name] = "->" + h.Linkname
			default:
				b, _ := io.ReadAll(tr)
				got[h.Name] = string(b)
				if h.Name == "run.sh" && h.Mode != 0o755 {
					t.Errorf("run.sh mode %o", h.Mode)
				}
			}
		}
		if !sort.StringsAreSorted(order) {
			t.Errorf("tar not in path order: %v", order)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Download(%q) = %q", dir, got)
		}
	}
	check("", map[string]string{
		"README.md": "hello\n", "a/one.txt": "one\n", "a/b/two.txt": "two\n", "a/b/c/three.txt": "three\n",
		"z.txt": "zed\n", "run.sh": "#!/bin/sh\necho hi\n", "link": "->a/one.txt",
	})
	check("a/b", map[string]string{"two.txt": "two\n", "c/three.txt": "three\n"})
}

func TestHistoryAndChanges(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	base := f.head(t)
	var shas []string // shas[i] after commit i
	f.write(t, "a/one.txt", "one v2\n")
	shas = append(shas, f.commit(t, "edit one"))
	f.write(t, "z.txt", "zed v2\n")
	shas = append(shas, f.commit(t, "edit z"))
	f.write(t, "a/b/two.txt", "two v2\n")
	shas = append(shas, f.commit(t, "edit two\n\nwith body"))
	f.write(t, "a/one.txt", "one v3\n")
	shas = append(shas, f.commit(t, "edit one again"))

	// Small depth forces several incremental history fetches.
	r, err := Open(ctx, f.url, NewCache(64<<20, true), HistoryDepth(2))
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.GetHistory(ctx, "main", "a/one.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	init := strings.TrimSpace(gittest.Git(t, f.bare, "rev-list", "--max-parents=0", "main"))
	if got := shaList(h); !reflect.DeepEqual(got, []string{shas[3], shas[0], init}) {
		t.Fatalf("history(a/one.txt) = %v", got)
	}
	if h[0].Author != "Test" || h[0].Email != "test@example.com" || h[0].Message != "edit one again" || h[0].Date.IsZero() {
		t.Fatalf("commit fields = %+v", h[0])
	}
	h, err = r.GetHistory(ctx, "main", "a", 2)
	if err != nil || !reflect.DeepEqual(shaList(h), []string{shas[3], shas[2]}) {
		t.Fatalf("history(a, 2) = %v, %v", shaList(h), err)
	}
	if h[1].Message != "edit two\n\nwith body" {
		t.Fatalf("message = %q", h[1].Message)
	}
	h, err = r.GetHistory(ctx, "main", "run.sh", 0)
	if err != nil || !reflect.DeepEqual(shaList(h), []string{base}) {
		t.Fatalf("history(run.sh) = %v, %v", shaList(h), err)
	}
	h, err = r.GetHistory(ctx, "main", "never-existed", 5)
	if err != nil || len(h) != 0 {
		t.Fatalf("history(missing) = %v, %v", h, err)
	}

	ch, err := r.GetChanges(ctx, base, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{"M", "a/b/two.txt"}, {"M", "a/one.txt"}, {"M", "z.txt"}}
	if !reflect.DeepEqual(ch, want) {
		t.Fatalf("GetChanges = %v", ch)
	}
	// Add, delete, mode change.
	gittest.Git(t, f.work, "rm", "-q", "z.txt")
	f.write(t, "new/dir/file.txt", "x")
	if err := os.Chmod(filepath.Join(f.work, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "mixed")
	ch, err = r.GetChanges(ctx, shas[3], "main")
	if err != nil {
		t.Fatal(err)
	}
	want = []Change{{"M", "README.md"}, {"A", "new/dir/file.txt"}, {"D", "z.txt"}}
	if !reflect.DeepEqual(ch, want) {
		t.Fatalf("GetChanges = %v", ch)
	}
}

func shaList(cs []Commit) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.SHA)
	}
	return out
}

func TestSave(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	r := f.repo
	head := f.head(t)
	author := Author{Name: "Saver", Email: "saver@example.com"}

	sha, err := r.Save(ctx, "main", head, "update files", author, map[string][]byte{
		"README.md":        []byte("hello v2\n"),
		"a/new/deep/n.txt": []byte("new\n"),
		"z.txt":            nil,
		"a/b/c/three.txt":  nil, // leaves a/b/c empty -> removed
		"does/not/exist":   nil,
		"run.sh":           []byte("#!/bin/sh\necho bye\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.head(t); got != sha {
		t.Fatalf("bare main = %s, Save returned %s", got, sha)
	}
	gittest.Git(t, f.bare, "fsck", "--strict")
	tree := gittest.Git(t, f.bare, "ls-tree", "-r", "--name-only", "main")
	wantTree := "README.md\na/b/two.txt\na/new/deep/n.txt\na/one.txt\nlink\nrun.sh\n"
	if tree != wantTree {
		t.Fatalf("ls-tree = %q", tree)
	}
	if got := gittest.Git(t, f.bare, "cat-file", "-p", "main:README.md"); got != "hello v2\n" {
		t.Fatalf("README = %q", got)
	}
	if got := gittest.Git(t, f.bare, "ls-tree", "main", "run.sh"); !strings.HasPrefix(got, "100755") {
		t.Fatalf("run.sh lost exec mode: %q", got)
	}
	if got := gittest.Git(t, f.bare, "log", "-1", "--format=%an <%ae>|%s|%P", "main"); got != "Saver <saver@example.com>|update files|"+head+"\n" {
		t.Fatalf("log = %q", got)
	}
	// The pushed history is what a normal clone sees.
	clone := t.TempDir()
	gittest.Git(t, "", "clone", "-q", f.url, clone)
	if b, err := os.ReadFile(filepath.Join(clone, "a/new/deep/n.txt")); err != nil || string(b) != "new\n" {
		t.Fatalf("clone n.txt = %q, %v", b, err)
	}

	if data, err := r.GetContent(ctx, "main", "README.md"); err != nil || string(data) != "hello v2\n" {
		t.Fatalf("GetContent after save = %q, %v", data, err)
	}
	if _, err := r.GetContent(ctx, sha, "z.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted file err = %v", err)
	}

	// Stale expected SHA.
	if _, err := r.Save(ctx, "main", head, "stale", author, map[string][]byte{"x": []byte("x")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save err = %v", err)
	}
	if f.head(t) != sha {
		t.Fatal("stale save moved the branch")
	}

	// New branch with no parent.
	nb, err := r.Save(ctx, "fresh", "", "root commit", author, map[string][]byte{"only.txt": []byte("only\n")})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetSHA(ctx, "fresh"); err != nil || got != nb {
		t.Fatalf("GetSHA(fresh) = %s, %v", got, err)
	}
	if got := gittest.Git(t, f.bare, "ls-tree", "-r", "--name-only", "fresh"); got != "only.txt\n" {
		t.Fatalf("fresh tree = %q", got)
	}
	// Creating an existing branch with "" conflicts.
	if _, err := r.Save(ctx, "fresh", "", "again", author, map[string][]byte{"a": []byte("a")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("recreate branch err = %v", err)
	}
	// Chained save on top of our own commit uses the cache.
	if _, err := r.Save(ctx, "fresh", nb, "second", author, map[string][]byte{"two.txt": []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if got := gittest.Git(t, f.bare, "ls-tree", "-r", "--name-only", "fresh"); got != "only.txt\ntwo.txt\n" {
		t.Fatalf("fresh tree = %q", got)
	}
}

func TestSaveEmptyRepo(t *testing.T) {
	ctx := context.Background()
	srv := gittest.NewServer(t)
	url := srv.NewRepo(t, "empty", nil)
	r, err := Open(ctx, url, NewCache(64<<20, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSHA(ctx, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSHA on empty repo err = %v", err)
	}
	sha, err := r.Save(ctx, "main", "", "first", Author{Name: "A", Email: "a@x"}, map[string][]byte{"d/f.txt": []byte("f")})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := r.GetContent(ctx, sha, "d/f.txt"); err != nil || string(data) != "f" {
		t.Fatalf("GetContent = %q, %v", data, err)
	}
}

func TestOpenNotFound(t *testing.T) {
	srv := gittest.NewServer(t)
	_, err := Open(context.Background(), srv.URL+"/nope.git", NewCache(0, true))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) err = %v", err)
	}
}
