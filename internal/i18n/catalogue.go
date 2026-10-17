// Package i18n holds the message catalogues every user-visible surface reads
// from.
//
// docs/architecture/frontend.md fixes this: no user-visible string is hardcoded
// in a Go template or a component; a string is referenced by identifier and
// resolved through a catalogue, and the server-rendered and client-rendered
// surfaces read the same identifiers so a message is translated once.
//
// The catalogue is a plain map rather than a message-format dependency, because
// this phase ships one locale and the requirement is the *indirection*, not a
// formatter. Pluralisation arrives with the first message that needs it, and
// frontend.md names the platform message format as the choice for that day:
// a `count == 1` conditional written now would have to be undone then.
package i18n

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultLocale is the locale the instance serves when nothing else applies.
const DefaultLocale = "en-US"

// Message identifiers.
//
// They are grouped by surface so an identifier names where it appears, which is
// what makes a missing translation findable.
//
// A constant's *value* is the string a template names. The two are kept in step
// by the web package's test, which asserts that every identifier a template
// writes resolves to a message: a mismatch between a constant's value and a
// template's literal renders the identifier into the page, which is exactly the
// failure the indirection exists to prevent.
const (
	IDSiteName = "site.name"

	IDNavSkipToContent = "nav.skip_to_content"
	IDNavRepositories  = "nav.repositories"

	IDRepoVisibilityPrivate = "repo.visibility.private"
	IDRepoVisibilityPublic  = "repo.visibility.public"
	IDRepoNoDescription     = "repo.no_description"
	IDRepoYourPermission    = "repo.your_permission"

	IDOverviewEmptyRepo   = "overview.empty_repo"
	IDOverviewNoReadme    = "overview.no_readme"
	IDOverviewDefaultRef  = "overview.default_ref"
	IDOverviewCloneHint   = "overview.clone_hint"
	IDOverviewTreeHeading = "overview.tree_heading"

	IDIssuesHeading    = "issues.heading"
	IDIssuesEmpty      = "issues.empty"
	IDIssuesFilterAll  = "issues.filter.all"
	IDIssuesFilterOpen = "issues.filter.open"
	IDIssuesFilterShut = "issues.filter.closed"
	IDIssuesNumber     = "issues.number"
	IDIssuesStateOpen  = "issues.state.open"
	IDIssuesStateShut  = "issues.state.closed"
	IDIssuesBy         = "issues.by"
	IDIssuesClosedBy   = "issues.closed_by"
	IDIssuesHistory    = "issues.history"
	IDIssuesNoHistory  = "issues.no_history"
	IDIssuesDeleted    = "issues.deleted"
	IDIssuesNotFound   = "issues.not_found"
	IDIssuesMore       = "issues.more"
	IDIssuesCreateHint = "issues.create_hint"

	IDMilestonesHeading            = "milestones.heading"
	IDMilestonesEmpty              = "milestones.empty"
	IDMilestonesTitle              = "milestones.title"
	IDMilestonesDescription        = "milestones.description"
	IDMilestonesDue                = "milestones.due"
	IDMilestonesDueHint            = "milestones.due_hint"
	IDMilestonesCreate             = "milestones.create"
	IDMilestonesCreateSubmit       = "milestones.create_submit"
	IDMilestonesStateOpen          = "milestones.state.open"
	IDMilestonesStateClosed        = "milestones.state.closed"
	IDMilestonesProgress           = "milestones.progress"
	IDMilestonesOverdue            = "milestones.overdue"
	IDMilestonesNoDue              = "milestones.no_due"
	IDMilestonesEdit               = "milestones.edit"
	IDMilestonesSave               = "milestones.save"
	IDMilestonesClose              = "milestones.close"
	IDMilestonesReopen             = "milestones.reopen"
	IDMilestonesDelete             = "milestones.delete"
	IDMilestonesDeleteHint         = "milestones.delete_hint"
	IDMilestonesOnIssue            = "milestones.on_issue"
	IDMilestonesNoneOnIssue        = "milestones.none_on_issue"
	IDMilestonesApply              = "milestones.apply"
	IDMilestonesApplySubmit        = "milestones.apply_submit"
	IDMilestonesRemove             = "milestones.remove"
	IDMilestonesManage             = "milestones.manage"
	IDMilestonesTitleInvalid       = "milestones.error.title_invalid"
	IDMilestonesDueInvalid         = "milestones.error.due_invalid"
	IDMilestonesDescriptionInvalid = "milestones.error.description_invalid"
	IDMilestonesTitleTaken         = "milestones.error.title_taken"
	IDMilestonesAlreadyInState     = "milestones.error.already_in_state"
	IDMilestonesClosed             = "milestones.error.closed"
	IDMilestonesForbidden          = "milestones.error.forbidden"
	IDMilestonesGone               = "milestones.error.gone"
	IDMilestonesUnknown            = "milestones.error.unknown"
	IDMilestonesNotApplied         = "milestones.error.not_applied"

	IDLabelsHeading            = "labels.heading"
	IDLabelsEmpty              = "labels.empty"
	IDLabelsName               = "labels.name"
	IDLabelsColor              = "labels.color"
	IDLabelsDescription        = "labels.description"
	IDLabelsIssueCount         = "labels.issue_count"
	IDLabelsCreate             = "labels.create"
	IDLabelsCreateSubmit       = "labels.create_submit"
	IDLabelsDelete             = "labels.delete"
	IDLabelsDeleteHint         = "labels.delete_hint"
	IDLabelsColorHint          = "labels.color_hint"
	IDLabelsOnIssue            = "labels.on_issue"
	IDLabelsNoneOnIssue        = "labels.none_on_issue"
	IDLabelsApply              = "labels.apply"
	IDLabelsApplySubmit        = "labels.apply_submit"
	IDLabelsRemove             = "labels.remove"
	IDLabelsManage             = "labels.manage"
	IDLabelsNameInvalid        = "labels.error.name_invalid"
	IDLabelsColorInvalid       = "labels.error.color_invalid"
	IDLabelsDescriptionInvalid = "labels.error.description_invalid"
	IDLabelsNameTaken          = "labels.error.name_taken"
	IDLabelsForbidden          = "labels.error.forbidden"
	IDLabelsGone               = "labels.error.gone"
	IDLabelsUnknown            = "labels.error.unknown"
	IDLabelsNotApplied         = "labels.error.not_applied"

	IDCommentsHeading   = "comments.heading"
	IDCommentsEmpty     = "comments.empty"
	IDCommentsNone      = "comments.none"
	IDCommentsWrite     = "comments.write"
	IDCommentsSubmit    = "comments.submit"
	IDCommentsBody      = "comments.body"
	IDCommentsEdit      = "comments.edit"
	IDCommentsSave      = "comments.save"
	IDCommentsDelete    = "comments.delete"
	IDCommentsDeleted   = "comments.deleted_tombstone"
	IDCommentsEdited    = "comments.edited"
	IDCommentsHistory   = "comments.history"
	IDCommentsHistoryBy = "comments.history_by"
	IDCommentsBy        = "comments.by"
	IDCommentsSignIn    = "comments.error.sign_in"
	IDCommentsForbidden = "comments.error.forbidden"
	IDCommentsConflict  = "comments.error.conflict"
	IDCommentsNoChange  = "comments.error.no_change"
	IDCommentsGone      = "comments.error.gone"
	IDCommentsTooLong   = "comments.error.too_long"

	IDIssuesNewLink    = "issues.new_link"
	IDIssuesNewTitle   = "issues.new_title"
	IDIssuesNewSubmit  = "issues.new_submit"
	IDIssuesNewWorking = "issues.new_working"
	IDIssuesFormTitle  = "issues.form.title"
	IDIssuesFormBody   = "issues.form.body"
	IDIssuesFormSave   = "issues.form.save"
	IDIssuesEdit       = "issues.form.edit_legend"
	IDIssuesClose      = "issues.action.close"
	IDIssuesReopen     = "issues.action.reopen"
	IDIssuesDelete     = "issues.action.delete"
	IDIssuesDeleteHint = "issues.action.delete_hint"
	IDIssuesSaved      = "issues.action.saved"
	IDIssuesCancel     = "issues.action.cancel"

	IDIssuesTitleInvalid        = "issues.error.title_invalid"
	IDIssuesBodyInvalid         = "issues.error.body_invalid"
	IDIssuesConflict            = "issues.error.conflict"
	IDIssuesNoChange            = "issues.error.no_change"
	IDIssuesAlreadyInState      = "issues.error.already_in_state"
	IDIssuesForbidden           = "issues.error.forbidden"
	IDIssuesGone                = "issues.error.gone"
	IDIssuesTooLargeTitle       = "issues.error.too_large_title"
	IDIssuesTooLargeBody        = "issues.error.too_large_body"
	IDIssuesBadRequestTitle     = "issues.error.bad_request_title"
	IDIssuesBadRequestBody      = "issues.error.bad_request_body"
	IDIssuesSignInRequiredTitle = "issues.error.sign_in_title"
	IDIssuesSignInRequiredBody  = "issues.error.sign_in_body"
	IDIssuesCSRFTitle           = "issues.error.csrf_title"
	IDIssuesCSRFBody            = "issues.error.csrf_body"
	IDIssuesNoTokenTitle        = "issues.error.no_token_title"
	IDIssuesNoTokenBody         = "issues.error.no_token_body"

	IDTreeHeading        = "tree.heading"
	IDTreeEmptyDirectory = "tree.empty_directory"
	IDTreeParentLink     = "tree.parent_link"
	IDTreeSortNotice     = "tree.sort_notice"
	IDTreeColumnName     = "tree.column.name"
	IDTreeColumnSize     = "tree.column.size"

	IDEntryTypeFile      = "entry.type.file"
	IDEntryTypeDirectory = "entry.type.directory"
	IDEntryTypeSymlink   = "entry.type.symlink"
	IDEntryTypeSubmodule = "entry.type.submodule"

	IDFileHeading     = "file.heading"
	IDFileBinary      = "file.binary"
	IDFileBytesSuffix = "file.bytes_suffix"

	IDCommitsHeading      = "commits.heading"
	IDCommitsEmpty        = "commits.empty"
	IDCommitsFilteredBy   = "commits.filtered_by"
	IDCommitsLoadMoreHint = "commits.load_more_hint"
	IDCommitsNextPage     = "commits.next_page"
	IDCommitBy            = "commit.by"

	IDErrorNotFoundTitle = "error.not_found.title"
	IDErrorNotFoundBody  = "error.not_found.body"
	IDErrorUnavailable   = "error.unavailable.title"
	IDErrorUnavailableIn = "error.unavailable.body"

	// The application shell's own strings.
	//
	// They exist because the shell is served by the Go process and has to say
	// something before the client bundle has run — including "the frontend is not
	// built" on a checkout where it is not. The interactive components read their
	// strings from the same catalogue, fetched from `/api/v1/meta`, so a message
	// is written once and translated once (frontend.md, Internationalization).
	IDAppName                 = "app.name"
	IDAppLoading              = "app.loading"
	IDAppNotBuilt             = "app.not_built"
	IDAppNotBuiltHow          = "app.not_built.how"
	IDAppNoScript             = "app.no_script"
	IDAppCreateTitle          = "app.create.title"
	IDAppCreateName           = "app.create.name"
	IDAppCreateDescription    = "app.create.description"
	IDAppCreateVisibility     = "app.create.visibility"
	IDAppCreateVisibilityHint = "app.create.visibility_hint"
	IDAppCreateSubmit         = "app.create.submit"
	IDAppCreateWorking        = "app.create.working"
	IDAppCreated              = "app.create.created"
	IDAppSignInTitle          = "app.signin.title"
	IDAppSignInLogin          = "app.signin.login"
	IDAppSignInPassword       = "app.signin.password"
	IDAppSignInSubmit         = "app.signin.submit"
	IDAppSignInWorking        = "app.signin.working"
	IDAppSignedInAs           = "app.signed_in_as"
	IDAppSignOut              = "app.sign_out"
	IDAppSignInNeeded         = "app.signin.needed"
	IDAppErrorGeneric         = "app.error.generic"
	IDAppErrorNetwork         = "app.error.network"

	// The repository settings surface.
	IDAppSettingsTitle             = "app.settings.title"
	IDAppSettingsVisibility        = "app.settings.visibility"
	IDAppSettingsVisibilityHint    = "app.settings.visibility_hint"
	IDAppSettingsDefaultBranch     = "app.settings.default_branch"
	IDAppSettingsDefaultBranchHint = "app.settings.default_branch_hint"
	IDAppSettingsArchive           = "app.settings.archive"
	IDAppSettingsArchiveHint       = "app.settings.archive_hint"
	IDAppSettingsConfirmation      = "app.settings.confirmation"
	IDAppSettingsConfirmationHint  = "app.settings.confirmation_hint"
	IDAppSettingsSave              = "app.settings.save"
	IDAppSettingsSaving            = "app.settings.saving"
	IDAppSettingsSaved             = "app.settings.saved"
	IDAppSettingsCollaborators     = "app.settings.collaborators"
	IDAppSettingsCollaboratorsHint = "app.settings.collaborators_hint"
	IDAppSettingsCollaborator      = "app.settings.collaborator"
	IDAppSettingsRole              = "app.settings.role"
	IDAppSettingsAdd               = "app.settings.add"
	IDAppSettingsAdding            = "app.settings.adding"
	IDAppSettingsRemove            = "app.settings.remove"
	IDAppSettingsNoCollaborators   = "app.settings.no_collaborators"
	IDAppSettingsArchivedNotice    = "app.settings.archived_notice"
	IDAppSettingsLoadFailed        = "app.settings.load_failed"

	// The grant roles, which a reader sees in the collaborator list.
	IDRoleRead  = "role.read"
	IDRoleWrite = "role.write"
	IDRoleAdmin = "role.admin"
)

