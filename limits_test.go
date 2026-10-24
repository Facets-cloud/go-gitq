package gitq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRefsLimit = 64 << 10
	maxPacket     = 65520    // the pkt-line protocol maximum, header included
	streamCeiling = 64 << 20 // a hostile server gives up here, so a missing limit fails instead of hanging
)

var zeroSHA = strings.Repeat("0", 40)

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// v2Advert is a minimal protocol v2 capability advertisement.
var v2Advert = pktLine("# service=git-upload-pack\n") + "0000" + pktLine("version 2\n") +
	pktLine("ls-refs\n") + pktLine("fetch=shallow filter\n") + "0000"

// endless writes next(0), next(1), ... until the client hangs up or
// streamCeiling bytes are out.
func endless(w http.ResponseWriter, r *http.Request, next func(i int) string) {
	for i, sent := 0, 0; sent < streamCeiling && r.Context().Err() == nil; i++ {
		s := next(i)
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
		sent += len(s)
	}
}

// countingBody counts the response body bytes gitq reads.
type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

type countingTransport struct{ n *atomic.Int64 }

func (t countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		resp.Body = countingBody{resp.Body, t.n}
	}
	return resp, err
}

// countingClient returns an HTTP client and the count of response body bytes
// read through it.
func countingClient() (*http.Client, *atomic.Int64) {
	n := new(atomic.Int64)
	return &http.Client{Transport: countingTransport{n}}, n
}

// checkCutOff asserts that a call against an endless response failed with
// ErrTooLarge naming the read, quickly, after reading no more than limit plus
// one packet.
func checkCutOff(t *testing.T, err error, what string, start time.Time, read, limit int64) {
	t.Helper()
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), what) {
		t.Fatalf("err = %v, want ErrTooLarge naming %q", err, what)
	}
	d := time.Since(start)
	t.Logf("cut off after %d bytes in %v: %v", read, d, err)
	if d > 5*time.Second {
		t.Fatalf("took %v to give up", d)
	}
	if read > limit+maxPacket {
		t.Fatalf("read %d bytes, want at most %d (limit %d + one packet)", read, limit+maxPacket, limit)
	}
}

// A server whose capability advertisement never ends is cut off at MaxRefsSize.
func TestMaxRefsSizeStopsEndlessAdvertisement(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, pktLine("# service=git-upload-pack\n")+"0000"+pktLine("version 2\n"))
		endless(w, r, func(i int) string { return pktLine(fmt.Sprintf("agent=git/%0100d\n", i)) })
	}))
	defer srv.Close()
	hc, read := countingClient()
	start := time.Now()
	_, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), HTTPClient(hc), MaxRefsSize(testRefsLimit))
	checkCutOff(t, err, "capability advertisement", start, read.Load(), testRefsLimit)
}

// A protocol v0/v1 server that ignores Git-Protocol advertises every ref; Open
// reads only the first line, and no more than MaxRefsSize in all.
func TestMaxRefsSizeBoundsV0Advertisement(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, pktLine("# service=git-upload-pack\n")+"0000"+
			pktLine(zeroSHA+" HEAD\x00multi_ack side-band-64k ofs-delta\n"))
		endless(w, r, func(i int) string { return pktLine(fmt.Sprintf("%s refs/heads/b%d\n", zeroSHA, i)) })
	}))
	defer srv.Close()
	hc, read := countingClient()
	start := time.Now()
	_, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), HTTPClient(hc), MaxRefsSize(testRefsLimit))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if n := read.Load(); n > testRefsLimit+maxPacket {
		t.Fatalf("read %d bytes of a v0 advertisement, limit %d", n, testRefsLimit)
	}
}

// A server whose ls-refs answer never ends is cut off at MaxRefsSize, for both
// the branch listing and a single ref lookup.
func TestMaxRefsSizeStopsEndlessLsRefs(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, v2Advert)
			return
		}
		endless(w, r, func(i int) string {
			return pktLine(fmt.Sprintf("%040x refs/heads/main-%d\n", i+1, i))
		})
	}))
	defer srv.Close()
	hc, read := countingClient()
	r, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), HTTPClient(hc), MaxRefsSize(testRefsLimit))
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"ListBranches": func() error { _, err := r.ListBranches(ctx); return err },
		"GetSHA":       func() error { _, err := r.GetSHA(ctx, "main"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			read.Store(0)
			start := time.Now()
			checkCutOff(t, call(), "ls-refs response", start, read.Load(), testRefsLimit)
		})
	}
}

// A fetch response that never reaches its end, through endless section lines
// before the pack or endless progress inside it, is cut off near MaxFetchSize.
func TestMaxFetchSizeStopsEndlessFetchResponse(t *testing.T) {
	ctx := context.Background()
	const maxFetch = 64 << 10
	for name, stream := range map[string]func(w http.ResponseWriter, r *http.Request){
		"section lines": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, pktLine("shallow-info\n"))
			endless(w, r, func(i int) string { return pktLine(fmt.Sprintf("shallow %040x\n", i+1)) })
		},
		"progress": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, pktLine("packfile\n"))
			endless(w, r, func(i int) string { return pktLine(fmt.Sprintf("\x02Counting objects: %d\r", i)) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, v2Advert)
					return
				}
				stream(w, r)
			}))
			defer srv.Close()
			hc, read := countingClient()
			r, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), HTTPClient(hc), MaxFetchSize(maxFetch))
			if err != nil {
				t.Fatal(err)
			}
			read.Store(0)
			start := time.Now()
			_, err = r.GetContent(ctx, strings.Repeat("a", 40), "f")
			// The fetch body may exceed the pack limit by framing and a little
			// slack for section lines; it must still stop far below the stream.
			checkCutOff(t, err, "fetch response", start, read.Load(), 2<<20)
		})
	}
}

