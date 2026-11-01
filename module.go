package caddyfsgit

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	defaultCacheSize = 64 << 20
	defaultRefresh   = time.Second
	// As Linux's MAXSYMLINKS
	maxLinks = 40
)

func init() {
	caddy.RegisterModule(FS{})
}

// Interface guards
var (
	_ fs.FS                 = (*FS)(nil)
	_ fs.StatFS             = (*FS)(nil)
	_ caddyfile.Unmarshaler = (*FS)(nil)
)

// FS provides a view into a specific git tree.
//
// go-git is not safe for concurrent use, so reads through it are serialised,
// and what they return is cached within CacheSize. Blobs and trees are cached
// by hash, so whatever a new commit leaves unchanged stays cached, and an
// unchanged file keeps the modification time it was first served with, and so
// its ETag. What each path of the current commit is, a file, a directory
// listing or nothing, is cached by commit. The revision is resolved again on
// the first request after Refresh.
type FS struct {
	Repository string `json:"repository,omitempty"`
	Revision   string `json:"revision,omitempty"`
	// CacheSize bounds the cache, in bytes; 64 MiB by default. Files over a sixteenth of it are
	// read on every request.
	CacheSize int64 `json:"cache_size,omitempty"`
	// Refresh is how often the revision is resolved again; a second by default
	Refresh caddy.Duration `json:"refresh,omitempty"`

	logger *zap.Logger
	*served
}

// served is what the FS serves from, set up at provisioning and shared by its requests
type served struct {
	// git guards repo and view
	git       sync.Mutex
	repo      *git.Repository
	view      *view
	commit    atomic.Pointer[plumbing.Hash]
	checkedAt atomic.Int64
	cache     *cache
}

// view is one commit: its root tree and when it was made
type view struct {
	hash plumbing.Hash
	root plumbing.Hash
	when time.Time
}

// CaddyModule returns the Caddy module information.
func (FS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.fs.git",
		New: func() caddy.Module { return new(FS) },
	}
}

func (gfs *FS) Provision(ctx caddy.Context) error {
	gfs.logger = ctx.Logger(gfs)
	if gfs.Repository == "" {
		gfs.logger.Error("Repository is unset")
		return errors.New("repository must be set")
	}
	repo, err := git.PlainOpen(gfs.Repository)
	if err != nil {
		gfs.logger.Error("failed to open git repository", zap.Error(err),
			zap.String("path", gfs.Repository))
		return err
	}
	if gfs.Revision == "" {
		gfs.logger.Info("Git Revision unset, defaulting to HEAD")
		gfs.Revision = "HEAD"
	}
	return gfs.start(repo)
}

func (gfs *FS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() { // consume start of block
		return d.ArgErr()
	}
	if d.NextArg() { // Optional "fs git <repo> [<rev>]" form
		gfs.Repository = d.Val()
		if d.NextArg() {
			gfs.Revision = d.Val()
		}
		return nil
	}
	// Form: fs git {
	//   repo[sitory] <path>
	//   rev[ision] <rev> (optional)
	//   cache_size <size> (optional)
	//   refresh <duration> (optional)
	// }
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "repository", "repo":
			if !d.Args(&gfs.Repository) {
				return d.ArgErr()
			}
		case "revision", "rev":
			if !d.Args(&gfs.Revision) {
				return d.ArgErr()
			}
		case "cache_size":
			var size string
			if !d.Args(&size) {
				return d.ArgErr()
			}
			bytes, err := humanize.ParseBytes(size)
			if err != nil {
				return d.Errf("cache_size %q is not a size: %v", size, err)
			}
			gfs.CacheSize = int64(bytes)
		case "refresh":
			var interval string
			if !d.Args(&interval) {
				return d.ArgErr()
			}
			duration, err := caddy.ParseDuration(interval)
			if err != nil {
				return d.Errf("refresh %q is not a duration: %v", interval, err)
			}
			gfs.Refresh = caddy.Duration(duration)
		default:
			return d.Errf("'%s' not a valid caddy.fs.git option", d.Val())
		}
	}
	return nil
}

func (gfs *FS) Open(name string) (fs.File, error) {
	p, err := gfs.lookup("open", name)
	if err != nil {
		return nil, err
	}
	if p.info.IsDir() {
		return &dirFile{info: p.info, entries: p.listing}, nil
	}
	data, err := gfs.contents(p)
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return &File{Reader: *bytes.NewReader(data), fileinfo: p.info}, nil
}

// To implement StatFS
func (gfs *FS) Stat(name string) (fs.FileInfo, error) {
	p, err := gfs.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return p.info, nil
}

