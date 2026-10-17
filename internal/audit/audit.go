// Package audit writes the append-only ledger of administrative changes.
//
// docs/reference/events.md defines the contract this package implements, and
// three of its rules shape the design:
//
//   - **"A failed audit write fails the operation. Losing the trail is worse
//     than refusing the change."** A record is therefore written on the same
//     transaction as the change it describes, never after it. That is why the
//     store methods in each domain take an Event alongside the change rather
//     than a writer callback: one transaction, or neither.
//   - **"One change, one record. An operation that changes two things emits two
//     records, each naming one subject."** An operation that moves a repository
//     and revokes a session emits two events.
//   - **"Types are stable identifiers."** Every type is a constant in this
//     package, taken verbatim from the catalogue, so a typo cannot invent a
//     type that no query will ever match.
//
// The names carry a conflict between two documents, and the catalogue wins: the
// naming convention in events.md section 2.1 prescribes
// `<domain>.<object>.<past_tense_verb>`, which would make the visibility event
// `repo.visibility.changed`, while the catalogue itself lists
// `repo.visibility_changed`. The catalogue is the contract a consumer matches
// on, so its spelling is used and the convention is left as a document to
// reconcile.
package audit

import (
	"context"
	"errors"
	"time"
)

// Action types, verbatim from the catalogue in docs/reference/events.md.
//
// Only the phase-1 types are listed. A later phase adds its own constants here
// rather than writing a literal at a call site, so the set stays enumerable.
const (
	// ActionRepoCreated is a repository being created.
	ActionRepoCreated = "repo.created"
	// ActionRepoRenamed is a repository's name changing.
	ActionRepoRenamed = "repo.renamed"
	// ActionRepoVisibilityChanged is a repository moving between visibilities.
	ActionRepoVisibilityChanged = "repo.visibility_changed"
	// ActionRepoDefaultBranchChanged is the default branch changing.
	ActionRepoDefaultBranchChanged = "repo.default_branch_changed"
	// ActionRepoArchived is a repository becoming read-only.
	ActionRepoArchived = "repo.archived"
	// ActionRepoUnarchived is a repository becoming writable again.
	ActionRepoUnarchived = "repo.unarchived"
	// ActionRepoCollaboratorAdded is a direct grant being created.
	ActionRepoCollaboratorAdded = "repo.collaborator_added"
	// ActionRepoCollaboratorRoleChanged is a direct grant's role changing.
	ActionRepoCollaboratorRoleChanged = "repo.collaborator_role_changed"
	// ActionRepoCollaboratorRemoved is a direct grant being revoked.
	ActionRepoCollaboratorRemoved = "repo.collaborator_removed"

	// The issue lifecycle, from the phase-2 block of the catalogue:
	//
	//	| `issue.created` / `issue.closed` / `issue.reopened` / `issue.deleted` |
	//	  An issue changes state | user | issue | 2 |
	//
	// These four are also W2.2's state-change history: the ledger is append-only,
	// records the actor and the moment, and has no foreign key to the issue, so a
	// history outlives the row it describes.
	//
	// **There is no `issue.edited`, and that is the catalogue's decision rather than an
	// omission.** An edit of a title or body therefore writes no audit record, and the
	// roadmap records the gap for the phase-2 event workstream (W2.15) to settle —
	// inventing the spelling here would put a value into the contract that nothing else
	// has agreed to.
	// The one comment action the catalogue defines:
	//
	//	| `comment.deleted` | A comment is deleted by its author or a moderator |
	//	  user / moderator | comment | 2 |
	//
	// **There is no `comment.created` and no `comment.edited`, and an edit therefore writes no
	// record.** That is consistent rather than an omission: the ledger is the security record, and
	// a comment's edit history is domain data the UI shows as "edited" — see the package comment on
	// `internal/comments` and `migrations/0008_issue_comments.up.sql`.
	ActionCommentDeleted = "comment.deleted"

	// The two label actions the catalogue defines:
	//
	//	| `label.created` / `label.deleted` | A label is created or deleted | admin | label | 2 |
	//
	// **There is no `label.edited`**, so an update of a label's name, colour, or description writes no
	// record — the same shape `issue.edited` and `comment.edited` have. The actor column says `admin`,
	// which is the repository-admin level: comparing these rows with `repo.renamed`,
	// `repo.visibility_changed`, and `repo.archived` (all `admin`, all gated by `LevelAdmin` in
	// `internal/repository`) is what fixes the mapping, while `repo.created` and `repo.forked` say
	// `user` because the actor becomes the repository's admin rather than needing to be one.
	ActionLabelCreated = "label.created"
	ActionLabelDeleted = "label.deleted"

	// The two milestone actions the catalogue defines:
	//
	//	| `milestone.closed` / `milestone.deleted` | A milestone is closed or deleted | user | milestone | 2 |
	//
	// **There is no `milestone.created`, no `milestone.edited`, and no `milestone.opened`**, so those
	// three write no record. The webhook payload catalogue at §5.2 lists a wider action set — `created`,
	// `closed`, `opened`, `edited`, `deleted` — but that is a *delivery* contract arriving in phase 4,
	// not the ledger's, and the two lists are different.
	//
	// The actor column says `user` where the label rows say `admin`, and that difference is the level:
	// milestones are content and need repository write, while labels are configuration and need admin.
	ActionMilestoneClosed  = "milestone.closed"
	ActionMilestoneDeleted = "milestone.deleted"

	ActionIssueCreated  = "issue.created"
	ActionIssueClosed   = "issue.closed"
	ActionIssueReopened = "issue.reopened"
	ActionIssueDeleted  = "issue.deleted"
)

