package web

import (
	"context"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/i18n"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// ignoreDependencies are the fakes the template tests need only to construct a
// handler. They answer nothing useful, because these tests exercise the templates
// rather than a surface.
type ignoreRepositories struct{}

func (ignoreRepositories) Resolve(context.Context, repository.Principal, string, string) (repository.Repository, repository.Level, error) {
	return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
}

type ignoreContent struct{}

func (ignoreContent) ResolveCommit(_ context.Context, _, revision string) (string, error) {
	return revision, nil
}

func (ignoreContent) ListTree(context.Context, string, string, string) ([]gittransport.Entry, error) {
	return nil, nil
}

func (ignoreContent) ReadFile(context.Context, string, string, string) (gittransport.File, error) {
	return gittransport.File{}, nil
}

func (ignoreContent) Log(context.Context, string, string, string, int, int) ([]gittransport.Commit, error) {
	return nil, nil
}

type ignoreOwners struct{}

func (ignoreOwners) OwnerByUsername(context.Context, string) (identity.User, error) {
	return identity.User{}, nil
}

type ignoreSessions struct{}

func (ignoreSessions) ResolveSession(context.Context, string) (identity.User, identity.Session, []byte, error) {
	return identity.User{}, identity.Session{}, nil, nil
}

// TestTemplateSetRendersEveryPage executes every shipped template with the data
// shapes the handlers build.
//
// It exists because a template failure surfaces as an error response rather than
// as a test failure anywhere else, so a broken template would otherwise be
// visible only through a page test that reports a status code. Running the render
// here keeps the diagnosis next to the cause.
func TestTemplateSetRendersEveryPage(t *testing.T) {
	for _, locale := range i18n.Locales() {
		catalogue := i18n.For(locale)
		t.Run(locale, func(t *testing.T) {
			handler, err := NewHandler(ignoreRepositories{}, ignoreContent{}, ignoreOwners{}, ignoreSessions{}, "")
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}

			view := &resolved{
				page: page{Locale: catalogue.Locale(), Title: "owner/name"},
				row: repository.Repository{
					Name: "my-repo", Visibility: repository.VisibilityPublic, DefaultBranch: "main",
				},
				level:  repository.LevelRead,
				commit: "aaaabbbbccccddddeeeeffff0000111122223333",
			}

			cases := map[string]any{
				"overview.html": &struct {
					*resolved
					Entries []entry
					Empty   bool
					Readme  *readmeView
				}{
					resolved: view,
					Entries: []entry{{
						Name: "README.md", Href: "/h", Kind: "file",
						TypeLabel: catalogue.T(i18n.IDEntryTypeFile), Size: 7, ShowSize: true,
					}},
					Readme: &readmeView{Path: "README.md", Content: "# Title"},
				},
				"tree.html": &struct {
					*resolved
					Entries    []entry
					Path       string
					ParentHref string
				}{
					resolved: view,
					Entries: []entry{{
						Name: "src", Href: "/t", Kind: "directory",
						TypeLabel: catalogue.T(i18n.IDEntryTypeDirectory),
					}},
					Path: "src",
				},
				"file.html": &struct {
					*resolved
					Path       string
					Content    string
					Size       int64
					Binary     bool
					ParentHref string
				}{resolved: view, Path: "src/main.go", Content: "package main", Size: 12},
				"commits.html": &struct {
					*resolved
					Path     string
					Commits  []commitView
					MoreHref string
				}{
					resolved: view,
					Commits: []commitView{{
						Href: "/c", Message: "first", AuthorName: "A Person",
						AuthoredAt: "2026-01-01T00:00:00Z",
					}},
				},
				"error.html": &struct {
					*page
					ErrorBody string
				}{page: &page{Locale: catalogue.Locale(), Title: "Not found"}, ErrorBody: "nothing here"},
			}

			for name, data := range cases {
				t.Run(name, func(t *testing.T) {
					selected, found := handler.templateFor(name, catalogue)
					if !found {
						t.Fatalf("no template set for %q in %q", name, locale)
					}
					buffer := &strings.Builder{}
					if err := selected.ExecuteTemplate(buffer, "layout.html", data); err != nil {
						t.Fatalf("executing %s: %v", name, err)
					}
					rendered := buffer.String()
					if !strings.Contains(rendered, "<!DOCTYPE html>") {
						t.Errorf("%s did not render the layout", name)
					}
					// The catalogue message must be present, which proves the `T`
					// function the set was bound with is the one the template used.
					if !strings.Contains(rendered, catalogue.T(i18n.IDNavSkipToContent)) {
						t.Errorf("%s does not carry a catalogue message", name)
					}
				})
			}
		})
	}
}

// TestUnknownIdentifierIsSearchable keeps a missing message visible rather than
// blank, so a template that names an unregistered identifier is findable in the
// output.
func TestUnknownIdentifierIsSearchable(t *testing.T) {
	catalogue := i18n.For(i18n.DefaultLocale)
	if got := catalogue.T("no.such.message"); got != "no.such.message" {
		t.Errorf("an unknown identifier resolved to %q, want the identifier itself", got)
	}
	if got := catalogue.T(i18n.IDNavSkipToContent); got == i18n.IDNavSkipToContent {
		t.Errorf("a registered identifier %q resolved to itself", i18n.IDNavSkipToContent)
	}
}

func TestTemplateForFallsBackToTheDefaultLocale(t *testing.T) {
	handler, err := NewHandler(ignoreRepositories{}, ignoreContent{}, ignoreOwners{}, ignoreSessions{}, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	unknown := i18n.For("zz-ZZ")
	if unknown.Locale() != i18n.DefaultLocale {
		t.Fatalf("an unknown locale resolved to %q, want the default", unknown.Locale())
	}
	if _, found := handler.templateFor("overview.html", unknown); !found {
		t.Error("an unknown locale found no template set")
	}
}
