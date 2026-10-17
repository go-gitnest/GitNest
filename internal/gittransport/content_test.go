package gittransport_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/google/uuid"
)

// fixture is a real bare repository with real history, read through the real
// runner. A fake runner would prove the parsing but not that the verbs and the
// argument shapes actually work, which is the part most likely to be wrong.
type fixture struct {
	t         *testing.T
	gitPath   string
	reader    *gittransport.Reader
	storageID string
	repoPath  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("set GITNEST_TEST_GIT_BIN or install git to run content-reader tests (%v)", err)
	}

	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := layout.EnsureTmpRoot(); err != nil {
		t.Fatalf("EnsureTmpRoot: %v", err)
	}
	runner, err := platformgit.NewRunner(gitPath, layout, 4)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	reader, err := gittransport.NewReader(runner, layout)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	storageID := uuid.NewString()
	repoPath, err := layout.EnsureRepository(storageID)
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if output, err := exec.Command(gitPath, "init", "--bare", "--quiet", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}

	return &fixture{t: t, gitPath: gitPath, reader: reader, storageID: storageID, repoPath: repoPath}
}

// hashObject writes a blob and returns its identifier.
func (f *fixture) hashObject(content string) string {
	f.t.Helper()
	command := exec.Command(f.gitPath, "-C", f.repoPath, "hash-object", "-w", "--stdin")
	command.Stdin = strings.NewReader(content)
	output, err := command.Output()
	if err != nil {
		f.t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// mktree builds a tree from specification lines.
func (f *fixture) mktree(specification string) string {
	f.t.Helper()
	command := exec.Command(f.gitPath, "-C", f.repoPath, "mktree")
	command.Stdin = strings.NewReader(specification)
	output, err := command.Output()
	if err != nil {
		f.t.Fatalf("mktree: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// commitTree commits a tree on top of a parent and returns the commit
// identifier.
//
// The parent matters: `commit-tree` without `-p` produces a root commit, so a
// fixture that forgot it would have a history of one however many commits it
// created.
func (f *fixture) commitTree(tree, message string, parent string) string {
	f.t.Helper()
	args := []string{"-C", f.repoPath, "commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)

	command := exec.Command(f.gitPath, args...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=A Person", "GIT_AUTHOR_EMAIL=person@example.com",
		"GIT_COMMITTER_NAME=A Person", "GIT_COMMITTER_EMAIL=person@example.com")
	output, err := command.Output()
	if err != nil {
		f.t.Fatalf("commit-tree: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// updateRef points a ref at a commit.
func (f *fixture) updateRef(ref, commit string) {
	f.t.Helper()
	command := exec.Command(f.gitPath, "-C", f.repoPath, "update-ref", ref, commit)
	if output, err := command.CombinedOutput(); err != nil {
		f.t.Fatalf("update-ref %s: %v\n%s", ref, err, output)
	}
}

// seed builds a repository with a directory, two files, and two chained commits,
// and returns the two commit identifiers.
func (f *fixture) seed() (first, second string) {
	f.t.Helper()

	readme := f.hashObject("# Title\n\nBody text.\n")
	source := f.hashObject("package main\n\nfunc main() {}\n")
	// A commit message containing a newline, which a naive parser would split.
	message := "first commit\n\nwith a body line"

	subtree := f.mktree("100644 blob " + source + "\tmain.go\n")
	root := f.mktree(
		"100644 blob " + readme + "\tREADME.md\n" +
			"040000 tree " + subtree + "\tsrc\n")
	first = f.commitTree(root, message, "")
	f.updateRef("refs/heads/main", first)

	// A second commit on top of the first, so the history has two entries and
	// the second is reachable only from the branch.
	readmeAgain := f.hashObject("# Title\n\nChanged body.\n")
	rootAgain := f.mktree(
		"100644 blob " + readmeAgain + "\tREADME.md\n" +
			"040000 tree " + subtree + "\tsrc\n")
	second = f.commitTree(rootAgain, "second commit", first)
	f.updateRef("refs/heads/main", second)
	f.updateRef("refs/tags/v1.0.0", first)

	return first, second
}

func (f *fixture) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func TestResolveCommitResolvesRefsAndPeelsTags(t *testing.T) {
	f := newFixture(t)
	first, second := f.seed()
	ctx, cancel := f.context()
	defer cancel()

	for _, test := range []struct {
		revision string
		want     string
	}{
		{revision: "HEAD", want: second},
		{revision: "main", want: second},
		{revision: "refs/heads/main", want: second},
		// A tag must be peeled to its commit: `rev-parse` alone would return the
		// tag object's own identifier and the caller would address the wrong
		// object.
		{revision: "v1.0.0", want: first},
		{revision: second, want: second},
	} {
		t.Run(test.revision, func(t *testing.T) {
			got, err := f.reader.ResolveCommit(ctx, f.storageID, test.revision)
			if err != nil {
				t.Fatalf("ResolveCommit(%q): %v", test.revision, err)
			}
			if got != test.want {
				t.Errorf("ResolveCommit(%q) = %q, want %q", test.revision, got, test.want)
			}
		})
	}
}

func TestResolveCommitRejectsUnknownAndInvalid(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	if _, err := f.reader.ResolveCommit(ctx, f.storageID, "no-such-branch"); !errors.Is(err, gittransport.ErrNotFound) {
		t.Errorf("an unknown revision = %v, want ErrNotFound", err)
	}
	for _, revision := range []string{"", "--all", "main..other", "HEAD:etc/passwd", "main\x00"} {
		if _, err := f.reader.ResolveCommit(ctx, f.storageID, revision); !errors.Is(err, platformgit.ErrInvalidArgument) {
			t.Errorf("ResolveCommit(%q) = %v, want ErrInvalidArgument", revision, err)
		}
	}
}

func TestResolveCommitOnAnEmptyRepository(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := f.context()
	defer cancel()

	if _, err := f.reader.ResolveCommit(ctx, f.storageID, "HEAD"); !errors.Is(err, gittransport.ErrEmptyRepository) {
		t.Errorf("an empty repository = %v, want ErrEmptyRepository", err)
	}
}

func TestListTreeListsTheRootAndASubdirectory(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	root, err := f.reader.ListTree(ctx, f.storageID, "HEAD", "")
	if err != nil {
		t.Fatalf("ListTree(root): %v", err)
	}
	if len(root) != 2 {
		t.Fatalf("the root has %d entries, want 2: %+v", len(root), root)
	}
	byName := map[string]gittransport.Entry{}
	for _, entry := range root {
		byName[entry.Name] = entry
	}
	readme, found := byName["README.md"]
	if !found {
		t.Fatalf("the root listing has no README.md: %+v", root)
	}
	if readme.Type != "file" || readme.Path != "README.md" || readme.Size == 0 {
		t.Errorf("README.md = %+v, want a file entry with a size", readme)
	}
	source, found := byName["src"]
	if !found {
		t.Fatalf("the root listing has no src: %+v", root)
	}
	if source.Type != "directory" || source.Path != "src" {
		t.Errorf("src = %+v, want a directory entry", source)
	}

	// A subdirectory lists its own contents, with paths relative to the root.
	nested, err := f.reader.ListTree(ctx, f.storageID, "HEAD", "src")
	if err != nil {
		t.Fatalf("ListTree(src): %v", err)
	}
	if len(nested) != 1 || nested[0].Name != "main.go" || nested[0].Path != "src/main.go" {
		t.Fatalf("src lists %+v, want the one file with a root-relative path", nested)
	}
}

func TestListTreeRejectsPathsItCannotAddress(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	for _, path := range []string{"../etc", "a/../../b", "/etc", "-flag", "a\x00b"} {
		if _, err := f.reader.ListTree(ctx, f.storageID, "HEAD", path); !errors.Is(err, platformgit.ErrInvalidArgument) {
			t.Errorf("ListTree(%q) = %v, want ErrInvalidArgument", path, err)
		}
	}
	// A path that does not exist in the tree is not-found, not an error.
	if _, err := f.reader.ListTree(ctx, f.storageID, "HEAD", "no-such-directory"); !errors.Is(err, gittransport.ErrNotFound) {
		t.Errorf("an absent path = %v, want ErrNotFound", err)
	}
}

func TestReadFileReturnsContent(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	file, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", "README.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if file.Path != "README.md" {
		t.Errorf("path = %q, want README.md", file.Path)
	}
	if !strings.Contains(string(file.Content), "Changed body.") {
		t.Errorf("content = %q, want the content at HEAD", file.Content)
	}
	if int64(len(file.Content)) != file.Size {
		t.Errorf("size = %d, want the content length %d", file.Size, len(file.Content))
	}

	// The same file at an earlier revision returns that revision's content,
	// which is what a permalink needs.
	older, err := f.reader.ReadFile(ctx, f.storageID, "v1.0.0", "README.md")
	if err != nil {
		t.Fatalf("ReadFile at the tag: %v", err)
	}
	if !strings.Contains(string(older.Content), "Body text.") {
		t.Errorf("the tagged content = %q, want the earlier revision", older.Content)
	}

	// A file inside a directory.
	nested, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", "src/main.go")
	if err != nil {
		t.Fatalf("ReadFile(src/main.go): %v", err)
	}
	if !strings.Contains(string(nested.Content), "func main") {
		t.Errorf("nested content = %q, want the source file", nested.Content)
	}
}

func TestReadFileRefusesWhatIsNotAFile(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	if _, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", ""); !errors.Is(err, gittransport.ErrNotAFile) {
		t.Errorf("an empty path = %v, want ErrNotAFile", err)
	}
	if _, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", "src"); !errors.Is(err, gittransport.ErrNotFound) {
		t.Errorf("a directory = %v, want ErrNotFound", err)
	}
	if _, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", "absent.txt"); !errors.Is(err, gittransport.ErrNotFound) {
		t.Errorf("an absent file = %v, want ErrNotFound", err)
	}
}

// TestReadFileReportsAFileBeyondTheBound keeps a browse request from returning
// an unbounded body.
func TestReadFileReportsAFileBeyondTheBound(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := f.context()
	defer cancel()

	// Larger than the reader's bound, so it must be refused rather than
	// truncated.
	large := strings.Repeat("x", gittransport.MaxFileBytes+1024)
	blob := f.hashObject(large)
	tree := f.mktree("100644 blob " + blob + "\tlarge.txt\n")
	commit := f.commitTree(tree, "large file", "")
	f.updateRef("refs/heads/main", commit)

	if _, err := f.reader.ReadFile(ctx, f.storageID, "HEAD", "large.txt"); !errors.Is(err, gittransport.ErrFileTooLarge) {
		t.Errorf("an oversized file = %v, want ErrFileTooLarge", err)
	}
}

func TestLogListsCommitsNewestFirst(t *testing.T) {
	f := newFixture(t)
	first, second := f.seed()
	ctx, cancel := f.context()
	defer cancel()

	commits, err := f.reader.Log(ctx, f.storageID, "HEAD", "", 10, 0)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("Log returned %d commits, want 2: %+v", len(commits), commits)
	}
	if commits[0].ID != second || commits[1].ID != first {
		t.Errorf("order = %q then %q, want newest first (%q then %q)",
			commits[0].ID, commits[1].ID, second, first)
	}

	// The subject is carried, which is what a commit list shows. The body is
	// deliberately not: it belongs to the single-commit view.
	if commits[1].Message != "first commit" {
		t.Errorf("message = %q, want the committed subject", commits[1].Message)
	}
	if commits[1].AuthorName != "A Person" || commits[1].AuthorEmail != "person@example.com" {
		t.Errorf("author = %q <%q>, want the commit's author", commits[1].AuthorName, commits[1].AuthorEmail)
	}
	if commits[1].AuthoredAt == "" {
		t.Error("the authored timestamp is empty")
	}
}

func TestLogPaginatesAndFiltersByPath(t *testing.T) {
	f := newFixture(t)

	// Two chained commits, so paging has a second page to reach.
	first, second := f.seed()

	// A third commit that touches only the nested file, so a path filter has
	// something to exclude.
	readme := f.hashObject("# Title\n\nChanged body.\n")
	source := f.hashObject("package main\n\n// changed\n")
	subtree := f.mktree("100644 blob " + source + "\tmain.go\n")
	root := f.mktree("100644 blob " + readme + "\tREADME.md\n" + "040000 tree " + subtree + "\tsrc\n")
	third := f.commitTree(root, "touch source", second)
	f.updateRef("refs/heads/main", third)

	ctx, cancel := f.context()
	defer cancel()

	// The whole history is three commits, newest first.
	history, err := f.reader.Log(ctx, f.storageID, "HEAD", "", 10, 0)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("the history has %d commits, want 3: %+v", len(history), history)
	}
	if history[0].ID != third || history[2].ID != first {
		t.Errorf("order = %s..%s, want newest %s first and oldest %s last",
			history[0].ID, history[2].ID, third, first)
	}

	// A page of one, then the next page, which must be a different commit.
	pageOne, err := f.reader.Log(ctx, f.storageID, "HEAD", "", 1, 0)
	if err != nil {
		t.Fatalf("Log page one: %v", err)
	}
	pageTwo, err := f.reader.Log(ctx, f.storageID, "HEAD", "", 1, 1)
	if err != nil {
		t.Fatalf("Log page two: %v", err)
	}
	if len(pageOne) != 1 || len(pageTwo) != 1 {
		t.Fatalf("pages = %d and %d commits, want one each", len(pageOne), len(pageTwo))
	}
	if pageOne[0].ID == pageTwo[0].ID {
		t.Errorf("both pages returned %s, so the skip was ignored", pageOne[0].ID)
	}
	if pageOne[0].ID != third || pageTwo[0].ID != second {
		t.Errorf("pages = %s then %s, want %s then %s", pageOne[0].ID, pageTwo[0].ID, third, second)
	}

	// A path filter is content-based, which is Git's own semantics: a commit
	// that leaves the path's object identical is not part of that path's
	// history. The two assertions below therefore distinguish a working filter
	// from one that merely returns everything.
	//
	// src/main.go was created by the first commit, unchanged by the second, and
	// changed by the third.
	filtered, err := f.reader.Log(ctx, f.storageID, "HEAD", "src/main.go", 10, 0)
	if err != nil {
		t.Fatalf("Log filtered: %v", err)
	}
	if len(filtered) != 2 || filtered[0].ID != third || filtered[1].ID != first {
		t.Errorf("the src/main.go history = %+v, want the third and first commits only", filtered)
	}

	// README.md was created by the first commit, changed by the second, and left
	// alone by the third.
	readmeHistory, err := f.reader.Log(ctx, f.storageID, "HEAD", "README.md", 10, 0)
	if err != nil {
		t.Fatalf("Log filtered by README: %v", err)
	}
	if len(readmeHistory) != 2 || readmeHistory[0].ID != second {
		t.Errorf("the README history = %+v, want the second and first commits only", readmeHistory)
	}
	if readmeHistory[0].ID == third {
		t.Error("the commit that did not change README.md is in its history")
	}
}

func TestLogRejectsInvalidParameters(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	for _, test := range []struct {
		name   string
		limit  int
		skip   int
		mutate func(*testing.T) error
	}{
		{name: "zero limit", limit: 0, skip: 0},
		{name: "negative limit", limit: -1, skip: 0},
		{name: "limit beyond the page cap", limit: 101, skip: 0},
		{name: "negative skip", limit: 10, skip: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.reader.Log(ctx, f.storageID, "HEAD", "", test.limit, test.skip)
			if !errors.Is(err, platformgit.ErrInvalidArgument) {
				t.Errorf("Log(limit=%d, skip=%d) = %v, want ErrInvalidArgument", test.limit, test.skip, err)
			}
		})
	}

	// A revision that is not valid is refused before Git runs.
	if _, err := f.reader.Log(ctx, f.storageID, "--all", "", 10, 0); !errors.Is(err, platformgit.ErrInvalidArgument) {
		t.Errorf("an option-shaped revision = %v, want ErrInvalidArgument", err)
	}
}

func TestUnknownStorageIdentifierIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx, cancel := f.context()
	defer cancel()

	if _, err := f.reader.ListTree(ctx, uuid.NewString(), "HEAD", ""); !errors.Is(err, gittransport.ErrNotFound) {
		t.Errorf("an unknown identifier = %v, want ErrNotFound", err)
	}
	// An identifier that is not a UUID cannot even be addressed.
	if _, err := f.reader.ListTree(ctx, "../etc", "HEAD", ""); err == nil {
		t.Error("a traversal-shaped identifier was accepted")
	}
}

func TestNewReaderValidatesItsDependencies(t *testing.T) {
	base := t.TempDir()
	layout, err := storage.NewLayout(filepath.Join(base, "repositories"), filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if _, err := gittransport.NewReader(nil, layout); err == nil {
		t.Error("a nil runner was accepted")
	}
	if _, err := gittransport.NewReader(stubRunner{}, nil); err == nil {
		t.Error("a nil layout was accepted")
	}
}

// stubRunner satisfies the reader's runner interface without running anything.
type stubRunner struct{}

func (stubRunner) Run(context.Context, platformgit.Request) (platformgit.Result, error) {
	return platformgit.Result{}, nil
}
