package gitq

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/facets-cloud/go-gitq/internal/gittest"
)

// histRepo builds a history with merges that first-parent walking gets wrong:
//
//	initial -> main edit -> merge side (f.json changed only on side)
//	        -> f edit on main -> evil merge (f.json differs from both parents)
//	        -> merge -s ours (side3's f.json change discarded)
//
// Each commit gets a distinct, increasing date so ordering is deterministic.
func histRepo(t *testing.T) (url, bare string) {
	t.Helper()
	srv := gittest.NewServer(t)
	url = srv.NewRepo(t, "hist", nil)
	bare = filepath.Join(srv.Root, "hist.git")
	work := t.TempDir()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	git := func(args ...string) string {
		clock = clock.Add(time.Minute)
		d := clock.Format(time.RFC3339)
		t.Setenv("GIT_AUTHOR_DATE", d)
		t.Setenv("GIT_COMMITTER_DATE", d)
		return gittest.Git(t, work, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	}
	write := func(p, c string) {
		fp := filepath.Join(work, p)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) { git("add", "-A"); git("commit", "-q", "-m", msg) }

	git("init", "-q", "-b", "main")
	write("f.json", "1\n")
	write("a/x", "x1\n")
	write("other.txt", "o1\n")
	commit("initial")

	git("checkout", "-q", "-b", "side")
	write("f.json", "side\n")
	write("a/x", "x-side\n")
	commit("side edit")
	git("checkout", "-q", "main")
	write("other.txt", "o2\n")
	commit("main edit")
	git("merge", "-q", "--no-ff", "-m", "merge side", "side")

	write("f.json", "main again\n")
	commit("f edit on main")

	git("checkout", "-q", "-b", "side2")
	write("f.json", "side2\n")
	commit("side2 edit")
	git("checkout", "-q", "main")
	write("f.json", "main conflicting\n")
	commit("main conflicting edit")
	git("merge", "-q", "--no-ff", "--no-commit", "-s", "ours", "side2")
	write("f.json", "resolved\n")
	git("add", "-A")
	git("commit", "-q", "-m", "evil merge")

	git("checkout", "-q", "-b", "side3")
	write("f.json", "discarded\n")
	write("g.txt", "g\n")
	commit("side3 edit")
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "-s", "ours", "-m", "merge ours", "side3")

	git("push", "-q", bare, "main")
	return url, bare
}

func TestHistoryMatchesGitLog(t *testing.T) {
	ctx := context.Background()
	url, bare := histRepo(t)
	// The fixture must be one where first-parent walking gives the wrong answer.
	if gittest.Git(t, bare, "log", "--format=%H", "main", "--", "f.json") ==
		gittest.Git(t, bare, "log", "--first-parent", "--format=%H", "main", "--", "f.json") {
		t.Fatal("fixture does not distinguish git log from first-parent history")
	}
	for _, depth := range []int{100, 1} { // depth 1 forces a fetch at every step
		r, err := Open(ctx, url, NewCache(64<<20, true), HistoryDepth(depth))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"f.json", "a", "a/x", "other.txt", "g.txt", "", "missing"} {
			for _, limit := range []int{0, 1, 2} {
				args := []string{"log", "--format=%H"}
				if limit > 0 {
					args = append(args, fmt.Sprintf("-n%d", limit))
				}
				args = append(args, "main")
				if p != "" {
					args = append(args, "--", p)
				}
				want := strings.Fields(gittest.Git(t, bare, args...))
				h, err := r.GetHistory(ctx, "main", p, limit)
				if err != nil {
					t.Fatal(err)
				}
				if got := shaList(h); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
					t.Errorf("depth %d: GetHistory(%q, %d)\n got  %v\n want %v (git log)", depth, p, limit, msgs(t, bare, got), msgs(t, bare, want))
				}
			}
		}
	}
}

func msgs(t *testing.T, bare string, shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = strings.TrimSpace(gittest.Git(t, bare, "log", "-1", "--format=%s", s))
	}
	return out
}