// ActorKind is the kind of principal that acted.
type ActorKind string

const (
	// ActorUser is an authenticated account acting for itself.
	ActorUser ActorKind = "user"
	// ActorAdmin is an instance administrator acting in an oversight role.
	ActorAdmin ActorKind = "admin"
	// ActorSystem is the instance acting on its own, such as a scheduled job.
	ActorSystem ActorKind = "system"
	// ActorAnonymous is a caller with no credential whose attempt was recorded.
	ActorAnonymous ActorKind = "anonymous"
)

// ScopeType is what a record is about.
type ScopeType string

const (
	// ScopeInstance is an instance-wide change.
	ScopeInstance ScopeType = "instance"
	// ScopeAccount is a change to one account.
	ScopeAccount ScopeType = "account"
	// ScopeRepository is a change to one repository.
	ScopeRepository ScopeType = "repository"
	// ScopeIssue is a change to one issue. It is a scope rather than only a subject
	// because an issue is what a reader asks the history of, and the catalogue lists it
	// as the scope for all four lifecycle events.
	ScopeIssue ScopeType = "issue"
	// ScopeComment is a change to one comment, which the catalogue lists as the scope for
	// `comment.deleted`.
	ScopeComment ScopeType = "comment"
	// ScopeLabel is a change to one label, which the catalogue lists as the scope for the two label
	// actions.
	ScopeLabel ScopeType = "label"
	// ScopeMilestone is a change to one milestone.
	ScopeMilestone ScopeType = "milestone"
)

// SubjectType names the kind of thing that changed.
const (
	// SubjectRepository is a repository.
	SubjectRepository = "repository"
	// SubjectCollaborator is one account's direct grant on a repository.
	SubjectCollaborator = "collaborator"
	// SubjectAccount is an account.
	SubjectAccount = "account"
	// SubjectIssue is one issue.
	SubjectIssue = "issue"
	// SubjectComment is one comment.
	SubjectComment = "comment"
	// SubjectLabel is one label.
	SubjectLabel = "label"
	// SubjectMilestone is one milestone.
	SubjectMilestone = "milestone"
)

// Event is one record.
//
// It deliberately has no timestamp field: the database sets `created_at`, so the
// ordering of two records from two processes is the database's answer rather
// than two clocks'. docs/architecture/data-model.md states that rule and notes
// the one exception it allows, which this is not.
type Event struct {
	// Action is the stable type from the catalogue.
	Action string
	// ActorID is the account that acted, empty for a non-account actor.
	ActorID string
	// ActorKind distinguishes an account from the system and from an anonymous
	// attempt.
	ActorKind ActorKind
	// ScopeType and ScopeID say what the record is about.
	ScopeType ScopeType
	ScopeID   string
	// SubjectType names the kind of thing that changed, and SubjectID the one
	// instance of it.
	SubjectType string
	SubjectID   string
	// Detail carries the change itself, never secret material.
	Detail map[string]any
	// SourceAddress and UserAgent are recorded when the change came from a
	// request.
	SourceAddress string
	UserAgent     string
}

// Change describes one field that moved, for a detail blob.
//
// A record names what changed, not merely that something did: an operator asking
// "when did this become public" wants the two values.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Validate checks a record before it reaches the database.
//
// The database constraints are the guarantee; this is what turns a violation
// into an error naming the field rather than a driver error naming a constraint.
func (e Event) Validate() error {
	if e.Action == "" {
		return errors.New("an audit record needs an action")
	}
	if e.SubjectType == "" {
		return errors.New("an audit record needs a subject type")
	}
	switch e.ActorKind {
	case ActorUser, ActorAdmin:
		if e.ActorID == "" {
			return errors.New("an audit record with an account actor needs the account")
		}
	case ActorSystem, ActorAnonymous:
		if e.ActorID != "" {
			return errors.New("an audit record without an account actor must not name one")
		}
	default:
		return errors.New("an audit record needs a recognised actor kind")
	}
	switch e.ScopeType {
	case ScopeInstance:
		if e.ScopeID != "" {
			return errors.New("an instance-scoped audit record must not name a scope")
		}
	case ScopeAccount, ScopeRepository, ScopeIssue, ScopeComment, ScopeLabel, ScopeMilestone:
		if e.ScopeID == "" {
			return errors.New("a scoped audit record needs its scope")
		}
	default:
		return errors.New("an audit record needs a recognised scope type")
	}
	return nil
}

// Writer records events.
//
// The interface exists so a domain store can accept one without importing the
// database, and so a test can assert what a service decided to record.
type Writer interface {
	Record(ctx context.Context, event Event) error
}

// RequestContext is the part of a request an audit record keeps.
//
// It is a small value rather than an *http.Request so a domain service does not
// import a transport, and so a record written from a job can say it had no
// request.
type RequestContext struct {
	SourceAddress string
	UserAgent     string
}

// Actor builds the actor fields for an account acting on a request.
func Actor(actorID string, isInstanceAdmin bool, request RequestContext) (string, ActorKind, string, string) {
	kind := ActorUser
	if isInstanceAdmin {
		kind = ActorAdmin
	}
	return actorID, kind, request.SourceAddress, request.UserAgent
}

// FormatTime renders a timestamp for a detail blob.
//
// It is UTC and RFC 3339, matching the storage rule in
// docs/architecture/data-model.md, so a detail value compares with a record's
// own timestamp.
func FormatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}
