package caddyfsgit

import (
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
	_ fs.ReadDirFS          = (*FS)(nil)
	_ caddyfile.Unmarshaler = (*FS)(nil)
)

// FS provides a view into a specific git tree.
type FS struct {
	fs.ReadDirFS `json:"-"`
	repoPath     string `json:"path,omitempty"`
	revision     string `json:"revision,omitempty"`
	repo         *git.Repository
	logger       *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (FS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.fs.git",
		New: func() caddy.Module { return new(FS) },
	}
}

func (fs *FS) Provision(ctx caddy.Context) error {
    repo, err := git.PlainOpen(fs.repoPath)
    if err != nil {
		fs.logger.Error("failed to open git repository", zap.Error(err))
        return err
    }
	fs.repo = repo
    return nil
}

func (fs *FS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Do I need to do anything?
	return nil
}

func (gfs *FS) RepoFS() (fs.FS, error) {
    hash, err := gfs.repo.ResolveRevision(plumbing.Revision(gfs.revision))
    if err != nil {
		gfs.logger.Error("failed to resolve revision", zap.Error(err))
        return nil, err
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
	return gitrepofs.New(gfs.repo, tree, commit.Author.When), nil
}

func (gfs *FS) Open(name string) (fs.File, error) {
	repofs, err := gfs.RepoFS()
	if err != nil {
		return nil, err
	}
	return repofs.Open(name)
}