// receivePackServer answers Open with a v2 advertisement and serves the
// receive-pack advertisement and push report with the given handlers.
func receivePackServer(t *testing.T, advert, report http.HandlerFunc) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("service") == "git-upload-pack":
			io.WriteString(w, v2Advert)
		case r.URL.Query().Get("service") == "git-receive-pack":
			advert(w, r)
		case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			io.Copy(io.Discard, r.Body)
			report(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var receivePreamble = pktLine("# service=git-receive-pack\n") + "0000"

// Save's receive-pack advertisement and push report are both cut off at
// MaxRefsSize when they never end.
func TestMaxRefsSizeStopsEndlessPush(t *testing.T) {
	ctx := context.Background()
	smallAdvert := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, receivePreamble+
			pktLine(zeroSHA+" capabilities^{}\x00report-status side-band-64k delete-refs\n")+"0000")
	}
	for _, tc := range []struct {
		what           string
		advert, report http.HandlerFunc
	}{
		{"receive-pack advertisement", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, receivePreamble+
				pktLine(fmt.Sprintf("%040x refs/heads/b0\x00report-status side-band-64k\n", 1)))
			endless(w, r, func(i int) string { return pktLine(fmt.Sprintf("%040x refs/heads/b%d\n", i+2, i+1)) })
		}, nil},
		{"push report", smallAdvert, func(w http.ResponseWriter, r *http.Request) {
			endless(w, r, func(i int) string { return pktLine("\x01" + pktLine(fmt.Sprintf("ok refs/heads/b%d\n", i))) })
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			srv := receivePackServer(t, tc.advert, tc.report)
			hc, read := countingClient()
			r, err := Open(ctx, srv.URL+"/x.git", NewCache(1<<20, true), HTTPClient(hc), MaxRefsSize(testRefsLimit))
			if err != nil {
				t.Fatal(err)
			}
			read.Store(0)
			start := time.Now()
			_, err = r.Save(ctx, "main", "", "m", Author{Name: "T", Email: "t@example.com"},
				map[string][]byte{"f": []byte("x")})
			// Allow for the small advertisement read before the push report.
			checkCutOff(t, err, tc.what, start, read.Load(), testRefsLimit+1<<10)
		})
	}
}

// A real repository with thousands of branches and tags works under a limit
// that fits its answers; a limit below them fails only the calls that list
// every ref, while a single-ref lookup stays small.
func TestMaxRefsSizeManyRefs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	head := f.head(t)
	var cmds strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&cmds, "create refs/heads/branch-%04d %s\ncreate refs/tags/v0.%d %s\n", i, head, i, head)
	}
	cmd := exec.Command("git", "update-ref", "--stdin")
	cmd.Dir, cmd.Stdin = f.bare, strings.NewReader(cmds.String())
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	author := Author{Name: "T", Email: "t@example.com"}

	r, err := Open(ctx, f.url, NewCache(64<<20, true), MaxRefsSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	branches, err := r.ListBranches(ctx)
	if err != nil || len(branches) != 3001 {
		t.Fatalf("ListBranches: %d branches, %v", len(branches), err)
	}
	for _, ref := range []string{"main", "branch-2999", "v0.2999"} {
		if sha, err := r.GetSHA(ctx, ref); err != nil || sha != head {
			t.Fatalf("GetSHA(%s) = %q, %v; want %s", ref, sha, err, head)
		}
	}
	if _, err := r.Save(ctx, "saved", "", "m", author, map[string][]byte{"f": []byte("x")}); err != nil {
		t.Fatalf("Save with 6000 refs advertised: %v", err)
	}

	small, err := Open(ctx, f.url, NewCache(64<<20, true), MaxRefsSize(testRefsLimit))
	if err != nil {
		t.Fatal(err)
	}
	if sha, err := small.GetSHA(ctx, "main"); err != nil || sha != head {
		t.Fatalf("GetSHA(main) under a small limit = %q, %v", sha, err)
	}
	if _, err := small.ListBranches(ctx); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "ls-refs response") {
		t.Fatalf("ListBranches over the limit: err = %v, want ErrTooLarge", err)
	}
	if _, err := small.Save(ctx, "other", "", "m", author, map[string][]byte{"f": []byte("x")}); !errors.Is(err, ErrTooLarge) ||
		!strings.Contains(err.Error(), "receive-pack advertisement") {
		t.Fatalf("Save over the limit: err = %v, want ErrTooLarge", err)
	}
}

func TestMaxRefsSizeDefault(t *testing.T) {
	f := newFixture(t)
	if got := f.repo.r.wc.MaxRefsSize; got != 16<<20 {
		t.Fatalf("default MaxRefsSize = %d, want 16 MiB", got)
	}
	r, err := Open(context.Background(), f.url, NewCache(1<<20, true), MaxRefsSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.r.wc.MaxRefsSize; got != 1<<20 {
		t.Fatalf("MaxRefsSize = %d, want 1 MiB", got)
	}
}
