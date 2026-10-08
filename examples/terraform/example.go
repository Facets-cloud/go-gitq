// Command example reads the hashicorp/terraform repo with gitq and times each
// call, cold and warm. With -compare it also times the git CLI doing the same
// work from a fresh clone.
//
//	go run ./examples/terraform
//	go run ./examples/terraform -compare
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/facets-cloud/go-gitq"
)

func main() {
	url := flag.String("repo", "https://github.com/hashicorp/terraform", "repository URL")
	ref := flag.String("ref", "main", "branch, tag or SHA")
	dir := flag.String("dir", "internal/command", "directory to list and read")
	file := flag.String("file", "internal/command/apply.go", "file to read and get history for")
	compare := flag.Bool("compare", false, "also time the git CLI from a fresh clone")
	flag.Parse()

	ctx := context.Background()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "step\tcold\twarm\tresult")

	timed := func(name string, fn func() string) {
		t0 := time.Now()
		res := fn()
		cold := time.Since(t0)
		t1 := time.Now()
		fn()
		warm := time.Since(t1)
		fmt.Fprintf(tw, "%s\t%v\t%v\t%s\n", name, cold.Round(time.Millisecond), warm.Round(time.Microsecond), res)
	}

	t0 := time.Now()
	cache := gitq.NewCache(256<<20, true) // one per process, shared by every repo
	repo, err := gitq.Open(ctx, *url, cache)
	must(err)
	fmt.Fprintf(tw, "Open\t%v\t-\tcapabilities ok\n", time.Since(t0).Round(time.Millisecond))

	var sha string
	timed("GetSHA", func() string {
		sha, err = repo.GetSHA(ctx, *ref)
		must(err)
		return *ref + " = " + sha[:12]
	})

	// Everything below reads at the pinned SHA so warm runs hit the cache.
	timed("GetContent", func() string {
		b, err := repo.GetContent(ctx, sha, *file)
		must(err)
		return fmt.Sprintf("%s: %d bytes", *file, len(b))
	})

	var files []gitq.File
	timed("ListFiles", func() string {
		files, err = repo.ListFiles(ctx, sha, *dir)
		must(err)
		return fmt.Sprintf("%s: %d files", *dir, len(files))
	})

	paths := make([]string, 0, 50)
	for _, f := range files {
		if len(paths) == 50 {
			break
		}
		if f.Path != *file {
			paths = append(paths, f.Path)
		}
	}
	timed("GetContents(50)", func() string {
		m, err := repo.GetContents(ctx, sha, paths)
		must(err)
		n := 0
		for _, b := range m {
			n += len(b)
		}
		return fmt.Sprintf("%d files, %d KB", len(m), n/1024)
	})

	timed("GetHistory(10)", func() string {
		cs, err := repo.GetHistory(ctx, sha, *file, 10)
		must(err)
		if len(cs) == 0 {
			return "0 commits"
		}
		return fmt.Sprintf("%d commits, latest %s %q", len(cs), cs[0].SHA[:12], firstLine(cs[0].Message))
	})

	timed("GetChanges(~10)", func() string {
		cs, err := repo.GetHistory(ctx, sha, "", 11)
		must(err)
		from := cs[len(cs)-1].SHA
		ch, err := repo.GetChanges(ctx, from, sha)
		must(err)
		return fmt.Sprintf("%d files changed over last %d commits", len(ch), len(cs)-1)
	})

	timed("Download", func() string {
		cw := &countWriter{}
		must(repo.Download(ctx, sha, *dir, cw))
		return fmt.Sprintf("%s as tar: %d KB", *dir, cw.n/1024)
	})
	tw.Flush()

	st := cache.Stats()
	fmt.Printf("\ncache: %d files in %d KB (%d KB uncompressed)\n", st.Files, st.StoredBytes>>10, st.FileBytes>>10)

	if *compare {
		fmt.Println()
		compareGit(*url, sha, *dir, *file)
	}
}

// compareGit times the git CLI answering the same questions from a fresh clone.
func compareGit(url, sha, dir, file string) {
	tmp, err := os.MkdirTemp("", "gitq-compare-")
	must(err)
	defer os.RemoveAll(tmp)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "git CLI\ttime\tnote")
	step := func(name, note string, args ...string) {
		t0 := time.Now()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Fatalf("git %v: %v\n%s", args, err, out)
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\n", name, time.Since(t0).Round(time.Millisecond), note)
	}
	repoDir := filepath.Join(tmp, "r")
	step("clone --depth 1 (checkout)", "what a checkout-based reader pays first", "clone", "-q", "--depth", "1", url, repoDir)
	step("clone --filter=blob:none --depth 50 --no-checkout", "closest git equivalent of gitq's index", "clone", "-q", "--filter=blob:none", "--depth", "50", "--no-checkout", url, filepath.Join(tmp, "b"))
	step("show sha:file (blobless, lazy fetch)", "one file from the blobless clone", "-C", filepath.Join(tmp, "b"), "show", sha+":"+file)
	step("log -n 10 -- file (blobless)", "history from the blobless clone", "-C", filepath.Join(tmp, "b"), "log", "-n", "10", "--format=%H", sha, "--", file)
	tw.Flush()
}

type countWriter struct{ n int }

func (c *countWriter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 50 {
		s = s[:50] + "..."
	}
	return s
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
