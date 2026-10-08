package wire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/facets-cloud/go-gitq/internal/gittest"
	"github.com/facets-cloud/go-gitq/internal/object"
)

func mustHash(t *testing.T, s string) object.Hash {
	t.Helper()
	h, err := parseHex(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func packCount(t *testing.T, p []byte) int {
	t.Helper()
	if len(p) < 32 || string(p[:4]) != "PACK" {
		t.Fatalf("not a pack: %q", p[:min(len(p), 16)])
	}
	return int(binary.BigEndian.Uint32(p[8:12]))
}

// packTypes indexes the pack with git and returns object counts by type.
func packTypes(t *testing.T, p []byte) map[string]int {
	t.Helper()
	dir := t.TempDir()
	gittest.Git(t, "", "init", "-q", "--bare", dir)
	cmd := exec.Command("git", "index-pack", "--stdin", "--fix-thin")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("index-pack: %v\n%s", err, out)
	}
	idx, _ := filepath.Glob(filepath.Join(dir, "objects/pack/*.idx"))
	out := gittest.Git(t, "", "verify-pack", "-v", idx[0])
	m := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && len(f[0]) == 40 {
			m[f[1]]++
		}
	}
	return m
}

var testFiles = map[string]string{
	"README.md":     "hello\n",
	"a/b/c.txt":     "c\n",
	"a/d.txt":       "d\n",
	"x/y/z/deep.go": "package z\n",
}

func setup(t *testing.T) (*gittest.Server, *Client, string) {
	s := gittest.NewServer(t)
	url := s.NewRepo(t, "repo", testFiles)
	return s, NewClient(url, nil, nil), filepath.Join(s.Root, "repo.git")
}

