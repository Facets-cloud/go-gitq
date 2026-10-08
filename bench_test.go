package gitq_test

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/facets-cloud/go-gitq"
)

// Live benchmarks against github.com/hashicorp/terraform at a pinned commit.
// They hit the network, so they only run with GITQ_LIVE=1:
//
//	GITQ_LIVE=1 go test -run '^$' -bench . -benchtime 5x
const (
	benchURL  = "https://github.com/hashicorp/terraform"
	benchSHA  = "cc82a31bf56b146e7dec70a1f254243ac88c6376"
	benchFile = "internal/command/apply.go"
	benchDir  = "internal/command"
)

func liveRepo(b *testing.B) *gitq.Repo {
	b.Helper()
	if os.Getenv("GITQ_LIVE") == "" {
		b.Skip("set GITQ_LIVE=1 to run live benchmarks")
	}
	r, err := gitq.Open(context.Background(), benchURL, gitq.NewCache(256<<20, true))
	if err != nil {
		b.Fatal(err)
	}
	return r
}

// Cold: a fresh Repo per iteration, so every call goes to the network.
func BenchmarkColdGetContent(b *testing.B) {
	liveRepo(b)
	ctx := context.Background()
	for b.Loop() {
		r, err := gitq.Open(ctx, benchURL, gitq.NewCache(256<<20, true)) // fresh cache: cold
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.GetContent(ctx, benchSHA, benchFile); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColdListFiles(b *testing.B) {
	liveRepo(b)
	ctx := context.Background()
	for b.Loop() {
		r, err := gitq.Open(ctx, benchURL, gitq.NewCache(256<<20, true)) // fresh cache: cold
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.ListFiles(ctx, benchSHA, benchDir); err != nil {
			b.Fatal(err)
		}
	}
}

// Warm: one Repo, cache primed before timing.
func BenchmarkWarmGetContent(b *testing.B) {
	r := liveRepo(b)
	ctx := context.Background()
	if _, err := r.GetContent(ctx, benchSHA, benchFile); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := r.GetContent(ctx, benchSHA, benchFile); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWarmListFiles(b *testing.B) {
	r := liveRepo(b)
	ctx := context.Background()
	if _, err := r.ListFiles(ctx, benchSHA, benchDir); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := r.ListFiles(ctx, benchSHA, benchDir); err != nil {
			b.Fatal(err)
		}
	}
}

// Batch: 50 uncached files in one call, fresh Repo each time (trees primed outside timing).
func BenchmarkColdGetContents50(b *testing.B) {
	liveRepo(b)
	ctx := context.Background()
	for b.Loop() {
		b.StopTimer()
		r, err := gitq.Open(ctx, benchURL, gitq.NewCache(256<<20, true)) // fresh cache: cold
		if err != nil {
			b.Fatal(err)
		}
		files, err := r.ListFiles(ctx, benchSHA, benchDir)
		if err != nil {
			b.Fatal(err)
		}
		paths := make([]string, 0, 50)
		for _, f := range files[:min(50, len(files))] {
			paths = append(paths, f.Path)
		}
		b.StartTimer()
		if _, err := r.GetContents(ctx, benchSHA, paths); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWarmDownload(b *testing.B) {
	r := liveRepo(b)
	ctx := context.Background()
	if err := r.Download(ctx, benchSHA, benchDir, io.Discard); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := r.Download(ctx, benchSHA, benchDir, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
