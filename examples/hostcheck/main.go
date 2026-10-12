// Command hostcheck runs every read call of gitq against one or more
// repositories and prints a pass/fail line per call, to verify a git host works.
//
//	go run ./examples/hostcheck https://gitlab.com/gitlab-org/gitlab-runner
//	GITQ_TOKEN=... go run ./examples/hostcheck https://ghe.example.com/org/repo
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/facets-cloud/go-gitq"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hostcheck <repo-url>...")
		os.Exit(2)
	}
	cache := gitq.NewCache(256<<20, true)
	failed := false
	for _, url := range os.Args[1:] {
		if !check(url, cache) {
			failed = true
		}
		fmt.Println()
	}
	if failed {
		os.Exit(1)
	}
}

func check(url string, cache *gitq.Cache) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fmt.Println(url)
	ok := true
	step := func(name string, fn func() (string, error)) {
		t0 := time.Now()
		res, err := fn()
		d := time.Since(t0).Round(time.Millisecond)
		if err != nil {
			ok = false
			fmt.Printf("  FAIL %-13s %8v  %v\n", name, d, err)
			return
		}
		fmt.Printf("  ok   %-13s %8v  %s\n", name, d, res)
	}

	var opts []gitq.Option
	if tok := os.Getenv("GITQ_TOKEN"); tok != "" {
		opts = append(opts, gitq.Token(tok))
	}
	repo, err := gitq.Open(ctx, url, cache, opts...)
	if err != nil {
		fmt.Printf("  FAIL Open           %v\n", err)
		return false
	}

	var branch, sha string
	step("ListBranches", func() (string, error) {
		bs, err := repo.ListBranches(ctx)
		if err != nil {
			return "", err
		}
		for _, b := range bs {
			if b == "main" || b == "master" {
				branch = b
			}
		}
		if branch == "" && len(bs) > 0 {
			branch = bs[0]
		}
		return fmt.Sprintf("%d branches, using %s", len(bs), branch), nil
	})
	if branch == "" {
		return false
	}
	step("GetSHA", func() (string, error) {
		sha, err = repo.GetSHA(ctx, branch)
		return sha, err
	})
	if sha == "" {
		return false
	}

	var files []gitq.File
	step("ListFiles", func() (string, error) {
		files, err = repo.ListFiles(ctx, sha, "")
		return fmt.Sprintf("%d files", len(files)), err
	})
	if len(files) == 0 {
		return false
	}
	first := files[0].Path
	for _, f := range files {
		if strings.EqualFold(f.Path, "README.md") || strings.EqualFold(f.Path, "README") {
			first = f.Path
		}
	}
	step("GetContent", func() (string, error) {
		b, err := repo.GetContent(ctx, sha, first)
		return fmt.Sprintf("%s, %d bytes", first, len(b)), err
	})
	step("GetContents", func() (string, error) {
		paths := make([]string, 0, 20)
		for _, f := range files[:min(20, len(files))] {
			paths = append(paths, f.Path)
		}
		m, err := repo.GetContents(ctx, sha, paths)
		if err == nil && len(m) != len(paths) {
			err = fmt.Errorf("got %d of %d files", len(m), len(paths))
		}
		return fmt.Sprintf("%d files", len(m)), err
	})
	var hist []gitq.Commit
	step("GetHistory", func() (string, error) {
		hist, err = repo.GetHistory(ctx, sha, "", 5)
		if err != nil || len(hist) == 0 {
			return "", err
		}
		return fmt.Sprintf("%d commits, latest by %s", len(hist), hist[0].Author), nil
	})
	if len(hist) > 1 {
		step("GetChanges", func() (string, error) {
			ch, err := repo.GetChanges(ctx, hist[len(hist)-1].SHA, sha)
			return fmt.Sprintf("%d files over %d commits", len(ch), len(hist)-1), err
		})
	}
	step("Download", func() (string, error) {
		dir := ""
		if i := strings.LastIndex(files[0].Path, "/"); i > 0 {
			dir = files[0].Path[:i]
		}
		var n countWriter
		err := repo.Download(ctx, sha, dir, &n)
		return fmt.Sprintf("dir %q, %d KB tar", dir, n/1024), err
	})
	return ok
}

type countWriter int

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

var _ io.Writer = (*countWriter)(nil)