func TestCapabilities(t *testing.T) {
	_, c, _ := setup(t)
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Version2 || !caps.HasFetchFeature("filter") || !caps.HasFetchFeature("shallow") {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestNotFound(t *testing.T) {
	s, _, _ := setup(t)
	c := NewClient(s.URL+"/nope.git", nil, nil)
	if _, err := c.Capabilities(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLsRefs(t *testing.T) {
	_, c, bare := setup(t)
	tip := mustHash(t, gittest.Git(t, bare, "rev-parse", "main"))
	gittest.Git(t, bare, "update-ref", "refs/heads/other", hexOf(tip))
	gittest.Git(t, bare, "-c", "user.name=T", "-c", "user.email=t@e", "tag", "-a", "-m", "v", "v1", "main")
	ctx := context.Background()

	refs, err := c.LsRefs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Ref{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	if r := byName["HEAD"]; r.Symref != "refs/heads/main" || r.Hash != tip {
		t.Fatalf("HEAD = %+v", r)
	}
	if r := byName["refs/tags/v1"]; r.Peeled != tip || r.Hash == tip {
		t.Fatalf("tag = %+v", r)
	}
	if len(refs) != 4 {
		t.Fatalf("refs = %+v", refs)
	}

	refs, err = c.LsRefs(ctx, []string{"refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" || refs[0].Hash != tip {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestFetchShallowBlobless(t *testing.T) {
	_, c, bare := setup(t)
	tip := mustHash(t, gittest.Git(t, bare, "rev-parse", "main"))
	p, err := c.Fetch(context.Background(), FetchRequest{Wants: []object.Hash{tip}, Depth: 1, Filter: "blob:none"})
	if err != nil {
		t.Fatal(err)
	}
	// trees: root, a, a/b, x, x/y, x/y/z
	if n := packCount(t, p); n != 7 {
		t.Fatalf("count = %d", n)
	}
	if m := packTypes(t, p); m["commit"] != 1 || m["tree"] != 6 || m["blob"] != 0 {
		t.Fatalf("types = %v", m)
	}
}

func TestFetchBlobs(t *testing.T) {
	_, c, bare := setup(t)
	b1 := mustHash(t, gittest.Git(t, bare, "rev-parse", "main:README.md"))
	b2 := mustHash(t, gittest.Git(t, bare, "rev-parse", "main:a/d.txt"))
	p, err := c.Fetch(context.Background(), FetchRequest{Wants: []object.Hash{b1, b2}})
	if err != nil {
		t.Fatal(err)
	}
	if m := packTypes(t, p); packCount(t, p) != 2 || m["blob"] != 2 {
		t.Fatalf("types = %v", m)
	}
}

func TestFetchMissingObject(t *testing.T) {
	_, c, _ := setup(t)
	var bogus object.Hash
	bogus[0] = 0xab
	if _, err := c.Fetch(context.Background(), FetchRequest{Wants: []object.Hash{bogus}}); err == nil {
		t.Fatal("expected error")
	}
}

// makeCommit builds a commit on top of parent (in a scratch repo that fetched
// the bare repo) changing one file, and returns it with a pack of the new objects.
func makeCommit(t *testing.T, bare string, parent object.Hash, content string) (object.Hash, []byte) {
	t.Helper()
	work := t.TempDir()
	gittest.Git(t, "", "init", "-q", "--bare", work)
	gittest.Git(t, work, "fetch", "-q", bare, "main")
	blob := runIn(t, work, content, "hash-object", "-w", "--stdin")
	tree := gittest.Git(t, work, "ls-tree", hexOf(parent))
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(tree), "\n") {
		if !strings.HasSuffix(l, "\tREADME.md") {
			lines = append(lines, l)
		}
	}
	lines = append(lines, "100644 blob "+strings.TrimSpace(blob)+"\tREADME.md")
	newTree := runIn(t, work, strings.Join(lines, "\n")+"\n", "mktree")
	commit := runIn(t, work, "", "-c", "user.name=T", "-c", "user.email=t@e",
		"commit-tree", strings.TrimSpace(newTree), "-p", hexOf(parent), "-m", "update "+content)
	ch := mustHash(t, commit)
	p := runIn(t, work, hexOf(ch)+"\n^"+hexOf(parent)+"\n", "pack-objects", "--stdout", "--revs")
	return ch, []byte(p)
}

func runIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

func TestPush(t *testing.T) {
	_, c, bare := setup(t)
	ctx := context.Background()
	tip := mustHash(t, gittest.Git(t, bare, "rev-parse", "main"))
	newC, p := makeCommit(t, bare, tip, "updated\n")

	// Stale old value is rejected before anything is sent.
	var wrong object.Hash
	wrong[0] = 1
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/main", Old: wrong, New: newC}}, p); !errors.Is(err, ErrStale) {
		t.Fatalf("stale push err = %v", err)
	}
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/main", Old: tip, New: newC}}, p); err != nil {
		t.Fatal(err)
	}
	if got := mustHash(t, gittest.Git(t, bare, "rev-parse", "main")); got != newC {
		t.Fatalf("main = %x", got)
	}
	// Repeating the same update now is stale.
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/main", Old: tip, New: newC}}, p); !errors.Is(err, ErrStale) {
		t.Fatalf("repeat push err = %v", err)
	}

	// Create a branch at an existing commit (nil pack -> empty pack sent).
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/feature", New: tip}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustHash(t, gittest.Git(t, bare, "rev-parse", "refs/heads/feature")); got != tip {
		t.Fatalf("feature = %x", got)
	}
	// Creating it again (Old zero) is stale.
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/feature", New: newC}}, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("recreate err = %v", err)
	}
	// Delete.
	if err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/feature", Old: tip}}, nil); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "-q", "refs/heads/feature").Output(); err == nil {
		t.Fatalf("feature still exists: %s", out)
	}
}

func TestPushRejectedByHook(t *testing.T) {
	_, c, bare := setup(t)
	ctx := context.Background()
	tip := mustHash(t, gittest.Git(t, bare, "rev-parse", "main"))
	newC, p := makeCommit(t, bare, tip, "hooked\n")
	hook := filepath.Join(bare, "hooks", "update")
	if err := writeExec(hook, "#!/bin/sh\necho protected >&2\nexit 1\n"); err != nil {
		t.Fatal(err)
	}
	err := c.Push(ctx, []RefUpdate{{Name: "refs/heads/main", Old: tip, New: newC}}, p)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Ref != "refs/heads/main" {
		t.Fatalf("err = %v", err)
	}
}

// Live tests against GitHub.

const liveURL = "https://github.com/hashicorp/terraform.git"

func liveClient(t *testing.T) *Client {
	if testing.Short() {
		t.Skip("live test")
	}
	return NewClient(liveURL, &http.Client{Timeout: 2 * time.Minute}, nil)
}

func TestLive(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Version2 || !caps.HasFetchFeature("filter") {
		t.Fatalf("caps = %+v", caps)
	}
	refs, err := c.LsRefs(ctx, []string{"refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs = %+v", refs)
	}
	start := time.Now()
	p, err := c.Fetch(ctx, FetchRequest{Wants: []object.Hash{refs[0].Hash}, Depth: 1, Filter: "blob:none"})
	if err != nil {
		t.Fatal(err)
	}
	n := packCount(t, p)
	t.Logf("tree pack: %d bytes, %d objects, %v", len(p), n, time.Since(start))
	if n < 500 || len(p) < 50<<10 {
		t.Fatalf("suspiciously small pack: %d objects %d bytes", n, len(p))
	}
	blob := mustHash(t, "0ed4a676b3af60c854a7019e223a57c739d21a10")
	p, err = c.Fetch(ctx, FetchRequest{Wants: []object.Hash{blob}})
	if err != nil {
		t.Fatal(err)
	}
	if n := packCount(t, p); n != 1 {
		t.Fatalf("blob pack count = %d", n)
	}
}

func writeExec(path, content string) error { return os.WriteFile(path, []byte(content), 0o755) }