// enUS is the shipped catalogue.
//
// A message is a template with `%s`/`%d` placeholders where it takes arguments;
// Resolve applies them. Substitution rather than concatenation is the rule
// frontend.md states, because a sentence assembled from fragments cannot be
// reordered by a translator.
var enUS = map[string]string{
	IDSiteName: "GitNest",

	IDNavSkipToContent: "Skip to content",
	IDNavRepositories:  "Repositories",

	IDRepoVisibilityPrivate: "Private",
	IDRepoVisibilityPublic:  "Public",
	IDRepoNoDescription:     "No description",
	IDRepoYourPermission:    "Your permission: %s",

	IDIssuesHeading:    "Issues",
	IDIssuesEmpty:      "There are no issues to show.",
	IDIssuesFilterAll:  "All",
	IDIssuesFilterOpen: "Open",
	IDIssuesFilterShut: "Closed",
	IDIssuesNumber:     "#%d",
	IDIssuesStateOpen:  "Open",
	IDIssuesStateShut:  "Closed",
	IDIssuesBy:         "opened by",
	IDIssuesClosedBy:   "closed by",
	IDIssuesHistory:    "History",
	IDIssuesNoHistory:  "No changes have been recorded for this issue.",
	IDIssuesDeleted:    "This issue was deleted.",
	IDIssuesNotFound:   "There is no issue with that number.",
	IDIssuesMore:       "More issues",
	IDIssuesCreateHint: "Open a new issue from the repository's issue list.",

	IDMilestonesHeading:            "Milestones",
	IDMilestonesEmpty:              "This repository has no milestones.",
	IDMilestonesTitle:              "Title",
	IDMilestonesDescription:        "Description",
	IDMilestonesDue:                "Due date",
	IDMilestonesDueHint:            "A calendar date, as 2026-10-14. Leave it empty for no due date.",
	IDMilestonesCreate:             "Create a milestone",
	IDMilestonesCreateSubmit:       "Create the milestone",
	IDMilestonesStateOpen:          "Open",
	IDMilestonesStateClosed:        "Closed",
	IDMilestonesProgress:           "%d of %d issues closed, %d%%",
	IDMilestonesOverdue:            "overdue",
	IDMilestonesNoDue:              "No due date",
	IDMilestonesEdit:               "Edit this milestone",
	IDMilestonesSave:               "Save",
	IDMilestonesClose:              "Close the milestone",
	IDMilestonesReopen:             "Reopen the milestone",
	IDMilestonesDelete:             "Delete the milestone",
	IDMilestonesDeleteHint:         "Deleting a milestone removes it from every issue. The issues themselves are not deleted.",
	IDMilestonesOnIssue:            "Milestone",
	IDMilestonesNoneOnIssue:        "This issue is on no milestone.",
	IDMilestonesApply:              "Move to a milestone",
	IDMilestonesApplySubmit:        "Move",
	IDMilestonesRemove:             "Remove from the milestone",
	IDMilestonesManage:             "Manage this repository's milestones",
	IDMilestonesTitleInvalid:       "A milestone title must be between 1 and 255 characters, without a leading or trailing space.",
	IDMilestonesDueInvalid:         "A due date must be a calendar date, as 2026-10-14.",
	IDMilestonesDescriptionInvalid: "The milestone description is too long.",
	IDMilestonesTitleTaken:         "A milestone with that title already exists. Titles are compared without regard to case, and a closed milestone still holds its title.",
	IDMilestonesAlreadyInState:     "The milestone is already in that state.",
	IDMilestonesClosed:             "This milestone is closed, so it cannot take new issues. Reopen it first.",
	IDMilestonesForbidden:          "You do not have permission to change this repository's milestones.",
	IDMilestonesGone:               "That milestone no longer exists.",
	IDMilestonesUnknown:            "There is no milestone with that number in this repository.",
	IDMilestonesNotApplied:         "This issue is not on that milestone.",

	IDLabelsHeading:            "Labels",
	IDLabelsEmpty:              "This repository has no labels.",
	IDLabelsName:               "Name",
	IDLabelsColor:              "Color",
	IDLabelsDescription:        "Description",
	IDLabelsIssueCount:         "%d issues",
	IDLabelsCreate:             "Create a label",
	IDLabelsCreateSubmit:       "Create the label",
	IDLabelsDelete:             "Delete",
	IDLabelsDeleteHint:         "Deleting a label removes it from every issue. The issues themselves are not deleted.",
	IDLabelsColorHint:          "Six hexadecimal digits, for example ff0000 or #ff0000.",
	IDLabelsOnIssue:            "Labels",
	IDLabelsNoneOnIssue:        "This issue has no labels.",
	IDLabelsApply:              "Apply a label",
	IDLabelsApplySubmit:        "Apply",
	IDLabelsRemove:             "Remove",
	IDLabelsManage:             "Manage this repository's labels",
	IDLabelsNameInvalid:        "A label name must be between 1 and 50 characters, without a leading or trailing space.",
	IDLabelsColorInvalid:       "A label color must be six hexadecimal digits.",
	IDLabelsDescriptionInvalid: "A label description must be at most 100 characters.",
	IDLabelsNameTaken:          "A label with that name already exists. Names are compared without regard to case.",
	IDLabelsForbidden:          "You do not have permission to change this repository's labels.",
	IDLabelsGone:               "That label no longer exists.",
	IDLabelsUnknown:            "There is no label with that name in this repository.",
	IDLabelsNotApplied:         "This issue does not carry that label.",

	IDCommentsHeading:   "Comments",
	IDCommentsEmpty:     "A comment needs some text.",
	IDCommentsNone:      "No comments yet.",
	IDCommentsWrite:     "Add a comment",
	IDCommentsSubmit:    "Comment",
	IDCommentsBody:      "Comment",
	IDCommentsEdit:      "Edit this comment",
	IDCommentsSave:      "Save",
	IDCommentsDelete:    "Delete this comment",
	IDCommentsDeleted:   "This comment was deleted.",
	IDCommentsEdited:    "edited",
	IDCommentsHistory:   "Earlier versions",
	IDCommentsHistoryBy: "edited by %s",
	IDCommentsBy:        "%s wrote",
	IDCommentsSignIn:    "Sign in to comment.",
	IDCommentsForbidden: "You do not have permission to change this comment.",
	IDCommentsConflict:  "Somebody changed this comment while you were editing it, so your change was not applied. The text below is what is now stored.",
	IDCommentsNoChange:  "That change would make no difference to the comment.",
	IDCommentsGone:      "This comment no longer exists.",
	IDCommentsTooLong:   "The comment is too long.",

	IDIssuesNewLink:    "Open a new issue",
	IDIssuesNewTitle:   "Open a new issue",
	IDIssuesNewSubmit:  "Open the issue",
	IDIssuesNewWorking: "Opening…",
	IDIssuesFormTitle:  "Title",
	IDIssuesFormBody:   "Description",
	IDIssuesFormSave:   "Save",
	IDIssuesEdit:       "Edit this issue",
	IDIssuesClose:      "Close the issue",
	IDIssuesReopen:     "Reopen the issue",
	IDIssuesDelete:     "Delete the issue",
	IDIssuesDeleteHint: "Deleting removes the issue. The record of the change is kept in the audit log.",
	IDIssuesSaved:      "Your change was saved.",
	IDIssuesCancel:     "Cancel",

	IDIssuesTitleInvalid:        "The title must be between 1 and 256 characters.",
	IDIssuesBodyInvalid:         "The description is too long.",
	IDIssuesConflict:            "Somebody changed this issue while you were editing it, so your change was not applied. The text below is what is now stored; copy anything you need and edit again.",
	IDIssuesNoChange:            "That change would make no difference to the issue.",
	IDIssuesAlreadyInState:      "The issue is already in that state.",
	IDIssuesForbidden:           "You do not have permission to change this issue.",
	IDIssuesGone:                "This issue no longer exists.",
	IDIssuesTooLargeTitle:       "The submission was too large.",
	IDIssuesTooLargeBody:        "The form you submitted was larger than this instance accepts.",
	IDIssuesBadRequestTitle:     "That request could not be understood.",
	IDIssuesBadRequestBody:      "The form did not name an action this page offers.",
	IDIssuesSignInRequiredTitle: "Sign in to continue",
	IDIssuesSignInRequiredBody:  "Opening or changing an issue needs a signed-in account.",
	IDIssuesCSRFTitle:           "That form had expired",
	IDIssuesCSRFBody:            "This page was open for a while, or it was opened in another tab. Reload the page and try again.",
	IDIssuesNoTokenTitle:        "This instance cannot render that form",
	IDIssuesNoTokenBody:         "A form that cannot be submitted is not offered. Check the instance's configuration.",

	IDOverviewEmptyRepo:   "This repository is empty. Push to it to see content here.",
	IDOverviewNoReadme:    "This repository has no README at its default branch.",
	IDOverviewDefaultRef:  "Default branch: %s",
	IDOverviewCloneHint:   "Clone it with: git clone %s",
	IDOverviewTreeHeading: "Files",

	IDTreeHeading:        "Files at %s",
	IDTreeEmptyDirectory: "This directory is empty.",
	IDTreeParentLink:     "Parent directory",
	IDTreeSortNotice:     "Directories are listed before files, each in name order.",
	IDTreeColumnName:     "Name",
	IDTreeColumnSize:     "Size",

	IDEntryTypeFile:      "file",
	IDEntryTypeDirectory: "directory",
	IDEntryTypeSymlink:   "symbolic link",
	IDEntryTypeSubmodule: "submodule",

	IDFileHeading:     "%s at %s",
	IDFileBinary:      "This file is not valid UTF-8 text, so it is not shown here.",
	IDFileBytesSuffix: "%d bytes",

	IDCommitsHeading:      "Commits on %s",
	IDCommitsEmpty:        "No commits yet.",
	IDCommitsFilteredBy:   "Filtered to %s",
	IDCommitsLoadMoreHint: "There are more commits than this page shows.",
	IDCommitsNextPage:     "Next page",
	IDCommitBy:            "by %s on %s",

	IDErrorNotFoundTitle: "Not found",
	IDErrorNotFoundBody:  "There is nothing here, or you do not have access to it.",
	IDErrorUnavailable:   "Temporarily unavailable",
	IDErrorUnavailableIn: "The server could not read the repository just now.",

	IDAppName:                 "GitNest",
	IDAppLoading:              "Loading…",
	IDAppNotBuilt:             "The interactive interface has not been built.",
	IDAppNotBuiltHow:          "Run npm install and npm run build in the web directory, then rebuild the server.",
	IDAppNoScript:             "This page needs JavaScript for the interactive parts. The read-only pages of a repository work without it.",
	IDAppCreateTitle:          "New repository",
	IDAppCreateName:           "Repository name",
	IDAppCreateDescription:    "Description",
	IDAppCreateVisibility:     "Visibility",
	IDAppCreateVisibilityHint: "A repository is private unless you choose public. A public repository is readable by anyone, including anonymous visitors.",
	IDAppCreateSubmit:         "Create repository",
	IDAppCreateWorking:        "Creating…",
	IDAppCreated:              "Created %s",
	IDAppSignInTitle:          "Sign in",
	IDAppSignInLogin:          "Username or email",
	IDAppSignInPassword:       "Password",
	IDAppSignInSubmit:         "Sign in",
	IDAppSignInWorking:        "Signing in…",
	IDAppSignedInAs:           "Signed in as %s",
	IDAppSignOut:              "Sign out",
	IDAppSignInNeeded:         "Sign in to create a repository.",
	IDAppErrorGeneric:         "Something went wrong. Try again.",
	IDAppErrorNetwork:         "The server could not be reached.",

	IDAppSettingsTitle:             "Settings for %s",
	IDAppSettingsVisibility:        "Visibility",
	IDAppSettingsVisibilityHint:    "A repository is private unless you choose public. A public repository is readable by anyone, including anonymous visitors.",
	IDAppSettingsDefaultBranch:     "Default branch",
	IDAppSettingsDefaultBranchHint: "The branch a clone checks out and the repository page opens at.",
	IDAppSettingsArchive:           "Archive",
	IDAppSettingsArchiveHint:       "An archived repository is read-only: a push is refused and its files cannot change, while cloning and fetching still work.",
	IDAppSettingsConfirmation:      "Confirmation",
	IDAppSettingsConfirmationHint:  "Required when the visibility changes: type the repository's name to make it public, or type %s to make it private.",
	IDAppSettingsSave:              "Save changes",
	IDAppSettingsSaving:            "Saving…",
	IDAppSettingsSaved:             "Settings saved.",
	IDAppSettingsCollaborators:     "Collaborators",
	IDAppSettingsCollaboratorsHint: "Direct access, granted one account at a time. An administrator can change everything, a writer can push, and a reader can only fetch.",
	IDAppSettingsCollaborator:      "Account name",
	IDAppSettingsRole:              "Role",
	IDAppSettingsAdd:               "Add collaborator",
	IDAppSettingsAdding:            "Adding…",
	IDAppSettingsRemove:            "Remove %s",
	IDAppSettingsNoCollaborators:   "No collaborators yet. Only the owner has access.",
	IDAppSettingsArchivedNotice:    "This repository is archived, so its contents are read-only. Its settings can still be changed.",
	IDAppSettingsLoadFailed:        "The settings could not be loaded.",

	IDRoleRead:  "Read",
	IDRoleWrite: "Write",
	IDRoleAdmin: "Admin",
}

