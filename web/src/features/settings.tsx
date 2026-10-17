import { useEffect, useId, useRef, useState, type ReactElement } from 'react'
import { ApiError, request } from '../api'
import type { Translate } from '../messages'

/** The repository shape the API returns, as far as this surface reads it. */
interface RepositoryResponse {
  full_name: string
  visibility: 'private' | 'public'
  default_branch: string
  archived: boolean
}

/** One direct grant, as a collaborator listing returns it. */
interface CollaboratorResponse {
  login: string
  role_name: string
}

const ROLES = ['read', 'write', 'admin'] as const
type Role = (typeof ROLES)[number]

export interface SettingsProps {
  readonly t: Translate
  /** Whether the caller holds a session. Every call here needs one. */
  readonly signedIn: boolean
  /** The repository this surface acts on, from the path the server routed on. */
  readonly owner: string
  readonly repo: string
  /**
   * The confirmation the server requires for a reducing transition. It is passed
   * in rather than written here, because the server owns the rule.
   */
  readonly confirmationWord: string
}

/**
 * RepositorySettings is the repository settings surface.
 *
 * It reads the repository and its collaborators, then submits changes to the
 * settings endpoint. Three things are deliberate:
 *
 *   - **The confirmation is a single field whose meaning depends on the
 *     transition.** Making a repository public requires its name, because that is
 *     the direction that can expose content; making it private requires the
 *     acknowledgement word. The hint says which, and the server refuses a change
 *     whose confirmation does not match — the client never decides.
 *   - **A field-level refusal is shown against its field.** The API returns an
 *     `errors` array naming each rejected field, and a form that collapsed that
 *     into one banner would make the reader hunt for which input was wrong.
 *   - **Every control is labelled and the failure is announced.** The
 *     accessibility checks are a release gate, not a preference.
 */
