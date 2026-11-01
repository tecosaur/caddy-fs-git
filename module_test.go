package caddyfsgit

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A repository whose files are committed, with commit to add more
type testRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
	when time.Time
}

func newTestRepo(t *testing.T, files map[string]string) *testRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: dir, repo: repo, when: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r.commit(files)
	return r
}

func (r *testRepo) commit(files map[string]string) {
	r.t.Helper()
	r.commitWith(files, func(path, contents string) error {
		return os.WriteFile(path, []byte(contents), 0o644)
	})
}

// link commits symlinks, each name pointing at its target
func (r *testRepo) link(links map[string]string) {
	r.t.Helper()
	r.commitWith(links, func(path, target string) error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Symlink(target, path)
	})
}

// commitWith writes each entry into the worktree with write, then commits them all
func (r *testRepo) commitWith(entries map[string]string, write func(path, value string) error) {
	r.t.Helper()
	tree, err := r.repo.Worktree()
	if err != nil {
		r.t.Fatal(err)
	}
	for name, value := range entries {
		path := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := write(path, value); err != nil {
			r.t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			r.t.Fatal(err)
		}
	}
	// Commit times are in whole seconds, so each commit is dated an hour after the last
	r.when = r.when.Add(time.Hour)
	signature := &object.Signature{Name: "test", Email: "test@example.org", When: r.when}
	if _, err := tree.Commit("files", &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) serve(cacheSize int64, refresh time.Duration) *FS {
	r.t.Helper()
	// Its own handle, as Provision opens: commits come from another writer
	repo, err := git.PlainOpen(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	gfs := &FS{Repository: r.dir, Revision: "HEAD", CacheSize: cacheSize, Refresh: caddy.Duration(refresh)}
	if err := gfs.start(repo); err != nil {
		r.t.Fatal(err)
	}
	return gfs
}

var siteFiles = map[string]string{
	"index.html": "contents of index.html",
	"a/one.txt":  "contents of a/one.txt",
	"a/two.txt":  "contents of a/two.txt",
}

func read(t *testing.T, gfs *FS, name string) string {
	t.Helper()
	file, err := gfs.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestOpenReadsCommittedFiles(t *testing.T) {
	gfs := newTestRepo(t, siteFiles).serve(0, time.Hour)
	for i := 0; i < 2; i++ { // the second read is a cache hit
		if got := read(t, gfs, "a/one.txt"); got != "contents of a/one.txt" {
			t.Fatalf("read %q", got)
		}
	}
	info, err := gfs.Stat("a/one.txt")
	if err != nil || info.Size() != int64(len("contents of a/one.txt")) || info.IsDir() {
		t.Fatalf("stat %v, %v", info, err)
	}
}

func TestMissingPathsAreNotExist(t *testing.T) {
	gfs := newTestRepo(t, siteFiles).serve(0, time.Hour)
	for i := 0; i < 2; i++ { // the second is the cached absence
		if _, err := gfs.Open("missing.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("open: %v", err)
		}
		if _, err := gfs.Stat("missing.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat: %v", err)
		}
	}
	if _, err := gfs.Open("../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("open of an invalid path: %v", err)
	}
}

func TestOpenListsDirectories(t *testing.T) {
	gfs := newTestRepo(t, siteFiles).serve(0, time.Hour)
	dir, err := gfs.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	first, err := dir.(fs.ReadDirFile).ReadDir(1)
	if err != nil || len(first) != 1 || first[0].Name() != "one.txt" {
		t.Fatalf("first entry %v, %v", first, err)
	}
	rest, err := dir.(fs.ReadDirFile).ReadDir(1)
	if err != nil || len(rest) != 1 || rest[0].Name() != "two.txt" {
		t.Fatalf("second entry %v, %v", rest, err)
	}
	if _, err := dir.(fs.ReadDirFile).ReadDir(1); err != io.EOF {
		t.Fatalf("past the end: %v", err)
	}
	if _, err := dir.Read(make([]byte, 1)); err == nil {
		t.Fatal("read a directory as a file")
	}
}

func TestRefreshPicksUpNewCommits(t *testing.T) {
	repo := newTestRepo(t, siteFiles)
	gfs := repo.serve(0, 20*time.Millisecond)
	if got := read(t, gfs, "index.html"); got != "contents of index.html" {
		t.Fatalf("read %q", got)
	}
	repo.commit(map[string]string{"index.html": "changed"})
	if got := read(t, gfs, "index.html"); got != "contents of index.html" {
		t.Fatalf("changed before the refresh interval passed: %q", got)
	}
	time.Sleep(30 * time.Millisecond)
	if got := read(t, gfs, "index.html"); got != "changed" {
		t.Fatalf("read %q after the refresh interval", got)
	}
}

func TestCacheStaysWithinBudget(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 64; i++ {
		files[filepath.Join("pages", string(rune('a'+i%26))+strings.Repeat("x", i/26+1))] = strings.Repeat("p", 200)
	}
	gfs := newTestRepo(t, files).serve(16<<10, time.Hour)
	for name, contents := range files {
		if got := read(t, gfs, name); got != contents {
			t.Fatalf("%s read %q", name, got)
		}
	}
	gfs.cache.mu.Lock()
	defer gfs.cache.mu.Unlock()
	if gfs.cache.used > gfs.cache.max || gfs.cache.used <= 0 {
		t.Fatalf("cache holds %d of %d bytes", gfs.cache.used, gfs.cache.max)
	}
}

func TestLargeFilesBypassTheCache(t *testing.T) {
	large := strings.Repeat("L", 4<<10)
	gfs := newTestRepo(t, map[string]string{"large.bin": large}).serve(16<<10, time.Hour)
	for i := 0; i < 2; i++ {
		if got := read(t, gfs, "large.bin"); got != large {
			t.Fatalf("read %d bytes", len(got))
		}
	}
	gfs.cache.mu.Lock()
	defer gfs.cache.mu.Unlock()
	for _, element := range gfs.cache.items {
		if b, ok := element.Value.(*cached).entry.(*blobEntry); ok && b.data != nil {
			t.Fatal("cached the contents of a file over a sixteenth of the cache")
		}
	}
}

// Caddy serves requests concurrently; run with -race
func TestConcurrentRequests(t *testing.T) {
	repo := newTestRepo(t, siteFiles)
	gfs := repo.serve(0, time.Millisecond)
	var wait sync.WaitGroup
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 20; j++ {
				if file, err := gfs.Open("index.html"); err != nil {
					t.Error(err)
				} else if _, err := io.ReadAll(file); err != nil {
					t.Error(err)
				}
				if _, err := gfs.Stat("a/two.txt"); err != nil {
					t.Error(err)
				}
				if dir, err := gfs.Open("a"); err != nil {
					t.Error(err)
				} else if _, err := dir.(fs.ReadDirFile).ReadDir(-1); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	// A commit while requests are in flight, which refreshes then pick up
	repo.commit(map[string]string{"index.html": "changed mid-flight"})
	wait.Wait()
}

func TestRefreshFindsCommitsInNewPacks(t *testing.T) {
	repo := newTestRepo(t, siteFiles)
	gfs := repo.serve(0, 20*time.Millisecond)
	if got := read(t, gfs, "index.html"); got != "contents of index.html" {
		t.Fatalf("read %q", got)
	}
	// As a push or gc leaves it: the new commit only in a pack the FS has not seen
	repo.commit(map[string]string{"index.html": "packed"})
	if err := repo.repo.RepackObjects(&git.RepackConfig{}); err != nil {
		t.Fatal(err)
	}
	head, err := repo.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(repo.dir, ".git", "objects", head.Hash().String()[:2], head.Hash().String()[2:])
	if _, err := os.Stat(loose); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the new commit is still loose: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := read(t, gfs, "index.html"); got != "packed" {
		t.Fatalf("read %q after the refresh interval", got)
	}
}

func TestUnchangedFilesStayCachedAcrossCommits(t *testing.T) {
	repo := newTestRepo(t, siteFiles)
	gfs := repo.serve(0, 20*time.Millisecond)
	read(t, gfs, "a/one.txt")
	read(t, gfs, "a/two.txt")
	unchanged := cachedBlob(t, gfs, "a/one.txt")
	repo.commit(map[string]string{"a/two.txt": "changed"})
	time.Sleep(30 * time.Millisecond)
	if got := read(t, gfs, "a/one.txt"); got != "contents of a/one.txt" {
		t.Fatalf("read %q", got)
	}
	// Reading a blob again would replace its entry
	if cachedBlob(t, gfs, "a/one.txt") != unchanged {
		t.Fatal("read the unchanged file's blob again")
	}
	if got := read(t, gfs, "a/two.txt"); got != "changed" {
		t.Fatalf("read %q", got)
	}
}

func TestUnchangedFilesKeepTheirETagInputs(t *testing.T) {
	repo := newTestRepo(t, siteFiles)
	gfs := repo.serve(0, 20*time.Millisecond)
	stat := func(name string) fs.FileInfo {
		t.Helper()
		info, err := gfs.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	unchanged, changed := stat("a/one.txt"), stat("a/two.txt")
	repo.commit(map[string]string{"a/two.txt": "changed"})
	time.Sleep(30 * time.Millisecond)
	// Caddy's file_server derives the ETag from the modification time and size
	if after := stat("a/one.txt"); !after.ModTime().Equal(unchanged.ModTime()) || after.Size() != unchanged.Size() {
		t.Fatalf("the unchanged file went from %v, %d to %v, %d",
			unchanged.ModTime(), unchanged.Size(), after.ModTime(), after.Size())
	}
	if after := stat("a/two.txt"); !after.ModTime().After(changed.ModTime()) {
		t.Fatalf("the changed file is dated %v, not after %v", after.ModTime(), changed.ModTime())
	}
}

func TestIdenticalFilesShareOneRead(t *testing.T) {
	gfs := newTestRepo(t, map[string]string{"x.css": "same", "y/x.css": "same"}).serve(0, time.Hour)
	if read(t, gfs, "x.css") != "same" || read(t, gfs, "y/x.css") != "same" {
		t.Fatal("misread")
	}
	if cachedBlob(t, gfs, "x.css") != cachedBlob(t, gfs, "y/x.css") {
		t.Fatal("two identical files are cached apart")
	}
}

func TestListingsAreSortedByName(t *testing.T) {
	// git orders a directory as if named "a/", after "a.txt"; fs.ReadDir puts "a" first
	gfs := newTestRepo(t, map[string]string{"a.txt": "file", "a/b.txt": "nested"}).serve(0, time.Hour)
	root, err := gfs.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := root.(fs.ReadDirFile).ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "a" || !entries[0].IsDir() || entries[1].Name() != "a.txt" {
		t.Fatalf("listed %v", entries)
	}
	info, err := entries[1].Info()
	if err != nil || info.Size() != int64(len("file")) {
		t.Fatalf("a.txt is %v, %v", info, err)
	}
}

// A site with symlinks: to a file, to a directory, chained, and broken in each way
func newLinkedRepo(t *testing.T) *testRepo {
	t.Helper()
	repo := newTestRepo(t, map[string]string{
		"docs/v1/index.html": "version one",
		"docs/v2/index.html": "version two",
		"style.css":          "body {}",
	})
	repo.link(map[string]string{
		"docs/latest":    "v2",
		"docs/current":   "latest",
		"docs/style.css": "../style.css",
		"home.html":      "docs/latest/index.html",
		"dangling":       "nowhere",
		"escapes":        "../outside",
		"absolute":       "/etc/hostname",
		"through-file":   "style.css/x",
		"loop-a":         "loop-b",
		"loop-b":         "loop-a",
	})
	return repo
}

func TestOpenFollowsSymlinks(t *testing.T) {
	gfs := newLinkedRepo(t).serve(0, time.Hour)
	for name, want := range map[string]string{
		"docs/style.css":          "body {}",
		"docs/latest/index.html":  "version two",
		"docs/current/index.html": "version two",
		"home.html":               "version two",
	} {
		if got := read(t, gfs, name); got != want {
			t.Errorf("%s read %q, want %q", name, got, want)
		}
	}
	info, err := gfs.Stat("home.html")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "home.html" || !info.Mode().IsRegular() || info.Size() != int64(len("version two")) {
		t.Fatalf("home.html is %s, %v, %d bytes", info.Name(), info.Mode(), info.Size())
	}
	// Linked or not, a file keeps one modification time, and so one ETag
	target, err := gfs.Stat("docs/v2/index.html")
	if err != nil || !target.ModTime().Equal(info.ModTime()) {
		t.Fatalf("the link is dated %v, its target %v (%v)", info.ModTime(), target.ModTime(), err)
	}
}

func TestSymlinkedDirectoriesList(t *testing.T) {
	gfs := newLinkedRepo(t).serve(0, time.Hour)
	info, err := gfs.Stat("docs/current")
	if err != nil || !info.IsDir() || info.Name() != "current" {
		t.Fatalf("docs/current is %v, %v", info, err)
	}
	entries, err := fs.ReadDir(gfs, "docs/current")
	if err != nil || len(entries) != 1 || entries[0].Name() != "index.html" {
		t.Fatalf("docs/current lists %v, %v", entries, err)
	}
}

func TestBrokenSymlinksAreNotExist(t *testing.T) {
	gfs := newLinkedRepo(t).serve(0, time.Hour)
	for _, name := range []string{"dangling", "escapes", "absolute", "through-file", "dangling/x"} {
		if _, err := gfs.Open(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("open %s: %v", name, err)
		}
	}
}

func TestSymlinkLoopsAreReported(t *testing.T) {
	gfs := newLinkedRepo(t).serve(0, time.Hour)
	for _, name := range []string{"loop-a", "loop-b/x"} {
		if _, err := gfs.Stat(name); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("stat %s: %v", name, err)
		}
	}
}

// As os.ReadDir does, and Caddy's file browser expects: the link itself, which Stat follows
func TestListingsShowSymlinksThemselves(t *testing.T) {
	gfs := newLinkedRepo(t).serve(0, time.Hour)
	entries, err := fs.ReadDir(gfs, "docs")
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]fs.FileMode{}
	for _, entry := range entries {
		modes[entry.Name()] = entry.Type()
	}
	if modes["latest"] != fs.ModeSymlink || modes["style.css"] != fs.ModeSymlink || modes["v1"] != fs.ModeDir {
		t.Fatalf("docs lists %v", modes)
	}
	root, err := fs.ReadDir(gfs, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range root {
		if entry.Name() == "dangling" {
			info, err := entry.Info()
			if err != nil || info.Size() != int64(len("nowhere")) {
				t.Fatalf("dangling is %v, %v", info, err)
			}
			return
		}
	}
	t.Fatal("a dangling link went unlisted")
}

func TestRetargetedSymlinksAreFollowedAfterRefresh(t *testing.T) {
	repo := newLinkedRepo(t)
	gfs := repo.serve(0, 20*time.Millisecond)
	if got := read(t, gfs, "docs/latest/index.html"); got != "version two" {
		t.Fatalf("read %q", got)
	}
	repo.link(map[string]string{"docs/latest": "v1"})
	time.Sleep(30 * time.Millisecond)
	if got := read(t, gfs, "docs/latest/index.html"); got != "version one" {
		t.Fatalf("read %q after retargeting", got)
	}
}

// cachedBlob is the cache's entry for the blob a path holds in the current commit
func cachedBlob(t *testing.T, gfs *FS, name string) *blobEntry {
	t.Helper()
	p, err := gfs.lookup("open", name)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := gfs.cache.get(objectKey(p.blob))
	if !ok {
		t.Fatalf("%s's blob is not cached", name)
	}
	return entry.(*blobEntry)
}
