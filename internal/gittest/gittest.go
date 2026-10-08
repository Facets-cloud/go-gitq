// Package gittest serves throwaway git repositories over smart HTTP (protocol v2,
// partial clone and push enabled) using the local git-http-backend, for tests.
package gittest

import (
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type Server struct {
	*httptest.Server
	Root string // GIT_PROJECT_ROOT
}

// NewServer starts an HTTP git server rooted at a temp dir.
func NewServer(t testing.TB) *Server {
	t.Helper()
	root := t.TempDir()
	execPath := strings.TrimSpace(run(t, "", "git", "--exec-path"))
	h := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"HOME=" + root,
			"GIT_CONFIG_NOSYSTEM=1",
		},
		// http-backend copies HTTP_GIT_PROTOCOL to GIT_PROTOCOL itself.
		InheritEnv: []string{"PATH"},
	}
	s := &Server{Server: httptest.NewServer(h), Root: root}
	t.Cleanup(s.Close)
	return s
}

// NewRepo creates a bare repo named name (".git" appended) and returns its URL.
// files are committed as one initial commit on main when non-empty.
func (s *Server) NewRepo(t testing.TB, name string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(s.Root, name+".git")
	run(t, "", "git", "init", "-q", "--bare", "-b", "main", bare)
	for _, kv := range [][2]string{
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
		{"http.receivepack", "true"},
		{"protocol.version", "2"},
	} {
		run(t, bare, "git", "config", kv[0], kv[1])
	}
	if len(files) > 0 {
		work := t.TempDir()
		run(t, "", "git", "init", "-q", "-b", "main", work)
		for p, c := range files {
			fp := filepath.Join(work, p)
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s.Commit(t, work, "initial")
		run(t, work, "git", "push", "-q", bare, "main")
	}
	return s.URL + "/" + name + ".git"
}

// Commit stages everything in work and commits it with a fixed identity.
func (s *Server) Commit(t testing.TB, work, msg string) {
	t.Helper()
	run(t, work, "git", "add", "-A")
	run(t, work, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", msg)
}

// Git runs git with args inside dir and returns stdout.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	return run(t, dir, "git", args...)
}

func run(t testing.TB, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