export function RepositorySettings({ t, signedIn, owner, repo, confirmationWord }: SettingsProps): ReactElement {
  const visibilityId = useId()
  const branchId = useId()
  const archivedId = useId()
  const confirmationId = useId()
  const clueId = useId()
  const errorId = useId()
  const statusId = useId()
  const collaboratorId = useId()
  const roleId = useId()
  const errorRef = useRef<HTMLDivElement>(null)

  const [repository, setRepository] = useState<RepositoryResponse>()
  const [collaborators, setCollaborators] = useState<CollaboratorResponse[]>([])
  const [loadError, setLoadError] = useState(false)

  const [visibility, setVisibility] = useState<'private' | 'public'>('private')
  const [branch, setBranch] = useState('')
  const [archived, setArchived] = useState(false)
  const [confirmation, setConfirmation] = useState('')

  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string>()
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})

  const [collaborator, setCollaborator] = useState('')
  const [role, setRole] = useState<Role>('read')
  const [adding, setAdding] = useState(false)

  const base = `/api/v1/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`

  // The repository and its grants are read once. A settings form is not a live
  // view, so it does not poll; it re-reads after a change instead.
  useEffect(() => {
    if (!signedIn) return
    let cancelled = false

    async function load(): Promise<void> {
      try {
        const loaded = await request<RepositoryResponse>(base)
        if (cancelled) return
        setRepository(loaded)
        setVisibility(loaded.visibility)
        setBranch(loaded.default_branch)
        setArchived(loaded.archived)
      } catch {
        if (!cancelled) setLoadError(true)
        return
      }
      try {
        const grants = await request<CollaboratorResponse[]>(`${base}/collaborators`)
        if (!cancelled) setCollaborators(grants)
      } catch (cause) {
        // A caller who may change settings but not list grants is possible in
        // principle; the settings themselves are still usable, so the listing
        // failure is reported in its own section rather than replacing the form.
        if (!cancelled && cause instanceof ApiError && cause.status !== 403) setLoadError(true)
      }
    }
    void load()
    return () => {
      cancelled = true
    }
  }, [base, signedIn])

  if (!signedIn) {
    return (
      <section>
        <h2>{t('app.settings.title', `${owner}/${repo}`)}</h2>
        <p>{t('app.signin.needed')}</p>
      </section>
    )
  }

  function applyErrors(cause: ApiError): void {
    const perField: Record<string, string> = {}
    for (const problem of cause.body.errors ?? []) {
      if (problem.field) perField[problem.field] = problem.message ?? ''
    }
    setFieldErrors(perField)
    setError(cause.status === 0 ? t('app.error.network') : cause.message)
    queueMicrotask(() => errorRef.current?.focus())
  }

  async function save(event: React.FormEvent): Promise<void> {
    event.preventDefault()
    setSaving(true)
    setSaved(false)
    setError(undefined)
    setFieldErrors({})
    try {
      const updated = await request<RepositoryResponse>(base, {
        method: 'PATCH',
        body: {
          visibility,
          default_branch: branch,
          archived,
          confirmation,
        },
      })
      setRepository(updated)
      setVisibility(updated.visibility)
      setBranch(updated.default_branch)
      setArchived(updated.archived)
      setConfirmation('')
      setSaved(true)
    } catch (cause) {
      if (cause instanceof ApiError) applyErrors(cause)
      else setError(t('app.error.generic'))
    } finally {
      setSaving(false)
    }
  }

  async function addCollaborator(event: React.FormEvent): Promise<void> {
    event.preventDefault()
    setAdding(true)
    setError(undefined)
    setFieldErrors({})
    try {
      await request<CollaboratorResponse>(
        `${base}/collaborators/${encodeURIComponent(collaborator)}`,
        { method: 'PUT', body: { role_name: role } },
      )
      const grants = await request<CollaboratorResponse[]>(`${base}/collaborators`)
      setCollaborators(grants)
      setCollaborator('')
    } catch (cause) {
      if (cause instanceof ApiError) applyErrors(cause)
      else setError(t('app.error.generic'))
    } finally {
      setAdding(false)
    }
  }

  async function removeCollaborator(login: string): Promise<void> {
    setError(undefined)
    setFieldErrors({})
    try {
      await request<void>(`${base}/collaborators/${encodeURIComponent(login)}`, { method: 'DELETE' })
      const grants = await request<CollaboratorResponse[]>(`${base}/collaborators`)
      setCollaborators(grants)
    } catch (cause) {
      if (cause instanceof ApiError) applyErrors(cause)
      else setError(t('app.error.generic'))
    }
  }

  const title = t('app.settings.title', `${owner}/${repo}`)

  return (
    <section>
      <h2>{title}</h2>

      {loadError ? (
        <div role="alert" className="error-summary">
          {t('app.settings.load_failed')}
        </div>
      ) : null}

      {error ? (
        <div id={errorId} ref={errorRef} role="alert" tabIndex={-1} className="error-summary">
          {error}
        </div>
      ) : null}
      {saved ? (
        <p id={statusId} role="status" className="status">
          {t('app.settings.saved')}
        </p>
      ) : null}

      {repository?.archived ? <p role="status">{t('app.settings.archived_notice')}</p> : null}

      <form onSubmit={save} aria-describedby={error ? errorId : undefined} noValidate>
        <fieldset>
          <legend>{t('app.settings.visibility')}</legend>
          <p id={clueId} className="hint">
            {t('app.settings.visibility_hint')}
          </p>
          <p>
            <input
              id={`${visibilityId}-private`}
              type="radio"
              name="visibility"
              value="private"
              checked={visibility === 'private'}
              onChange={() => setVisibility('private')}
            />
            <label htmlFor={`${visibilityId}-private`}>{t('repo.visibility.private')}</label>
          </p>
          <p>
            <input
              id={`${visibilityId}-public`}
              type="radio"
              name="visibility"
              value="public"
              checked={visibility === 'public'}
              onChange={() => setVisibility('public')}
              aria-describedby={clueId}
            />
            <label htmlFor={`${visibilityId}-public`}>{t('repo.visibility.public')}</label>
          </p>
        </fieldset>

        <p>
          <label htmlFor={branchId}>{t('app.settings.default_branch')}</label>
          <input
            id={branchId}
            name="default_branch"
            type="text"
            value={branch}
            onChange={(event) => setBranch(event.target.value)}
            aria-describedby={fieldErrors['default_branch'] ? `${branchId}-error` : undefined}
            aria-invalid={fieldErrors['default_branch'] ? true : undefined}
          />
          <span className="hint">{t('app.settings.default_branch_hint')}</span>
          {fieldErrors['default_branch'] ? (
            <span id={`${branchId}-error`} className="field-error">
              {fieldErrors['default_branch']}
            </span>
          ) : null}
        </p>

        <p>
          <input
            id={archivedId}
            name="archived"
            type="checkbox"
            checked={archived}
            onChange={(event) => setArchived(event.target.checked)}
          />
          <label htmlFor={archivedId}>{t('app.settings.archive')}</label>
          <span className="hint">{t('app.settings.archive_hint')}</span>
        </p>

        <p>
          <label htmlFor={confirmationId}>{t('app.settings.confirmation')}</label>
          <input
            id={confirmationId}
            name="confirmation"
            type="text"
            value={confirmation}
            onChange={(event) => setConfirmation(event.target.value)}
            aria-describedby={
              fieldErrors['confirmation'] ? `${confirmationId}-error` : `${confirmationId}-hint`
            }
            aria-invalid={fieldErrors['confirmation'] ? true : undefined}
          />
          <span id={`${confirmationId}-hint`} className="hint">
            {t('app.settings.confirmation_hint', confirmationWord)}
          </span>
          {fieldErrors['confirmation'] ? (
            <span id={`${confirmationId}-error`} className="field-error">
              {fieldErrors['confirmation']}
            </span>
          ) : null}
        </p>

        <p>
          <button type="submit" disabled={saving} aria-busy={saving}>
            {saving ? t('app.settings.saving') : t('app.settings.save')}
          </button>
        </p>
      </form>

      <hr />

      <h3>{t('app.settings.collaborators')}</h3>
      <p className="hint">{t('app.settings.collaborators_hint')}</p>

      {collaborators.length === 0 ? (
        <p>{t('app.settings.no_collaborators')}</p>
      ) : (
        <ul className="collaborators">
          {collaborators.map((grant) => (
            <li key={grant.login}>
              <span className="collaborator-name">{grant.login}</span>
              <span className="collaborator-role">{t(`role.${grant.role_name}`)}</span>
              <button type="button" onClick={() => void removeCollaborator(grant.login)}>
                {t('app.settings.remove', grant.login)}
              </button>
            </li>
          ))}
        </ul>
      )}

      <form onSubmit={addCollaborator}>
        <p>
          <label htmlFor={collaboratorId}>{t('app.settings.collaborator')}</label>
          <input
            id={collaboratorId}
            name="collaborator"
            type="text"
            required
            value={collaborator}
            onChange={(event) => setCollaborator(event.target.value)}
            aria-invalid={fieldErrors['login'] ? true : undefined}
          />
        </p>
        <p>
          <label htmlFor={roleId}>{t('app.settings.role')}</label>
          <select id={roleId} name="role" value={role} onChange={(event) => setRole(event.target.value as Role)}>
            {ROLES.map((option) => (
              <option key={option} value={option}>
                {t(`role.${option}`)}
              </option>
            ))}
          </select>
        </p>
        <p>
          <button type="submit" disabled={adding} aria-busy={adding}>
            {adding ? t('app.settings.adding') : t('app.settings.add')}
          </button>
        </p>
      </form>
    </section>
  )
}