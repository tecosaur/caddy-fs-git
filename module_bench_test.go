package caddyfsgit

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// A small site: 200 pages of 10 KiB
func benchSite(b *testing.B) (*testRepo, []string) {
	b.Helper()
	files := map[string]string{}
	var names []string
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("posts/%03d/index.html", i)
		files[name] = strings.Repeat("x", 10<<10)
		names = append(names, name)
	}
	t := &testing.T{}
	repo := newTestRepo(t, files)
	if t.Failed() {
		b.Fatal("could not create the repository")
	}
	return repo, names
}

func readAll(b *testing.B, fsys fs.FS, name string) {
	file, err := fsys.Open(name)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, file); err != nil {
		b.Fatal(err)
	}
	file.Close()
}

func benchReads(b *testing.B, fsys fs.FS, names []string) {
	for _, name := range names { // warm
		readAll(b, fsys, name)
	}
	b.SetBytes(10 << 10)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			readAll(b, fsys, names[i%len(names)])
			i++
		}
	})
}

func BenchmarkCacheHits(b *testing.B) {
	repo, names := benchSite(b)
	gfs := repo.serve(64<<20, time.Hour)
	benchReads(b, gfs, names)
}

// Every read through go-git, as each request was before the cache
func BenchmarkGoGitReads(b *testing.B) {
	repo, names := benchSite(b)
	gfs := repo.serve(64<<10, time.Hour) // 10 KiB pages exceed a sixteenth of it
	benchReads(b, gfs, names)
}

func BenchmarkPlainFiles(b *testing.B) {
	repo, names := benchSite(b)
	benchReads(b, os.DirFS(repo.dir), names)
}

// A deploy changing one page, then the whole site requested again
func BenchmarkSiteAfterCommit(b *testing.B) {
	repo, names := benchSite(b)
	gfs := repo.serve(64<<20, time.Hour)
	for _, name := range names {
		readAll(b, gfs, name)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		repo.commit(map[string]string{names[i%len(names)]: fmt.Sprintf("revision %d", i)})
		gfs.checkedAt.Store(0) // the next request resolves the revision, as after a refresh interval
		b.StartTimer()
		for _, name := range names {
			readAll(b, gfs, name)
		}
	}
}
