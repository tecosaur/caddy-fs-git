package caddyfsgit

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	repoPath string `json:"path,omitempty"`
	revision string `json:"revision,omitempty"`
	repo  *git.Repository
	logger *zap.Logger
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
        return fmt.Errorf("failed to open git repository: %w", err)
    }
	// TODO
	return nil
}

func (fs *FS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// TODO
	return nil
}
