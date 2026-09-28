package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// gitLabels are the git-derived fields of the envelope. The cache holds only
// these, never the remote URL.
type gitLabels struct {
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Repo    string `json:"repo"`
	Issue   int    `json:"issue"`
}

type fileStamp struct {
	ModNano int64 `json:"mtime"`
	Size    int64 `json:"size"`
}

// cacheStamp is the validity key: the HEAD git reads for this directory
// (per-worktree for a worktree) and the common config, where origin lives.
type cacheStamp struct {
	Head   fileStamp `json:"head"`
	Config fileStamp `json:"config"`
}

type cacheEntry struct {
	Dir    string     `json:"dir"`
	Stamp  cacheStamp `json:"stamp"`
	Labels gitLabels  `json:"labels"`
}

// gitCache is one directory's cache slot. A nil *gitCache disables caching.
type gitCache struct {
	dir   string // absolute working directory
	path  string // cache file
	stamp cacheStamp
}

// openGitCache resolves the cache slot for dir without running git. It
// returns nil when the directory, its git layout or the cache dir can't be
// resolved; the caller then just runs git.
func openGitCache(dir string, env Env) *gitCache {
	if os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" {
		return nil
	}
	cacheDir := env.CacheDir
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		cacheDir = filepath.Join(base, "agentboard", "ctx")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	stamp, err := stampFor(abs)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(abs))
	return &gitCache{
		dir:   abs,
		path:  filepath.Join(cacheDir, hex.EncodeToString(sum[:16])+".json"),
		stamp: stamp,
	}
}

// load returns the cached labels when the entry is intact and still valid.
func (c *gitCache) load() (gitLabels, bool) {
	if c == nil {
		return gitLabels{}, false
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return gitLabels{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return gitLabels{}, false
	}
	if e.Dir != c.dir || e.Stamp != c.stamp {
		return gitLabels{}, false
	}
	return e.Labels, true
}

// store writes the entry atomically. Errors are ignored: the next call runs
// git again.
func (c *gitCache) store(l gitLabels) {
	if c == nil {
		return
	}
	b, err := json.Marshal(cacheEntry{Dir: c.dir, Stamp: c.stamp, Labels: l})
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp, 0o600) != nil || os.Rename(tmp, c.path) != nil {
		os.Remove(tmp)
	}
}

// stampFor finds the .git entry for dir or its nearest ancestor and stamps the
// HEAD and config files git would read.
func stampFor(dir string) (cacheStamp, error) {
	gitDir, commonDir, err := findGitDirs(dir)
	if err != nil {
		return cacheStamp{}, err
	}
	head, err := stampFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return cacheStamp{}, err
	}
	config, err := stampFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return cacheStamp{}, err
	}
	return cacheStamp{Head: head, Config: config}, nil
}

func stampFile(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{ModNano: fi.ModTime().UnixNano(), Size: fi.Size()}, nil
}

var errNoGit = errors.New("no .git found")

// findGitDirs returns the per-worktree git dir and the common git dir. A .git
// directory is both; a .git file points at a per-worktree dir whose commondir
// file points at the common one.
func findGitDirs(dir string) (gitDir, commonDir string, err error) {
	for d := dir; ; {
		dotGit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotGit)
		if err == nil {
			if fi.IsDir() {
				return dotGit, dotGit, nil
			}
			return resolveGitFile(d, dotGit)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", errNoGit
		}
		d = parent
	}
}

func resolveGitFile(worktree, dotGit string) (gitDir, commonDir string, err error) {
	target, err := readFirstLine(dotGit)
	if err != nil {
		return "", "", err
	}
	target, ok := strings.CutPrefix(target, "gitdir: ")
	if !ok || target == "" {
		return "", "", errors.New("malformed .git file")
	}
	gitDir = resolveRel(worktree, target)
	common, err := readFirstLine(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return gitDir, gitDir, nil
	}
	if err != nil {
		return "", "", err
	}
	return gitDir, resolveRel(gitDir, common), nil
}

func resolveRel(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func readFirstLine(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return strings.TrimSpace(string(line)), nil
}