// catalogues holds every shipped locale. A locale absent here falls back to the
// default, which is what makes an unsupported `Accept-Language` harmless.
var catalogues = map[string]map[string]string{
	DefaultLocale: enUS,
}

// Catalogue resolves messages for one locale.
type Catalogue struct {
	locale   string
	messages map[string]string
}

// For returns the catalogue for a locale.
//
// An unknown locale resolves to the default rather than failing: a request
// carrying a locale the instance does not ship must still render.
func For(locale string) *Catalogue {
	normalized := Normalize(locale)
	if messages, found := catalogues[normalized]; found {
		return &Catalogue{locale: normalized, messages: messages}
	}
	return &Catalogue{locale: DefaultLocale, messages: catalogues[DefaultLocale]}
}

// Default returns the catalogue for the instance default.
func Default() *Catalogue { return For(DefaultLocale) }

// Locale returns the locale this catalogue resolved to.
func (c *Catalogue) Locale() string { return c.locale }

// T resolves a message identifier, applying any arguments.
//
// A missing identifier returns the identifier itself rather than an empty
// string, so a template that names an unregistered message renders something an
// operator can search for instead of a blank space that looks like a layout bug.
func (c *Catalogue) T(id string, args ...any) string {
	message, found := c.messages[id]
	if !found {
		// Falling back to the default catalogue covers a locale that is complete
		// except for a message added after it was written.
		message, found = catalogues[DefaultLocale][id]
		if !found {
			return id
		}
	}
	if len(args) == 0 {
		return message
	}
	return fmt.Sprintf(message, args...)
}

