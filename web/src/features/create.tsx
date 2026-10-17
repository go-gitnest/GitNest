import { useId, useRef, useState , type ReactElement } from 'react'
import { ApiError, request } from '../api'
import type { Translate } from '../messages'

/** The repository a create call returns. */
interface RepositoryResponse {
  full_name: string
}

export interface CreateProps {
  readonly t: Translate
  /** Whether the caller holds a session, which a create requires. */
  readonly signedIn: boolean
  /** The account name, shown so the reader knows where the repository will live. */
  readonly account?: string
}

/**
 * CreateRepository is the repository creation form.
 *
 * It is private by default, which is the product rule: a repository is public
 * only when its owner chooses so. The control is a pair of radios rather than a
 * checkbox, so neither state is implied by the absence of a mark.
 *
 * A creation requires a session and a CSRF token. When the shell reports no
 * session the form is replaced by a sentence saying so, rather than being shown
 * and failing on submit: a form that cannot work is worse than a form that is
 * not there.
 */
export function CreateRepository({ t, signedIn, account }: CreateProps): ReactElement {
  const nameId = useId()
  const descriptionId = useId()
  const clueId = useId()
  const errorId = useId()
  const nameFieldErrorId = useId()
  const errorRef = useRef<HTMLDivElement>(null)

  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [visibility, setVisibility] = useState<'private' | 'public'>('private')
  const [error, setError] = useState<string>()
  const [nameError, setNameError] = useState<string>()
  const [created, setCreated] = useState<string>()
  const [busy, setBusy] = useState(false)

  if (!signedIn) {
    return (
      <section>
        <h2>{t('app.create.title')}</h2>
        <p>{t('app.signin.needed')}</p>
      </section>
    )
  }

  async function submit(event: React.FormEvent): Promise<void> {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    setNameError(undefined)
    setCreated(undefined)
    try {
      const repository = await request<RepositoryResponse>('/api/v1/user/repos', {
        method: 'POST',
        body: { name, description, visibility },
      })
      setCreated(t('app.create.created', repository.full_name))
      setName('')
      setDescription('')
    } catch (cause) {
      if (cause instanceof ApiError) {
        // A field-level failure is attached to its field rather than only
        // summarised, which is what the envelope's `errors` array is for.
        setNameError(cause.fieldError('name'))
        setError(cause.status === 0 ? t('app.error.network') : cause.message)
      } else {
        setError(t('app.error.generic'))
      }
      queueMicrotask(() => errorRef.current?.focus())
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <h2>{t('app.create.title')}</h2>
      {account ? <p>{t('app.signed_in_as', account)}</p> : null}

      {error ? (
        <div id={errorId} ref={errorRef} role="alert" tabIndex={-1} className="error-summary">
          {error}
        </div>
      ) : null}
      {created ? (
        <p role="status" className="status">
          {created}
        </p>
      ) : null}

      <form onSubmit={submit} aria-describedby={error ? errorId : undefined} noValidate>
        <p>
          <label htmlFor={nameId}>{t('app.create.name')}</label>
          <input
            id={nameId}
            name="name"
            type="text"
            required
            value={name}
            onChange={(event) => setName(event.target.value)}
            aria-describedby={nameError ? nameFieldErrorId : undefined}
            aria-invalid={nameError ? true : undefined}
          />
          {nameError ? (
            <span id={nameFieldErrorId} className="field-error">
              {nameError}
            </span>
          ) : null}
        </p>

        <p>
          <label htmlFor={descriptionId}>{t('app.create.description')}</label>
          <input
            id={descriptionId}
            name="description"
            type="text"
            value={description}
            onChange={(event) => setDescription(event.target.value)}
          />
        </p>

        <fieldset>
          <legend>{t('app.create.visibility')}</legend>
          <p id={clueId} className="hint">
            {t('app.create.visibility_hint')}
          </p>
          <p>
            <input
              id={`${nameId}-private`}
              type="radio"
              name="visibility"
              value="private"
              checked={visibility === 'private'}
              onChange={() => setVisibility('private')}
            />
            <label htmlFor={`${nameId}-private`}>{t('repo.visibility.private')}</label>
          </p>
          <p>
            <input
              id={`${nameId}-public`}
              type="radio"
              name="visibility"
              value="public"
              checked={visibility === 'public'}
              onChange={() => setVisibility('public')}
              aria-describedby={clueId}
            />
            <label htmlFor={`${nameId}-public`}>{t('repo.visibility.public')}</label>
          </p>
        </fieldset>

        <p>
          <button type="submit" disabled={busy} aria-busy={busy}>
            {busy ? t('app.create.working') : t('app.create.submit')}
          </button>
        </p>
      </form>
    </section>
  )
}