# go-gitq

Read and write remote git repositories from Go without cloning or checking out.

gitq speaks the git smart-HTTP protocol (v2) directly. It fetches only the tree
objects it needs to know what files exist, then fetches file contents by hash on
demand, in batches. Everything fetched is cached in memory and content-addressed,
so repeat reads at a commit SHA never go back to the server.

```go
cache := gitq.NewCache(256 << 20, true) // create once per process, share across repos
repo, err := gitq.Open(ctx, "https://github.com/hashicorp/terraform", cache)

sha, _   := repo.GetSHA(ctx, "main")
data, _  := repo.GetContent(ctx, sha, "README.md")
many, _  := repo.GetContents(ctx, sha, []string{"go.mod", "main.go"})
files, _ := repo.ListFiles(ctx, sha, "internal/command")   // File{Path, SHA, Mode}
log, _   := repo.GetHistory(ctx, sha, "internal/command/apply.go", 10)
diff, _  := repo.GetChanges(ctx, "v1.9.0", "main")         // Change{Status A|M|D, Path}
_         = repo.Download(ctx, sha, "internal/command", w) // tar
branches, _ := repo.ListBranches(ctx)

newSHA, err := repo.Save(ctx, "main", sha, "update config", gitq.Author{Name: "Bot", Email: "bot@example.com"},
	map[string][]byte{
		"config/app.json": []byte(`{"replicas": 3}`),
		"old.txt":         nil, // delete
	})
if errors.Is(err, gitq.ErrConflict) { /* branch moved; re-read and retry */ }
```

The cache is required, and it is the only place fetched data lives: file
contents, directory listings and commits all share the byte budget you give it,
and the least recently used are evicted (and fetched again if needed). File
contents can be stored compressed (source code shrinks 3-5x). Objects are keyed
by hash, so one cache shared by all repos is safe and dedupes identical files.
`cache.Stats()` reports usage. Size it to hold at least the directory listings
of your largest repo at one commit (a few MB for hashicorp/terraform).

`GetHistory` returns what `git log -n limit ref -- path` returns, including
across merges.

A `ref` is a branch, tag or full commit SHA. `File.SHA` changes only when that
file's content changes, so callers can skip unchanged files cheaply.

## Requirements and limits

- HTTPS only. `git@host:owner/repo` URLs are accepted and rewritten to HTTPS.
- The server must support protocol v2 with partial-clone filters (GitHub, GitLab,
  Gitea/Forgejo do). Others fail `Open` with `ErrUnsupported`.
- Auth: `gitq.Token(tok)`, `gitq.BasicAuth(user, pass)`, or
  `gitq.AuthFunc(func(*http.Request) error)`, called on every request, for
  tokens that rotate (GitHub App installation tokens).
- Memory: `gitq.MaxFileSize(n)` (default 100 MiB) and `gitq.MaxFetchSize(n)`
  (default 512 MiB) bound what a server can make gitq allocate. Packs are
  decoded as they stream in; sizes are checked before allocating, and exceeding
  a limit fails with `ErrTooLarge`.
- Go 1.24 or later.
- Not supported: SSH, LFS content, submodule contents, merges, signed commits.

## Layout

```
gitq.go, repo.go      public API and its implementation
internal/wire         smart-HTTP client: ls-refs, fetch (v2), push (receive-pack)
internal/pack         packfile decode (incl. deltas) and encode
internal/object       commit/tree/blob encoding and hashing
internal/gittest      local git-http-backend server for tests
examples/terraform    timed walkthrough against hashicorp/terraform
```

## Try it

```
go test -short -race ./...                   # offline tests (needs the git binary)
go test ./...                                # plus live tests against GitHub
go test ./internal/pack -run '^$' -fuzz FuzzDecode -fuzztime 60s
go run ./examples/terraform -compare         # timings vs the git CLI
go run ./examples/hostcheck <repo-url>...    # every read call against any host
GITQ_LIVE=1 go test -run '^$' -bench . -benchtime 5x
```

Live tests with credentials run per host when its variables are set
(`HOST` is `GITHUB`, `GHE`, `GITLAB`, `GITLAB_SELF` or `BITBUCKET`):

```
GITQ_TEST_<HOST>_URL=https://...  GITQ_TEST_<HOST>_TOKEN=...  [GITQ_TEST_<HOST>_USER=...]
GITQ_TEST_WRITE=1   # also Save to a throwaway branch, then delete it
go test -run TestLiveHosts -v .
```

## License

MIT, see [LICENSE](LICENSE).