// Locales lists the shipped locales, for a settings page or a doctor command.
func Locales() []string {
	names := make([]string, 0, len(catalogues))
	for name := range catalogues {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Identifiers lists every message identifier, so the hardcoded-string check can
// assert that a template only ever names a registered one.
func Identifiers() []string {
	seen := map[string]struct{}{}
	for _, messages := range catalogues {
		for id := range messages {
			seen[id] = struct{}{}
		}
	}
	identifiers := make([]string, 0, len(seen))
	for id := range seen {
		identifiers = append(identifiers, id)
	}
	sort.Strings(identifiers)
	return identifiers
}

// Normalize returns the comparison form of a locale tag.
//
// The language and the region are compared case-insensitively and the region is
// preserved, so `en-us`, `EN-US`, and `en-US` all resolve to the same catalogue.
func Normalize(locale string) string {
	trimmed := strings.TrimSpace(locale)
	if trimmed == "" {
		return DefaultLocale
	}
	// Only the first language tag matters here: a quality-weighted list has
	// already been resolved to one tag by the caller.
	if index := strings.IndexAny(trimmed, ",;"); index >= 0 {
		trimmed = trimmed[:index]
	}
	parts := strings.Split(trimmed, "-")
	if len(parts) == 1 {
		return strings.ToLower(parts[0])
	}
	language := strings.ToLower(parts[0])
	region := parts[1]
	if language == "en" && strings.EqualFold(region, "us") {
		return DefaultLocale
	}
	return language + "-" + strings.ToUpper(region)
}

// SelectFromAcceptLanguage picks a locale from an `Accept-Language` header.
//
// frontend.md fixes the precedence as the account setting first, then the
// request header, then the instance default. This function is the middle step,
// so it takes only the header.
//
// It deliberately does not parse quality weights: it takes the first tag the
// client listed that the instance ships, and otherwise the default. A
// quality-weighted negotiation is worth writing when a second locale ships and
// the ordering actually matters.
func SelectFromAcceptLanguage(header string) string {
	for _, entry := range strings.Split(header, ",") {
		tag := Normalize(entry)
		if tag == "" {
			continue
		}
		if _, found := catalogues[tag]; found {
			return tag
		}
	}
	return DefaultLocale
}
