import { useId, useRef, useState , type ReactElement } from 'react'
import { ApiError, request, setCSRFToken } from '../api'
import type { Translate } from '../messages'

/** The sign-in response, as the account contract returns it. */
interface SignInResponse {
  user: { username: string }
  csrf_token: string
}

export interface SignInProps {
  readonly t: Translate
  readonly onSignedIn: (username: string, csrfToken: string) => void
}

/**
 * SignIn is the account form.
 *
 * Accessibility is a release gate, not an aspiration (`frontend.md`,
 * Accessibility). What that means concretely here:
 *
 *   - every control has a label associated by `htmlFor`, so it is announced;
 *   - the failure is announced through a live region and tied to the form, with
 *     no reliance on colour;
 *   - the submit control reports that it is working and is disabled while it is,
 *     so a keyboard user cannot submit twice;
 *   - focus moves to the error summary on failure, so a screen-reader user is not
 *     left at the bottom of the form wondering what happened.
 */
export function SignIn({ t, onSignedIn }: SignInProps): ReactElement {
  const loginId = useId()
  const passwordId = useId()
  const errorId = useId()
  const errorRef = useRef<HTMLDivElement>(null)

  const [login, setLogin] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  async function submit(event: React.FormEvent): Promise<void> {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const response = await request<SignInResponse>('/api/v1/login', {
        method: 'POST',
        body: { login, password },
      })
      setCSRFToken(response.csrf_token)
      onSignedIn(response.user.username, response.csrf_token)
    } catch (cause) {
      // The API deliberately does not distinguish an unknown account from a wrong
      // password, so the surface does not either: it repeats what it was told.
      setError(cause instanceof ApiError && cause.status !== 0 ? cause.message : t('app.error.network'))
      // Focus the summary rather than the field, so the message is read before
      // the input it belongs to.
      queueMicrotask(() => errorRef.current?.focus())
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} aria-describedby={error ? errorId : undefined} noValidate>
      <h2>{t('app.signin.title')}</h2>

      {error ? (
        <div id={errorId} ref={errorRef} role="alert" tabIndex={-1} className="error-summary">
          {error}
        </div>
      ) : null}

      <p>
        <label htmlFor={loginId}>{t('app.signin.login')}</label>
        <input
          id={loginId}
          name="login"
          type="text"
          autoComplete="username"
          required
          value={login}
          onChange={(event) => setLogin(event.target.value)}
        />
      </p>

      <p>
        <label htmlFor={passwordId}>{t('app.signin.password')}</label>
        <input
          id={passwordId}
          name="password"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </p>

      <p>
        <button type="submit" disabled={busy} aria-busy={busy}>
          {busy ? t('app.signin.working') : t('app.signin.submit')}
        </button>
      </p>
    </form>
  )
}