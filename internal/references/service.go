package references

import (
	"context"
	"errors"
	"strconv"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The resolver that answers through the shared access decision.
//
// `urls-and-identifiers.md` §7 requires that `owner/repo#123` "requires read access to the target at
// render time and renders as plain text when access is absent". **That access question is answered by
// `repository.Service`** — the same service the API, the browser, and the Git transports call — and not
// by anything in this package. A reference that made its own decision would be a second way to read a
// repository, which is the rule `docs/architecture.md` states and the reason this type exists as a thin
// adapter rather than as a policy.
//
// # What is asked, and what is deliberately not
//
// For a **same-repository** reference the target is in the repository the text lives in, whose access the
// caller has already established: a reader looking at an issue has read access to its repository by
// definition, so no second decision is worth making and none is made. The href is still built, because
// the reader must be able to follow it.
//
// For a **qualified** reference the target is somewhere else, and the question is real. It is answered by
// looking up the owner and then asking the repository service to resolve the name for this actor — which
// is exactly how the read surfaces resolve a repository, so a reference cannot reach one they cannot.
//
// # Why "does not exist" and "may not read" are the same answer
//
// Both produce an invisible reference, which renders as plain text. That is §7's requirement, and it is
// also the T-A4 rule: a caller must not learn that a private repository exists from the fact that its
// reference behaved differently from a nonexistent one's.

// Owners resolves an `{owner}` segment of a reference.
//
// **The signature is exactly the read surfaces' `OwnerLookup`**, so `*identity.Service` satisfies both and
// a reference resolves an owner by the same two-step they do. A different shape here would need an adapter
// whose only job was to disagree about nothing.
type Owners interface {
	OwnerByUsername(ctx context.Context, username string) (identity.User, error)
}

// Access resolves a repository for a reader. It is satisfied by *repository.Service.
type Access interface {
	Resolve(ctx context.Context, viewer repository.Principal, ownerID, name string) (repository.Repository, repository.Level, error)
}

// Service resolves references through the shared decision.
type Service struct {
	owners       Owners
	repositories Access
}

// NewService returns a resolver over the two lookups.
func NewService(owners Owners, repositories Access) *Service {
	return &Service{owners: owners, repositories: repositories}
}

// LocalResolver is the `Resolver` for one repository, which is what a read surface uses.
//
// **The local context is per request rather than a field on `Service`.** One service serves every
// repository, so holding the current repository on it would be a race and, worse, would occasionally
// answer for the wrong one. A per-request view makes that impossible to express.
type LocalResolver struct {
	service *Service
	local   Local
}

// ForLocal returns the resolver for one repository, which satisfies `Resolver`.
func (s *Service) ForLocal(local Local) LocalResolver {
	return LocalResolver{service: s, local: local}
}

// Resolve implements `Resolver`.
func (r LocalResolver) Resolve(ctx context.Context, actor Actor, owner, name string, number int64) (Target, bool) {
	return r.service.resolve(ctx, actor, owner, name, number, r.local)
}

// resolve returns the target and whether this actor may read it.
//
// A same-repository reference is answered from the repository the caller already resolved. A qualified one
// is resolved through the owner lookup and the repository service.
func (s *Service) resolve(ctx context.Context, actor Actor, owner, name string, number int64, local Local) (Target, bool) {
	if number < 1 {
		return Target{}, false
	}

	if owner == "" && name == "" {
		// The target is in the repository the text lives in. Its access was established before the page
		// was rendered, so the href is built and no second decision is made.
		if local.OwnerName == "" || local.RepositoryName == "" {
			return Target{}, false
		}
		return Target{Href: href(local.OwnerName, local.RepositoryName, number)}, true
	}

	if s.owners == nil || s.repositories == nil {
		return Target{}, false
	}
	ownerAccount, err := s.owners.OwnerByUsername(ctx, owner)
	if err != nil {
		// An unknown owner and an unreadable repository are the same answer, so a caller cannot tell them
		// apart — see the comment on the type. `identity.ErrInvalidCredentials` is what the lookup reports
		// for a name no account holds.
		return Target{}, false
	}

	principal := repository.Principal{}
	if actor.AccountID != "" {
		principal = repository.User(actor.AccountID, actor.IsAdmin)
	}
	found, level, err := s.repositories.Resolve(ctx, principal, ownerAccount.ID, name)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) && !errors.Is(err, repository.ErrForbidden) {
			// An unexpected failure is reported as invisible rather than propagated: §7's rule is about
			// what renders, and a reference that cannot be resolved must not break the page it is on —
			// which would turn a database blip into an error page for content that is otherwise readable.
			return Target{}, false
		}
		return Target{}, false
	}
	if !level.Allows(repository.LevelRead) {
		return Target{}, false
	}
	// The href is built from the owner and repository names as they were **written**.
	//
	// That is correct here rather than a shortcut, and the reason is in §6.1: "routes are matched with
	// ASCII case folding for owner and repository segments", so `/Alice/Repo#1` links to `/Alice/...` and
	// the router reaches the same page the canonical `/alice/...` does. A canonical name would need a
	// second read of the account row, which the lookup this call already made does not return — and
	// spending a query per reference to change only the spelling of a URL that resolves either way is not
	// worth it.
	//
	// The **repository** name is the resolved row's rather than the written one, because the row is what
	// established the repository exists and it is already in hand, so there is no reason to repeat a
	// spelling that might not be the stored one.
	return Target{Href: href(owner, found.Name, number)}, true
}

// Local is the repository the referencing text lives in, which a same-repository reference resolves
// against.
//
// It is passed per call rather than held on the service, because one service serves every repository and a
// field would have to be mutated per request — which is a race and a wrong answer waiting to happen.
type Local struct {
	OwnerName      string
	RepositoryName string
}

// href builds the canonical path of an issue or pull-request reference.
//
// It is an **issue** path rather than a pull-request one, because a number is shared between the two and
// the issue route is the one that resolves both: `urls-and-identifiers.md` §2.4 requires that a number
// "resolved as a pull request on the `/issues/{number}` route redirects" to the pull request. So one href
// serves both, and the router is what distinguishes them.
func href(owner, repositoryName string, number int64) string {
	return "/" + owner + "/" + repositoryName + "/issues/" + strconv.FormatInt(number, 10)
}

// Marker is the text this package searches for. It is exported for a caller that needs the same
// convention, so the two cannot disagree.
const Marker = "#"