// start readies the FS over repo, resolving its revision once
func (gfs *FS) start(repo *git.Repository) error {
	if gfs.logger == nil {
		gfs.logger = zap.NewNop()
	}
	if gfs.CacheSize <= 0 {
		gfs.CacheSize = defaultCacheSize
	}
	if gfs.Refresh <= 0 {
		gfs.Refresh = caddy.Duration(defaultRefresh)
	}
	gfs.served = &served{repo: repo, cache: newCache(gfs.CacheSize)}
	gfs.git.Lock()
	defer gfs.git.Unlock()
	return gfs.resolve()
}

// resolve points the FS at the revision's commit, dropping the last commit's paths if it moved.
// Under git.
func (gfs *FS) resolve() error {
	gfs.checkedAt.Store(time.Now().UnixNano())
	hash, err := gfs.repo.ResolveRevision(plumbing.Revision(gfs.Revision))
	if err != nil {
		return fmt.Errorf("resolving %s in %s: %w", gfs.Revision, gfs.Repository, err)
	}
	last := gfs.commit.Load()
	if last != nil && *last == *hash {
		return nil
	}
	commit, err := gfs.repo.CommitObject(*hash)
	if err != nil {
		return fmt.Errorf("reading commit %s of %s: %w", hash, gfs.Repository, err)
	}
	gfs.view = &view{hash: *hash, root: commit.TreeHash, when: commit.Committer.When}
	gfs.commit.Store(hash)
	if last != nil {
		gfs.cache.dropPaths(*last)
	}
	return nil
}

// current is the commit to serve. Once Refresh has passed, one request resolves the revision
// again while the rest carry on with the commit they had.
func (gfs *FS) current() plumbing.Hash {
	last := gfs.checkedAt.Load()
	if time.Since(time.Unix(0, last)) >= time.Duration(gfs.Refresh) &&
		gfs.checkedAt.CompareAndSwap(last, time.Now().UnixNano()) {
		gfs.git.Lock()
		if err := gfs.resolve(); err != nil {
			gfs.logger.Error("failed to resolve revision; serving the last commit", zap.Error(err))
		}
		gfs.git.Unlock()
	}
	return *gfs.commit.Load()
}

