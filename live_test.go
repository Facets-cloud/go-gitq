package gitq

import (
	"context"
	"testing"
	"time"
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
