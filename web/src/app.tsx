import { useState , type ReactElement } from 'react'
import { setCSRFToken } from './api'
import type { BootPayload } from './boot'
import { CreateRepository } from './features/create'
import { RepositorySettings } from './features/settings'
import { SignIn } from './features/signin'
import { createTranslator } from './messages'

export interface AppProps {
  readonly boot: BootPayload
}

/**
 * App is the root of one interactive surface.
 *
 * The session is tracked here rather than in each feature, because signing in
 * changes what every surface may do: a create form without a session is a form
 * that cannot work, and it is the shell's job to know that.
 */
export function App({ boot }: AppProps): ReactElement {
  const t = createTranslator(boot.messages)
  const [account, setAccount] = useState(boot.account)

  function signedIn(username: string, csrfToken: string): void {
    setCSRFToken(csrfToken)
    setAccount(username)
  }

  function signedOut(): void {
    setCSRFToken(undefined)
    setAccount(undefined)
  }

  return (
    <div className="app">
      <header className="app-header">
        <h1>{account ? t('app.signed_in_as', account) : t('app.name')}</h1>
      </header>

      {boot.area === 'create' ? (
        <>
          {account ? null : <SignIn t={t} onSignedIn={signedIn} />}
          <CreateRepository t={t} signedIn={account !== undefined} {...(account ? { account } : {})} />
          {account ? (
            <p>
              <button type="button" onClick={signedOut}>
                {t('app.sign_out')}
              </button>
            </p>
          ) : null}
        </>
      ) : null}

      {boot.area === 'settings' ? (
        <>
          {account ? null : <SignIn t={t} onSignedIn={signedIn} />}
          <RepositorySettings
            t={t}
            signedIn={account !== undefined}
            owner={boot.owner ?? ''}
            repo={boot.repo ?? ''}
            confirmationWord={boot.confirmation_word ?? ''}
          />
          {account ? (
            <p>
              <button type="button" onClick={signedOut}>
                {t('app.sign_out')}
              </button>
            </p>
          ) : null}
        </>
      ) : null}
    </div>
  )
}