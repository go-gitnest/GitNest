package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// The read surfaces' own assets.
//
// They are embedded in the binary and served from this instance, rather than built by
// Vite, because these pages are the ones `frontend.md` requires to be complete on first
// byte and this project keeps them working on a checkout where the frontend has not been
// built. See `static/gitnest.css` for the full reasoning.
//
// The directory is inside the package so `//go:embed` can reach it: an embed pattern
// cannot cross out of its own directory, which is also why the frontend's build output
// carries its own small Go file next to it.
//
//go:embed static
var staticFS embed.FS

// StaticPrefix is the path every read-surface asset is served under.
//
// `static` is one of the reserved infrastructure names in
// `docs/architecture/urls-and-identifiers.md` section 6.9, so a repository can never be
// called this and the prefix cannot shadow a real repository path — the same reasoning
// that puts the React bundle under `/assets/`.
const StaticPrefix = "/static/"

// ReadStylesheet is the stylesheet every read surface links.
//
// It is the only element of the list today, and it is named rather than inline so the
// templates, the handler, and the tests all refer to one value.
var ReadStylesheet = []string{StaticPrefix + "gitnest.css"}

// staticHandler serves the embedded read-surface assets.
type staticHandler struct {
	fsys fs.FS
	// digest is the content hash of the served files, used as the cache key.
	digest string
}

// newStaticHandler prepares the embed for serving and hashes what it holds.
//
// The digest goes into the URL as a query parameter rather than into the file name,
// because the file name is referenced by the templates and by an operator's reverse-proxy
// rules, and a name that changed with every build would make both of those fragile. A
// query parameter is enough to make a new build a new URL for every cache in the path,
// which is all the hash is for.
//
// An error hashing the tree is returned rather than ignored: an asset set that cannot be
// read is a build fault, and a digest of the empty string would silently serve a stale
// file forever.
func newStaticHandler() (*staticHandler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("open the embedded read-surface assets: %w", err)
	}

	digest := sha256.New()
	walkErr := fs.WalkDir(sub, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		_, _ = digest.Write([]byte(path))
		_, _ = digest.Write(content)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("hash the embedded read-surface assets: %w", walkErr)
	}
	return &staticHandler{fsys: sub, digest: hex.EncodeToString(digest.Sum(nil))[:16]}, nil
}

// versioned returns the stylesheet paths with the build's cache key attached.
//
// It is what the templates link, so a reader's browser fetches one URL per build and a
// changed stylesheet is never served from a cache that holds the old one.
func (h *staticHandler) versioned() []string {
	versioned := make([]string, 0, len(ReadStylesheet))
	for _, sheet := range ReadStylesheet {
		versioned = append(versioned, sheet+"?v="+h.digest)
	}
	return versioned
}

// Handler serves the assets.
//
// The path is resolved against the embedded filesystem rather than joined, so a
// traversal attempt cannot escape it — the same rule the frontend assets follow.
// A directory is refused rather than listed, because an asset path is not a page.
func (h *staticHandler) Handler() http.Handler {
	// The prefix is stripped **before** the file server sees the request, and that is
	// not a detail: `http.FileServerFS` resolves the full `r.URL.Path` against its
	// filesystem. This filesystem is rooted at `static/`, so an unstripped
	// `/static/gitnest.css` would be looked up as `static/gitnest.css` inside it and
	// answered with the file server's own 404 — which reads as "the asset is missing"
	// rather than "the handler is wrong". The frontend assets get away without this
	// because their prefix `/assets/` happens to be the directory name inside `dist`,
	// which is a coincidence rather than a design.
	fileServer := http.StripPrefix(StaticPrefix, http.FileServerFS(h.fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, StaticPrefix)
		if name == "" {
			writeAssetMiss(w)
			return
		}
		info, err := fs.Stat(h.fsys, name)
		if err != nil || info.IsDir() {
			writeAssetMiss(w)
			return
		}

		// The content is addressed by a hash in the query, so it cannot change meaning:
		// the same URL always names the same bytes, which is what makes a long cache
		// lifetime safe. The build's digest is in that query, so a new build is a new
		// URL.
		//
		// `immutable` is used rather than a bare `max-age`, because it tells the browser
		// not to revalidate even on a reload.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}