// lookup finds what name is in the current commit, reading the tree only on a miss
func (gfs *FS) lookup(op, name string) (*pathEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	entry, ok := gfs.cache.get(pathKey(gfs.current(), name))
	if !ok {
		gfs.git.Lock()
		// Keyed by the commit actually read, which a refresh may have moved
		key := pathKey(gfs.view.hash, name)
		if entry, ok = gfs.cache.get(key); !ok {
			p, err := gfs.readPath(gfs.view, name)
			if err != nil {
				gfs.git.Unlock()
				return nil, &fs.PathError{Op: op, Path: name, Err: err}
			}
			gfs.cache.put(key, p)
			entry = p
		}
		gfs.git.Unlock()
	}
	p := entry.(*pathEntry)
	if p.missing {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return p, nil
}

// contents is a file's data, from the cache by blob or read through go-git
func (gfs *FS) contents(p *pathEntry) ([]byte, error) {
	if entry, ok := gfs.cache.get(objectKey(p.blob)); ok && entry.(*blobEntry).data != nil {
		return entry.(*blobEntry).data, nil
	}
	gfs.git.Lock()
	defer gfs.git.Unlock()
	return gfs.readBlob(p.blob, gfs.view.when)
}

// readPath reads what name is in a commit's tree, following symlinks. Under git.
func (gfs *FS) readPath(v *view, name string) (*pathEntry, error) {
	entry, err := gfs.walk(v, name)
	if errors.Is(err, fs.ErrNotExist) {
		return &pathEntry{missing: true}, nil
	} else if err != nil {
		return nil, err
	}
	// Named as requested, as os.Stat names a link by itself
	entry.Name = path.Base(name)
	switch entry.Mode {
	case filemode.Dir:
		listing, err := gfs.list(v, entry.Hash)
		return &pathEntry{info: dirInfo(entry.Name, v), listing: listing}, err
	case filemode.Submodule:
		return &pathEntry{missing: true}, nil
	}
	info, err := gfs.blobInfo(v, entry)
	if err != nil {
		return nil, err
	}
	// Opening usually follows, so a cacheable file is read now
	if info.size <= gfs.cache.max/16 {
		if _, err := gfs.readBlob(entry.Hash, v.when); err != nil {
			return nil, err
		}
	}
	return &pathEntry{info: info, blob: entry.Hash}, nil
}

// walk finds the entry name reaches from the root, through at most maxLinks symlinks.
// fs.ErrNotExist if it reaches nothing within the tree. Under git.
func (gfs *FS) walk(v *view, name string) (object.TreeEntry, error) {
	for links := 0; links <= maxLinks; links++ {
		entry, target, err := gfs.walkToLink(v, name)
		if err != nil || target == "" {
			return entry, err
		}
		name = target
	}
	return object.TreeEntry{}, syscall.ELOOP
}

// walkToLink finds the entry name reaches, or stops at the first symlink on the way with the
// path it redirects to. Under git.
func (gfs *FS) walkToLink(v *view, name string) (object.TreeEntry, string, error) {
	entry := object.TreeEntry{Mode: filemode.Dir, Hash: v.root}
	if name == "." {
		return entry, "", nil
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if entry.Mode != filemode.Dir {
			return entry, "", fs.ErrNotExist
		}
		entries, err := gfs.tree(entry.Hash)
		if err != nil {
			return entry, "", err
		}
		j := sort.Search(len(entries), func(j int) bool { return entries[j].Name >= part })
		if j == len(entries) || entries[j].Name != part {
			return entry, "", fs.ErrNotExist
		}
		entry = entries[j]
		if entry.Mode != filemode.Symlink {
			continue
		}
		link, err := gfs.readBlob(entry.Hash, v.when)
		if err != nil {
			return entry, "", err
		}
		// Clean resolves "..", so a target outside the tree is left with a leading one
		dir := path.Dir(strings.Join(parts[:i+1], "/"))
		target := path.Join(dir, string(link), path.Join(parts[i+1:]...))
		if path.IsAbs(string(link)) || !fs.ValidPath(target) {
			return entry, "", fs.ErrNotExist
		}
		return entry, target, nil
	}
	return entry, "", nil
}

// tree is a tree's entries sorted by name, decoded once per tree, whichever commits share it.
// Under git.
func (gfs *FS) tree(hash plumbing.Hash) ([]object.TreeEntry, error) {
	if entry, ok := gfs.cache.get(objectKey(hash)); ok {
		return entry.(*treeEntry).entries, nil
	}
	tree, err := gfs.repo.TreeObject(hash)
	if err != nil {
		return nil, err
	}
	// git orders a directory as if its name ended in a slash; by name, for searching and listing
	entries := append([]object.TreeEntry(nil), tree.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	gfs.cache.put(objectKey(hash), &treeEntry{entries: entries})
	return entries, nil
}

// list is a tree's entries, sorted by name as fs.ReadDir sorts them. Under git.
func (gfs *FS) list(v *view, hash plumbing.Hash) ([]fs.DirEntry, error) {
	tree, err := gfs.tree(hash)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, 0, len(tree))
	for _, entry := range tree {
		switch entry.Mode {
		case filemode.Submodule:
			continue
		case filemode.Dir:
			entries = append(entries, dirEntry{dirInfo(entry.Name, v)})
		default:
			info, err := gfs.blobInfo(v, entry)
			if err != nil {
				return nil, err
			}
			entries = append(entries, dirEntry{info})
		}
	}
	return entries, nil
}

func dirInfo(name string, v *view) *fileInfo {
	return &fileInfo{name: name, mode: fs.ModeDir | 0o755, modTime: v.when}
}

// blobInfo describes a file entry, dated by when its blob was first served. Under git.
func (gfs *FS) blobInfo(v *view, entry object.TreeEntry) (*fileInfo, error) {
	mode, err := entry.Mode.ToOSFileMode()
	if err != nil {
		mode = fs.ModeIrregular
	}
	b, err := gfs.blobMeta(entry.Hash, v.when)
	if err != nil {
		return nil, err
	}
	return &fileInfo{name: entry.Name, size: b.size, mode: mode, modTime: b.firstServed}, nil
}

// blobMeta is a blob's cached entry, made with its size alone if absent. Under git.
func (gfs *FS) blobMeta(hash plumbing.Hash, now time.Time) (*blobEntry, error) {
	if entry, ok := gfs.cache.get(objectKey(hash)); ok {
		return entry.(*blobEntry), nil
	}
	size, err := gfs.repo.Storer.EncodedObjectSize(hash)
	if err != nil {
		return nil, err
	}
	b := &blobEntry{size: size, firstServed: now}
	gfs.cache.put(objectKey(hash), b)
	return b, nil
}

// readBlob reads a blob's contents, caching them if small enough. Under git.
func (gfs *FS) readBlob(hash plumbing.Hash, now time.Time) ([]byte, error) {
	if entry, ok := gfs.cache.get(objectKey(hash)); ok && entry.(*blobEntry).data != nil {
		return entry.(*blobEntry).data, nil
	}
	blob, err := gfs.repo.BlobObject(hash)
	if err != nil {
		return nil, err
	}
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	meta, err := gfs.blobMeta(hash, now)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) <= gfs.cache.max/16 {
		gfs.cache.put(objectKey(hash), &blobEntry{size: meta.size, firstServed: meta.firstServed, data: data})
	}
	return data, nil
}

