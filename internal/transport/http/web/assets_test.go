package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtAssets is a filesystem shaped like a real build: a manifest, a hashed
// module, a hashed stylesheet, and an index.
func builtAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!DOCTYPE html><html></html>")},
		".vite/manifest.json": {Data: []byte(`{
			"index.html": {
				"file": "assets/index-abc123.js",
				"css": ["assets/index-def456.css"],
				"isEntry": true
			},
			"src/app.tsx": { "file": "assets/app-ghi789.js" }
		}`)},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/app-ghi789.js":    {Data: []byte("export const a = 1")},
		"assets/index-def456.css": {Data: []byte("body{margin:0}")},
	}
}

// TestUnbuiltCheckoutIsNotAnError covers the placeholder the repository commits:
// `go build` has to work on a checkout where the frontend has not been built.
func TestUnbuiltCheckoutIsNotAnError(t *testing.T) {
	assets, err := NewAssets(fstest.MapFS{"dist/.gitkeep": {Data: []byte("placeholder")}})
	if err != nil {
		t.Fatalf("a checkout with no build was rejected: %v", err)
	}
	if assets.Built() {
		t.Error("an unbuilt checkout reported a build")
	}
	if _, _, err := assets.Entry(); err == nil {
		t.Error("an unbuilt checkout produced an entry script")
	}
}

// TestEntryComesFromTheManifest is the contract between the two toolchains: the
// Go side never guesses a hashed name.
func TestEntryComesFromTheManifest(t *testing.T) {
	assets, err := NewAssets(builtAssets())
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	if !assets.Built() {
		t.Fatal("a built frontend reported no build")
	}

	script, stylesheets, err := assets.Entry()
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if script != "/assets/index-abc123.js" {
		t.Errorf("the entry script = %q, want the hashed module from the manifest", script)
	}
	if len(stylesheets) != 1 || stylesheets[0] != "/assets/index-def456.css" {
		t.Errorf("the stylesheets = %v, want the entry's stylesheet", stylesheets)
	}
}

// TestAManifestWithoutAnEntryIsRejected keeps a half-finished build from being
// served as an empty page.
func TestAManifestWithoutAnEntryIsRejected(t *testing.T) {
	_, err := NewAssets(fstest.MapFS{
		".vite/manifest.json": {Data: []byte(`{"src/app.tsx":{"file":"assets/app.js"}}`)},
	})
	if err == nil {
		t.Fatal("a manifest with no entry was accepted")
	}
}

// TestAMalformedManifestIsRejected covers a build that half happened.
func TestAMalformedManifestIsRejected(t *testing.T) {
	if _, err := NewAssets(fstest.MapFS{".vite/manifest.json": {Data: []byte("not json")}}); err == nil {
		t.Fatal("a malformed manifest was accepted")
	}
}

// TestHashedAssetsAreImmutableAndTheShellIsNot covers the cache rules: a hashed
// name cannot change meaning, and a shell that can pins a browser to a build that
// no longer exists.
func TestHashedAssetsAreImmutableAndTheShellIsNot(t *testing.T) {
	assets, err := NewAssets(builtAssets())
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	handler := assets.Handler()

	t.Run("a hashed asset is immutable", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Errorf("Cache-Control = %q, want immutable", got)
		}
	})

	t.Run("the index is not cached", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/index.html", nil))

		if got := recorder.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
			t.Errorf("Cache-Control = %q, want no long cache for the document", got)
		}
	})
}

// TestAssetTraversalIsRefused covers the path rule: the handler resolves against
// the embedded filesystem, which refuses an escaping name.
func TestAssetTraversalIsRefused(t *testing.T) {
	assets, err := NewAssets(builtAssets())
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	handler := assets.Handler()

	for _, path := range []string{
		"/assets/../../etc/passwd",
		"/..%2f..%2fetc/passwd",
		// A directory is refused rather than listed: `http.FileServerFS` would
		// otherwise enumerate the build's file names.
		"/assets/",
		"/assets",
		"/",
		"/assets/not-there.js",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			if strings.Contains(recorder.Body.String(), "root:") {
				t.Error("the response carries file content from outside the build")
			}
		})
	}
}
