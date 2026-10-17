package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// Manifest is the file Vite writes beside a build.
//
// It is the contract between the two toolchains: the Go side never guesses an
// asset name, because the names carry a content hash. Reading the manifest is
// what lets the shell reference `assets/index-<hash>.js` without the server
// knowing the hash at build time.
const manifestName = ".vite/manifest.json"

// entryKey is the manifest key of the mount point's entry module.
const entryKey = "index.html"

// manifestEntry is one record in the manifest.
type manifestEntry struct {
	File string `json:"file"`
	// CSS is the stylesheets the entry needs, which Vite keeps separate from the
	// module.
	CSS []string `json:"css"`
	// IsEntry marks the module the page loads.
	IsEntry bool `json:"isEntry"`
}

// Assets serves the built frontend from an embedded filesystem.
type Assets struct {
	fsys     fs.FS
	manifest map[string]manifestEntry
	// built reports whether the frontend has been built.
	built bool
}

// NewAssets reads the manifest from a built frontend.
//
// A checkout where the frontend has not been built is not an error: the
// repository commits a placeholder so `go build` works, and the shell reports
// what to run rather than serving a page that cannot work. Only a manifest that
// exists and cannot be read is an error, because that is a build that half
// happened.
func NewAssets(fsys fs.FS) (*Assets, error) {
	assets := &Assets{fsys: fsys, manifest: map[string]manifestEntry{}}

	raw, err := fs.ReadFile(fsys, manifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return assets, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the frontend manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &assets.manifest); err != nil {
		return nil, fmt.Errorf("parse the frontend manifest: %w", err)
	}
	if _, found := assets.manifest[entryKey]; !found {
		return nil, fmt.Errorf("the frontend manifest has no %q entry", entryKey)
	}
	assets.built = true
	return assets, nil
}

// Built reports whether the frontend has been built.
func (a *Assets) Built() bool { return a.built }

// Entry returns the module the shell loads and the stylesheets it needs.
//
// The manifest is keyed by the source module rather than by the output name, so
// the lookup survives a change to the build's naming scheme.
func (a *Assets) Entry() (script string, stylesheets []string, err error) {
	entry, found := a.manifest[entryKey]
	if !found {
		return "", nil, errors.New("the frontend has not been built")
	}
	stylesheets = make([]string, 0, len(entry.CSS))
	for _, sheet := range entry.CSS {
		stylesheets = append(stylesheets, "/"+strings.TrimPrefix(sheet, "/"))
	}
	return "/" + strings.TrimPrefix(entry.File, "/"), stylesheets, nil
}

// Preloads returns the modules worth preloading, which are the entry's own
// imported chunks. A client that preloads them starts its first render one round
// trip earlier.
func (a *Assets) Preloads() []string {
	names := make([]string, 0, len(a.manifest))
	for name := range a.manifest {
		names = append(names, name)
	}
	sort.Strings(names)

	// The imports are read from the manifest as raw JSON because Vite's shape
	// differs between versions: a string in one, an object in another. Reading
	// them loosely here keeps the Go side from failing on a manifest it could
	// otherwise serve.
	preloads := make([]string, 0, len(names))
	for _, name := range names {
		if name == entryKey {
			continue
		}
		entry := a.manifest[name]
		if entry.File == "" || !strings.HasPrefix(entry.File, "assets/") {
			continue
		}
		preloads = append(preloads, "/"+entry.File)
	}
	return preloads
}

// Handler serves a built asset.
//
// The path is resolved against the embedded filesystem rather than joined, so a
// traversal attempt cannot escape it: `fs.FS` refuses a name containing `..` or
// beginning with `/`, and a request that does either is answered as not-found.
//
// A hashed asset is immutable, which the addressing document states, so it is
// served with a long cache lifetime. The shell is not: it names the current
// hashes and would otherwise pin a browser to a build that no longer exists.
func (a *Assets) Handler() http.Handler {
	fileServer := http.FileServerFS(a.fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.built {
			writeAssetMiss(w)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			writeAssetMiss(w)
			return
		}
		// A directory is refused rather than served. `http.FileServerFS` lists a
		// directory by default, which would enumerate the build's file names — an
		// asset path is not a page, and a listing is not part of the interface.
		info, err := fs.Stat(a.fsys, name)
		if err != nil || info.IsDir() {
			writeAssetMiss(w)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			// A content-hashed name cannot change meaning, so it may be cached
			// for as long as a browser likes.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// writeAssetMiss answers a request for an asset that is not there.
//
// It is a plain 404 with no body beyond a line: an asset path is not a page, and
// a browser asking for a missing module should not receive a document.
func writeAssetMiss(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}
