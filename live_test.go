package gitq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/facets-cloud/go-gitq/internal/object"
	"github.com/facets-cloud/go-gitq/internal/wire"
)

func TestLiveGitHub(t *testing.T) {
	if testing.Short() {
		t.Skip("live network test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := Open(ctx, "https://github.com/hashicorp/terraform", NewCache(64<<20, true))
	if err != nil {
		t.Fatal(err)
	}
	readme, err := r.GetContent(ctx, "main", "README.md")
	if err != nil || len(readme) == 0 {
		t.Fatalf("README: %d bytes, %v", len(readme), err)
	}
	files, err := r.ListFiles(ctx, "main", "internal/command")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) <= 100 {
		t.Fatalf("internal/command has %d files", len(files))
	}
	paths := make([]string, 50)
	for i := range paths {
		paths[i] = files[i].Path
	}
	m, err := r.GetContents(ctx, "main", paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 50 {
		t.Fatalf("GetContents returned %d files", len(m))
	}
	start := time.Now()
	h, err := r.GetHistory(ctx, "main", "README.md", 3)
	if err != nil || len(h) == 0 {
		t.Fatalf("GetHistory: %d commits, %v", len(h), err)
	}
	t.Logf("GetHistory(README.md, 3): %d commits in %v, newest %s %q", len(h), time.Since(start), h[0].SHA[:10], h[0].Date)
}

// TestLiveHosts runs every read call, and optionally a write round trip,
// against private repos on each hosting provider, with real credentials:
//
//	GITQ_TEST_<HOST>_URL    repository URL
//	GITQ_TEST_<HOST>_TOKEN  access token (Token option)
//	GITQ_TEST_<HOST>_USER   optional: use BasicAuth(user, token) instead
//	GITQ_TEST_WRITE=1       also Save to a throwaway branch, then delete it
//
// for HOST in GITHUB, GHE, GITLAB, GITLAB_SELF, BITBUCKET. Hosts without a URL are skipped.
func TestLiveHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("live network test")
	}
	for _, host := range []string{"GITHUB", "GHE", "GITLAB", "GITLAB_SELF", "BITBUCKET"} {
		t.Run(host, func(t *testing.T) {
			url := os.Getenv("GITQ_TEST_" + host + "_URL")
			if url == "" {
				t.Skipf("GITQ_TEST_%s_URL not set", host)
			}
			tok := os.Getenv("GITQ_TEST_" + host + "_TOKEN")
			var auth Option = Token(tok)
			if user := os.Getenv("GITQ_TEST_" + host + "_USER"); user != "" {
				auth = BasicAuth(user, tok)
			}
			liveHost(t, url, auth)
		})
	}
}

func liveHost(t *testing.T, url string, auth Option) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := Open(ctx, url, NewCache(1<<20, true)); err == nil {
		t.Logf("%s is public: credentials are sent but not required", url)
	} else if !errors.Is(err, ErrAuth) && !errors.Is(err, ErrNotFound) {
		t.Errorf("Open without credentials: err = %v, want ErrAuth or ErrNotFound", err)
	}
	r, err := Open(ctx, url, NewCache(64<<20, true), auth)
	if err != nil {
		t.Fatal(err)
	}
	branches, err := r.ListBranches(ctx)
	if err != nil || len(branches) == 0 {
		t.Fatalf("ListBranches: %v, %v", branches, err)
	}
	branch := branches[0]
	for _, b := range branches {
		if b == "main" || b == "master" {
			branch = b
		}
	}
	head, err := r.GetSHA(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.ListFiles(ctx, head, "")
	if err != nil || len(files) == 0 {
		t.Fatalf("ListFiles: %d files, %v", len(files), err)
	}
	if _, err := r.GetContent(ctx, head, files[0].Path); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 20)
	for _, f := range files[:min(20, len(files))] {
		paths = append(paths, f.Path)
	}
	if m, err := r.GetContents(ctx, head, paths); err != nil || len(m) != len(paths) {
		t.Fatalf("GetContents: %d of %d, %v", len(m), len(paths), err)
	}
	hist, err := r.GetHistory(ctx, head, "", 5)
	if err != nil || len(hist) == 0 {
		t.Fatalf("GetHistory: %v, %v", hist, err)
	}
	if len(hist) > 1 {
		if _, err := r.GetChanges(ctx, hist[len(hist)-1].SHA, head); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Download(ctx, head, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d branches, %d files at %s, %d commits of history", url, len(branches), len(files), head[:10], len(hist))

	if os.Getenv("GITQ_TEST_WRITE") != "1" {
		return
	}
	scratch := fmt.Sprintf("gitq-live-%d", time.Now().UnixNano())
	author := Author{Name: "gitq live test", Email: "gitq@example.com"}
	sha, err := r.Save(ctx, scratch, "", "gitq live test", author, map[string][]byte{"gitq-live.txt": []byte(scratch)})
	if err != nil {
		t.Fatalf("Save new branch: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := r.r.wc.Push(ctx, []wire.RefUpdate{{Name: "refs/heads/" + scratch, Old: mustHash(t, sha)}}, nil); err != nil {
			t.Errorf("deleting %s: %v", scratch, err)
		}
	})
	if got, err := r.GetContent(ctx, scratch, "gitq-live.txt"); err != nil || string(got) != scratch {
		t.Fatalf("read back: %q, %v", got, err)
	}
	if _, err := r.Save(ctx, scratch, head, "stale", author, map[string][]byte{"x": []byte("x")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Save: err = %v, want ErrConflict", err)
	}
	sha2, err := r.Save(ctx, scratch, sha, "second", author, map[string][]byte{"gitq-live.txt": nil})
	if err != nil {
		t.Fatalf("Save delete: %v", err)
	}
	sha = sha2
}

func mustHash(t *testing.T, s string) object.Hash {
	h, err := object.ParseHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