// authProxy fronts a test git server and accepts only the current token.
type authProxy struct {
	*httptest.Server
	token atomic.Value // string
	seen  atomic.Int64
}

func newAuthProxy(t *testing.T, upstream string) *authProxy {
	u, _ := url.Parse(upstream)
	rp := httputil.NewSingleHostReverseProxy(u)
	p := &authProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.seen.Add(1)
		_, pass, ok := r.BasicAuth()
		if !ok || pass != p.token.Load().(string) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func TestAuthFuncRotatesWithoutReopening(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	proxy := newAuthProxy(t, f.srv.URL)
	proxy.token.Store("token-1")
	url := strings.Replace(f.url, f.srv.URL, proxy.URL, 1)

	var current atomic.Value
	current.Store("token-1")
	var calls atomic.Int64
	r, err := Open(ctx, url, NewCache(64<<20, true), AuthFunc(func(req *http.Request) error {
		calls.Add(1)
		if req.Context() == nil {
			return errors.New("no context")
		}
		req.SetBasicAuth("x-access-token", current.Load().(string))
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetContent(ctx, "main", "README.md"); err != nil {
		t.Fatal(err)
	}

	// The server rotates; the repo handle (and its cache) keeps working once
	// the callback hands out the new token.
	proxy.token.Store("token-2")
	f.write(t, "README.md", "rotated\n")
	f.commit(t, "after rotation")
	if _, err := r.GetContent(ctx, "main", "README.md"); !errors.Is(err, ErrAuth) {
		t.Fatalf("stale token: err = %v, want ErrAuth", err)
	}
	current.Store("token-2")
	got, err := r.GetContent(ctx, "main", "README.md")
	if err != nil || string(got) != "rotated\n" {
		t.Fatalf("after rotation: %q, %v", got, err)
	}
	if calls.Load() != proxy.seen.Load() {
		t.Fatalf("AuthFunc called %d times for %d requests", calls.Load(), proxy.seen.Load())
	}

	// A failing callback aborts the request as ErrAuth without reaching the server.
	mintErr := errors.New("mint failed")
	r2, err := Open(ctx, url, NewCache(1<<20, true), AuthFunc(func(*http.Request) error { return mintErr }))
	if !errors.Is(err, ErrAuth) || !errors.Is(err, mintErr) || r2 != nil {
		t.Fatalf("failing AuthFunc: %v", err)
	}
}

func TestSizeLimits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	big := strings.Repeat("0123456789abcdef", 4096) // 64 KiB, compresses well
	f.write(t, "big.txt", big)
	f.commit(t, "big file")

	r, err := Open(ctx, f.url, NewCache(64<<20, true), MaxFileSize(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetContent(ctx, "main", "README.md"); err != nil {
		t.Fatalf("small file under MaxFileSize: %v", err)
	}
	if _, err := r.GetContent(ctx, "main", "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("big file: err = %v, want ErrTooLarge", err)
	}

	// MaxFetchSize bounds the decoded total even when the wire bytes are tiny.
	r, err = Open(ctx, f.url, NewCache(64<<20, true), MaxFetchSize(32<<10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetContent(ctx, "main", "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("fetch over MaxFetchSize: err = %v, want ErrTooLarge", err)
	}
	r, err = Open(ctx, f.url, NewCache(64<<20, true), MaxFetchSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetContent(ctx, "main", "big.txt"); err != nil || string(got) != big {
		t.Fatalf("within limits: %d bytes, %v", len(got), err)
	}
}

// A server that streams an endless pack is cut off at MaxFetchSize.
func TestMaxFetchSizeStopsEndlessPack(t *testing.T) {
	ctx := context.Background()
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, pkt("version 2\n")+pkt("fetch=shallow filter\n")+"0000")
			return
		}
		io.WriteString(w, pkt("packfile\n"))
		// A pack header claiming 4 billion objects, then 32 KiB blobs forever.
		var body bytes.Buffer
		body.WriteString("\x01PACK\x00\x00\x00\x02\xff\xff\xff\xff")
		blob := make([]byte, 32<<10)
		body.Write([]byte{0x30 | 0x80, 0x80, 0x10}) // type blob, size 32 KiB
		zw, _ := zlib.NewWriterLevel(&body, zlib.NoCompression)
		zw.Write(blob)
		zw.Close()
		send := func(b []byte) error {
			_, err := io.WriteString(w, fmt.Sprintf("%04x", len(b)+4))
			if err == nil {
				_, err = w.Write(b)
			}
			return err
		}
		first, obj := body.Bytes()[:13], body.Bytes()[13:]
		send(first)
		for i := 0; i < 1<<20 && r.Context().Err() == nil; i++ {
			for len(obj) > 0 {
				n := min(len(obj), 65000)
				if send(append([]byte{1}, obj[:n]...)) != nil {
					return
				}
				obj = obj[n:]
			}
			obj = body.Bytes()[13:]
		}
	}))
	defer srv.Close()
	r, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), MaxFetchSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.GetContent(ctx, strings.Repeat("a", 40), "f")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// Many goroutines share one Repo and a cache far smaller than the repo, so
// commits, trees and files are evicted and refetched while others read them.
// Run with -race.
func TestConcurrentReadsUnderEviction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for i := range 40 {
		f.write(t, fmt.Sprintf("d%d/e%d/file%d.txt", i%5, i%3, i), strings.Repeat(fmt.Sprintf("content %d\n", i), 100+i))
	}
	base := f.commit(t, "many files")
	for i := range 5 {
		f.write(t, fmt.Sprintf("d%d/e0/file%d.txt", i, i*5), fmt.Sprintf("edit %d\n", i))
		f.commit(t, fmt.Sprintf("edit %d", i))
	}
	head := f.head(t)

	// Reference answers from a roomy cache.
	ref, err := Open(ctx, f.url, NewCache(64<<20, true))
	if err != nil {
		t.Fatal(err)
	}
	files, err := ref.ListFiles(ctx, head, "")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(files))
	for i, fl := range files {
		paths[i] = fl.Path
	}
	wantAll, err := ref.GetContents(ctx, head, paths)
	if err != nil {
		t.Fatal(err)
	}
	wantHist, err := ref.GetHistory(ctx, head, "d0", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantChanges, err := ref.GetChanges(ctx, base, head)
	if err != nil {
		t.Fatal(err)
	}

	cache := NewCache(16<<10, true) // a fraction of the repo
	r, err := Open(ctx, f.url, cache, HistoryDepth(2))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 6 {
				var err error
				switch (g + i) % 5 {
				case 0:
					p := paths[(g*7+i)%len(paths)]
					var got []byte
					if got, err = r.GetContent(ctx, head, p); err == nil && !bytes.Equal(got, wantAll[p]) {
						err = fmt.Errorf("GetContent(%s) mismatch", p)
					}
				case 1:
					var got map[string][]byte
					if got, err = r.GetContents(ctx, head, paths); err == nil && !reflect.DeepEqual(got, wantAll) {
						err = errors.New("GetContents mismatch")
					}
				case 2:
					var got []File
					if got, err = r.ListFiles(ctx, head, ""); err == nil && !reflect.DeepEqual(got, files) {
						err = errors.New("ListFiles mismatch")
					}
				case 3:
					var got []Commit
					if got, err = r.GetHistory(ctx, head, "d0", 0); err == nil && !reflect.DeepEqual(shaList(got), shaList(wantHist)) {
						err = errors.New("GetHistory mismatch")
					}
				case 4:
					var got []Change
					if got, err = r.GetChanges(ctx, base, head); err == nil && !reflect.DeepEqual(got, wantChanges) {
						err = errors.New("GetChanges mismatch")
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if st := cache.Stats(); st.StoredBytes > 16<<10 {
		t.Fatalf("cache over budget: %+v", st)
	}
}