// Since go-git's blobs aren't seekable (and Caddy requires seekable
// files), files are read whole and served from memory.

var (
	_ fs.File        = (*File)(nil)
	_ io.Seeker      = (*File)(nil)
	_ fs.ReadDirFile = (*dirFile)(nil)
)

type File struct {
	bytes.Reader
	fileinfo fs.FileInfo
}

func (f *File) Stat() (fs.FileInfo, error) {
	return f.fileinfo, nil
}

func (f *File) Close() error {
	return nil
}

// dirFile lists a directory from its cached entries
type dirFile struct {
	info    fs.FileInfo
	entries []fs.DirEntry
	next    int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.Name(), Err: errors.New("is a directory")}
}

func (d *dirFile) Close() error { return nil }

func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.next:]
	if n <= 0 {
		d.next = len(d.entries)
		return append([]fs.DirEntry(nil), rest...), nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.next += n
	return append([]fs.DirEntry(nil), rest[:n]...), nil
}

type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() fs.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return f.modTime }
func (f *fileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f *fileInfo) Sys() any           { return nil }

type dirEntry struct{ info *fileInfo }

func (d dirEntry) Name() string               { return d.info.name }
func (d dirEntry) IsDir() bool                { return d.info.IsDir() }
func (d dirEntry) Type() fs.FileMode          { return d.info.mode.Type() }
func (d dirEntry) Info() (fs.FileInfo, error) { return d.info, nil }

// The cache's entries, never changed once cached

// pathEntry is what a path of one commit is: a file's blob, a directory's listing, or absent
type pathEntry struct {
	info    *fileInfo
	blob    plumbing.Hash
	listing []fs.DirEntry
	missing bool
}

// blobEntry is a blob's size, when it was first served, and its contents if small enough
type blobEntry struct {
	size        int64
	firstServed time.Time
	data        []byte
}

// treeEntry is a tree's entries, sorted by name
type treeEntry struct {
	entries []object.TreeEntry
}

// Bookkeeping per entry, besides its contents
const entryOverhead = 256

func (p *pathEntry) cost() int64 { return entryOverhead * int64(1+len(p.listing)) }
func (b *blobEntry) cost() int64 { return entryOverhead + int64(len(b.data)) }
func (t *treeEntry) cost() int64 { return entryOverhead * int64(1+len(t.entries)) }

// cacheKey names a path of a commit, or with no path, a git object: a blob or a tree
type cacheKey struct {
	hash plumbing.Hash
	path string
}

func pathKey(commit plumbing.Hash, name string) cacheKey { return cacheKey{hash: commit, path: name} }
func objectKey(hash plumbing.Hash) cacheKey              { return cacheKey{hash: hash} }

type cacheEntry interface{ cost() int64 }

// cache holds entries, least recently used first out, within max bytes
type cache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List
	items map[cacheKey]*list.Element
}

type cached struct {
	key   cacheKey
	entry cacheEntry
}

func newCache(max int64) *cache {
	return &cache{max: max, order: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *cache) get(key cacheKey) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*cached).entry, true
}

// put adds or replaces key's entry, evicting the least recently used beyond max
func (c *cache) put(key cacheKey, entry cacheEntry) {
	cost := entry.cost()
	if cost > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.used -= element.Value.(*cached).entry.cost()
		element.Value = &cached{key: key, entry: entry}
		c.order.MoveToFront(element)
	} else {
		c.items[key] = c.order.PushFront(&cached{key: key, entry: entry})
	}
	c.used += cost
	for c.used > c.max {
		c.remove(c.order.Back())
	}
}

// dropPaths removes a commit's path entries
func (c *cache) dropPaths(commit plumbing.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for element := c.order.Front(); element != nil; {
		next := element.Next()
		if key := element.Value.(*cached).key; key.hash == commit && key.path != "" {
			c.remove(element)
		}
		element = next
	}
}

func (c *cache) remove(element *list.Element) {
	evicted := element.Value.(*cached)
	c.order.Remove(element)
	delete(c.items, evicted.key)
	c.used -= evicted.entry.cost()
}
