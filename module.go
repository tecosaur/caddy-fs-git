package caddyfsgit

import (
	"bytes"
	"errors"
	"io"
	"io/fs"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/thediveo/gitrepofs"
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
type FS struct {
	fs.FS              `json:"-"`
	Repository  string `json:"repository,omitempty"`
	Revision    string `json:"revision,omitempty"`
	revCommit   plumbing.Hash
	repo        *git.Repository
	logger      *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (FS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.fs.git",
		New: func() caddy.Module { return new(FS) },
	}
}

func (fs *FS) Provision(ctx caddy.Context) error {
	fs.logger = ctx.Logger(fs)
	if fs.Repository == "" {
		fs.logger.Error("Repository is unset")
		return errors.New("repository must be set")
	}
    repo, err := git.PlainOpen(fs.Repository)
    if err != nil {
		fs.logger.Error("failed to open git repository", zap.Error(err),
			zap.String("path", fs.Repository))
        return err
    }
	if fs.Revision == "" {
		fs.logger.Info("Git Revision unset, defaulting to HEAD")
		fs.Revision = "HEAD"
	}
	fs.repo = repo
    return nil
}

func (fs *FS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() { // consume start of block
		return d.ArgErr()
	}
	if d.NextArg() { // Optional "fs git <repo> [<rev>]" form
		fs.Repository = d.Val()
		if d.NextArg() {
			fs.Revision = d.Val()
		}
	} else {
		// Form: fs git {
		//   repo[sitory] <path>
		//   rev[ision] <rev> (optional)
		// }
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "repository", "repo":
				if !d.Args(&fs.Repository) {
					return d.ArgErr()
				}
			case "revision", "rev":
				if !d.Args(&fs.Revision) {
					return d.ArgErr()
				}
			default:
				return d.Errf("'%s' not a valid caddy.fs.git option", d.Val())
			}
		}
	}
	return nil
}

func (gfs *FS) RepoFS() (fs.FS, error) {
    hash, err := gfs.repo.ResolveRevision(plumbing.Revision(gfs.Revision))
    if err != nil {
		gfs.logger.Error("failed to resolve revision", zap.Error(err))
        return nil, err
    }
	if *hash ==gfs.revCommit {
		return gfs.FS, nil
	}
    commit, err := gfs.repo.CommitObject(*hash)
    if err != nil {
		gfs.logger.Error("failed to get commit object", zap.Error(err))
        return nil, err
    }
    tree, err := commit.Tree()
    if err != nil {
        gfs.logger.Error("failed to get tree object", zap.Error(err))
		return nil, err
    }
	gfs.revCommit = *hash
	gfs.FS = gitrepofs.New(gfs.repo, tree, commit.Author.When)
	return gfs.FS, nil
}

// Since go-git's file objects aren't seekable (and Caddy requires
// seekable files), we'll just read them entirely into a `bytes.Buffer`
// and use that as the basis for a new `fs.File` type.

var (
	_ fs.File  =  (*File)(nil)
	_ io.Seeker = (*File)(nil)
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

func seekableFile(f fs.File) (fs.File, error) {
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(buf)
	finfo, _ := f.Stat() // We know this will never error
	return &File{Reader: *reader, fileinfo: finfo}, nil
}

func (gfs *FS) Open(name string) (fs.File, error) {
	repofs, err := gfs.RepoFS()
	if err != nil {
		return nil, err
	}
	file, err := repofs.Open(name)
	if err != nil {
		return nil, err
	}
	info, _ := file.Stat()
	if info.IsDir() {
		return file, nil
	} else {
		return seekableFile(file)
	}
}

// To implement StatFS
func (gfs *FS) Stat(name string) (fs.FileInfo, error) {
	repofs, err := gfs.RepoFS()
	if err != nil {
		return nil, err
	}
	file, err := repofs.Open(name)
	if err != nil {
		return nil, err
	}
	return file.Stat()
}
