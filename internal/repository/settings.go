package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// Field names as they appear in an API payload.
//
// They are constants because a validation failure names its field, and the name
// has to be the one the caller used rather than the one Go uses.
const (
	FieldVisibility    = "visibility"
	FieldDefaultBranch = "default_branch"
	FieldArchived      = "archived"
	FieldConfirmation  = "confirmation"
	FieldRole          = "role"
)

// FieldError names one rejected field.
type FieldError struct {
	Field   string
	Message string
}

// ValidationErrors is every field a request got wrong.
//
// A settings request carries several fields, so a failure reports all of them at
// once rather than one per round trip. The transport renders this as the
// field-level part of the documented error envelope.
type ValidationErrors []FieldError

// Error renders the failures as one line.
func (v ValidationErrors) Error() string {
	if len(v) == 0 {
		return "the request is not valid"
	}
	parts := make([]string, 0, len(v))
	for _, problem := range v {
		parts = append(parts, problem.Field+": "+problem.Message)
	}
	return "the request is not valid: " + strings.Join(parts, "; ")
}

// UpdateSettingsInput is a caller's request to change repository settings.
//
// It carries exactly the settings this workstream's deliverable names:
// visibility, the default branch, and the archive state. A repository's name and
// its description are deliberately absent. The name is absent because renaming
// without a redirect for the old path is a state this phase does not want to
// reach through a settings form, and the description is absent because the audit
// catalogue in docs/reference/events.md has no type for a repository metadata
// edit — `repo.created`, `repo.renamed`, `repo.visibility_changed`,
// `repo.default_branch_changed`, `repo.archived`, `repo.unarchived`, and the
// three `repo.collaborator_*` types are the phase-1 set, and inventing a type
// here is what that catalogue exists to prevent. Both are recorded in the
// roadmap as gaps for a maintainer to settle.
//
// Every field is a pointer so "not provided" is distinguishable from "set to the
// zero value": a request that omits `archived` must not unarchive a repository,
// and a request that sets it to false must.
type UpdateSettingsInput struct {
	Actor  Principal
	RepoID string
	// Visibility changes visibility when it is not nil. It requires the
	// confirmation the transition needs.
	Visibility *Visibility
	// DefaultBranch changes the default branch when it is not nil.
	DefaultBranch *string
	// Archived archives or unarchives when it is not nil.
	Archived *bool
	// Confirmation satisfies the visibility transition, when one is requested.
	Confirmation string
	Request      audit.RequestContext
}

// IsEmpty reports whether the request would change nothing.
func (i UpdateSettingsInput) IsEmpty() bool {
	return i.Visibility == nil && i.DefaultBranch == nil && i.Archived == nil
}

// UpdateSettings applies every provided setting to a repository.
//
// It validates the whole request before it changes anything, so a payload with
// two bad fields reports both and applies neither. Only after validation does it
// apply each field, and each change writes its own audit record: "one change,
// one record" means a request that moves the visibility and the default branch
// produces two records, each naming one subject.
//
// A field whose value already matches is not an error here, unlike the
// single-purpose setters: a settings form submits the whole form, so an
// unchanged field is the normal case rather than a mistake. It writes nothing.
func (s *Service) UpdateSettings(ctx context.Context, input UpdateSettingsInput) (Repository, error) {
	if input.IsEmpty() {
		return Repository{}, ValidationErrors{{
			Field:   FieldVisibility,
			Message: "the request changes nothing; supply at least one setting",
		}}
	}

	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return Repository{}, err
	}

	// Validation covers the whole request before anything is applied.
	var problems ValidationErrors
	target := found.Visibility
	if input.Visibility != nil {
		parsed, parseErr := ParseVisibility(string(*input.Visibility))
		if parseErr != nil {
			problems = append(problems, FieldError{Field: FieldVisibility, Message: parseErr.Error()})
		} else {
			target = parsed
		}
	}
	if input.DefaultBranch != nil {
		if branchErr := ValidateBranchName(strings.TrimSpace(*input.DefaultBranch)); branchErr != nil {
			problems = append(problems, FieldError{Field: FieldDefaultBranch, Message: branchErr.Error()})
		}
	}
	// The confirmation is checked during validation rather than at the point of
	// application, so a request that also has another bad field reports both and
	// the caller learns what to type in the same round trip.
	if input.Visibility != nil && target != found.Visibility && !confirmsVisibilityChange(found, target, input.Confirmation) {
		problems = append(problems, FieldError{
			Field:   FieldConfirmation,
			Message: confirmationInstruction(found, target),
		})
	}
	if len(problems) > 0 {
		return Repository{}, problems
	}

	// Application runs in a fixed order, with archive last: it is the most
	// consequential change, and ordering it last means every other field is
	// already recorded before the repository becomes read-only.
	updated := found
	apply := func(next Repository, applyErr error) error {
		switch {
		case applyErr == nil:
			updated = next
			return nil
		case errors.Is(applyErr, ErrNoChange):
			// The field already holds this value, which a form submits routinely.
			return nil
		default:
			return applyErr
		}
	}

	if input.Visibility != nil && target != found.Visibility {
		next, visibilityErr := s.SetVisibility(ctx, SetVisibilityInput{
			Actor: input.Actor, RepoID: found.ID, Visibility: target,
			Confirmation: input.Confirmation, Request: input.Request,
		})
		if err := apply(next, visibilityErr); err != nil {
			return Repository{}, err
		}
	}
	if input.DefaultBranch != nil {
		next, branchErr := s.SetDefaultBranch(ctx, SetDefaultBranchInput{
			Actor: input.Actor, RepoID: found.ID,
			Branch: strings.TrimSpace(*input.DefaultBranch), Request: input.Request,
		})
		if err := apply(next, branchErr); err != nil {
			return Repository{}, err
		}
	}
	if input.Archived != nil {
		next, archiveErr := s.SetArchived(ctx, SetArchivedInput{
			Actor: input.Actor, RepoID: found.ID,
			Archived: *input.Archived, Request: input.Request,
		})
		if err := apply(next, archiveErr); err != nil {
			return Repository{}, err
		}
	}
	return updated, nil
}

// confirmationInstruction tells a caller what to supply.
//
// A refusal that only says "confirmation required" leaves the caller guessing,
// and the value to type is the repository's own name, which they may not have to
// hand in the shape the server compares.
func confirmationInstruction(found Repository, target Visibility) string {
	if target == VisibilityPublic && found.Visibility != VisibilityPublic {
		return fmt.Sprintf("making a repository public requires its name as the confirmation; supply %q",
			ConfirmationForVisibility(found, target))
	}
	return fmt.Sprintf("this transition requires the confirmation %q", VisibilityConfirmationWord)
}
