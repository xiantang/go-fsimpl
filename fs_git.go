package fsimpl

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/hairyhenderson/go-fsimpl/internal/billyadapter"
	"github.com/hairyhenderson/go-fsimpl/internal/env"
)

type gitFS struct {
	ctx     context.Context
	client  *http.Client
	headers http.Header

	repofs fs.FS

	repo *url.URL
	root string
}

// GitFS provides a file system (an fs.FS) for the git repository indicated by
// the given URL. Valid schemes are "git", "git+file", "git+http", "git+https",
// "git+ssh", "file", "http", "https", and "ssh".
//
// A context can be given by using WithContextFS.
// HTTP Headers can be provided by using WithHeaderFS.
func GitFS(base *url.URL) fs.FS {
	repoURL := *base
	repoPath, root := splitRepoPath(repoURL.Path)
	repoURL.Path = repoPath

	if root == "" {
		root = "/"
	}

	return &gitFS{
		ctx:     context.Background(),
		client:  http.DefaultClient,
		repo:    &repoURL,
		root:    root,
		headers: http.Header{},
	}
}

var (
	_ fs.FS         = (*gitFS)(nil)
	_ fs.ReadDirFS  = (*gitFS)(nil)
	_ withContexter = (*gitFS)(nil)
	_ withHeaderer  = (*gitFS)(nil)
)

func (f gitFS) WithHTTPClient(client *http.Client) fs.FS {
	fsys := f
	fsys.client = client

	return &fsys
}

func (f gitFS) WithContext(ctx context.Context) fs.FS {
	fsys := f
	fsys.ctx = ctx

	return &fsys
}

func (f gitFS) WithHeader(headers http.Header) fs.FS {
	fsys := f
	if len(fsys.headers) == 0 {
		fsys.headers = headers
	} else {
		for k, vs := range fsys.headers {
			for _, v := range vs {
				fsys.headers.Add(k, v)
			}
		}
	}

	return &fsys
}

// validPath - return a valid path for fs.FS operations from a traditional path
func validPath(p string) string {
	if p == "/" || p == "" {
		return "."
	}

	return strings.TrimPrefix(p, "/")
}

func (f *gitFS) clone() (fs.FS, error) {
	if f.repofs == nil {
		depth := 1
		if f.repo.Scheme == "git+file" {
			// we can't do shallow clones for filesystem repos apparently
			depth = 0
		}

		hc := githttp.NewClient(f.client)
		client.InstallProtocol("http", hc)
		client.InstallProtocol("https", hc)

		bfs, _, err := gitClone(f.ctx, *f.repo, depth)
		if err != nil {
			return nil, err
		}

		fsys := billyadapter.BillyToFS(bfs)

		f.repofs, err = fs.Sub(fsys, validPath(f.root))
		if err != nil {
			return nil, err
		}
	}

	return f.repofs, nil
}

func (f *gitFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	fsys, err := f.clone()
	if err != nil {
		return nil, fmt.Errorf("open: failed to clone: %w", err)
	}

	return fsys.Open(name)
}

func (f *gitFS) ReadDir(name string) ([]fs.DirEntry, error) {
	fsys, err := f.clone()
	if err != nil {
		return nil, fmt.Errorf("readdir: failed to clone: %w", err)
	}

	return fs.ReadDir(fsys, name)
}

// Split the git repo path from the subpath, delimited by "//"
func splitRepoPath(repopath string) (repo, subpath string) {
	parts := strings.SplitN(repopath, "//", 2)
	switch len(parts) {
	case 1:
		subpath = "/"
	case 2:
		subpath = "/" + parts[1]

		i := strings.LastIndex(repopath, subpath)
		repopath = repopath[:i-1]
	}

	if subpath != "/" {
		subpath = strings.TrimSuffix(subpath, "/")
	}

	return repopath, subpath
}

func refFromURL(u url.URL) plumbing.ReferenceName {
	switch {
	case strings.HasPrefix(u.Fragment, "refs/"):
		return plumbing.ReferenceName(u.Fragment)
	case u.Fragment != "":
		return plumbing.NewBranchReferenceName(u.Fragment)
	default:
		return plumbing.ReferenceName("")
	}
}

// gitClone a repo for later reading through http(s), git, or ssh. u must be the URL to the repo
// itself, and must have any file path stripped
func gitClone(ctx context.Context, repoURL url.URL, depth int) (billy.Filesystem, *git.Repository, error) {
	// copy repoURL so we can perhaps use it later
	u := repoURL

	auth, err := auth(u)
	if err != nil {
		return nil, nil, err
	}

	if strings.HasPrefix(u.Scheme, "git+") {
		scheme := u.Scheme[len("git+"):]
		u.Scheme = scheme
	}

	ref := refFromURL(u)
	u.Fragment = ""
	u.RawQuery = ""

	opts := git.CloneOptions{
		URL:           u.String(),
		Auth:          auth,
		Depth:         depth,
		ReferenceName: ref,
		SingleBranch:  true,
		Tags:          git.NoTags,
	}

	bfs := memfs.New()
	bfs = billyadapter.FrozenModTimeFilesystem(bfs, time.Now())

	storer := memory.NewStorage()

	repo, err := git.CloneContext(ctx, storer, bfs, &opts)

	if u.Scheme == "file" && err == transport.ErrRepositoryNotFound && !strings.HasSuffix(u.Path, ".git") {
		// maybe this has a `.git` subdirectory...
		u = repoURL
		u.Path = path.Join(u.Path, ".git")

		return gitClone(ctx, u, depth)
	}

	if err != nil {
		return nil, nil, fmt.Errorf("git clone for %v failed: %w", repoURL, err)
	}

	return bfs, repo, nil
}

/*
auth methods:
- ssh named key (no password support)
	- GIT_SSH_KEY (base64-encoded) or GIT_SSH_KEY_FILE (base64-encoded, or not)
- ssh agent auth (preferred)
- http basic auth (for github, gitlab, bitbucket tokens)
- http token auth (bearer token, somewhat unusual)
*/
func auth(u url.URL) (transport.AuthMethod, error) {
	user := u.User.Username()

	switch u.Scheme {
	case "git+http", "git+https":
		var auth transport.AuthMethod
		if pass, ok := u.User.Password(); ok {
			auth = &githttp.BasicAuth{Username: user, Password: pass}
		} else if pass := env.Getenv("GIT_HTTP_PASSWORD"); pass != "" {
			auth = &githttp.BasicAuth{Username: user, Password: pass}
		} else if tok := env.Getenv("GIT_HTTP_TOKEN"); tok != "" {
			// note docs on TokenAuth - this is rarely to be used
			auth = &githttp.TokenAuth{Token: tok}
		}

		return auth, nil
	case "git+ssh":
		k := env.Getenv("GIT_SSH_KEY")
		if k != "" {
			key, err := base64.StdEncoding.DecodeString(k)
			if err != nil {
				key = []byte(k)
			}

			return ssh.NewPublicKeys(user, key, "")
		}

		return ssh.NewSSHAgentAuth(user)
	case "git", "git+file":
		return nil, nil
	default:
		return nil, fmt.Errorf("auth: unsupported scheme %q", u.Scheme)
	}
}