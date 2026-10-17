import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import axe from 'axe-core'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RepositorySettings } from './settings'
import type { Translate } from '../messages'
import { createTranslator } from '../messages'

/** The messages a shell would send for the settings area. */
const messages: Record<string, string> = {
  'app.name': 'GitNest',
  'app.settings.title': 'Settings for %s',
  'app.settings.visibility': 'Visibility',
  'app.settings.visibility_hint': 'A repository is private unless you choose public.',
  'app.settings.default_branch': 'Default branch',
  'app.settings.default_branch_hint': 'The branch a clone checks out.',
  'app.settings.archive': 'Archive',
  'app.settings.archive_hint': 'An archived repository is read-only.',
  'app.settings.confirmation': 'Confirmation',
  'app.settings.confirmation_hint': 'Type the name to make it public, or %s to make it private.',
  'app.settings.save': 'Save changes',
  'app.settings.saving': 'Saving…',
  'app.settings.saved': 'Settings saved.',
  'app.settings.collaborators': 'Collaborators',
  'app.settings.collaborators_hint': 'Direct access, granted one account at a time.',
  'app.settings.collaborator': 'Account name',
  'app.settings.role': 'Role',
  'app.settings.add': 'Add collaborator',
  'app.settings.adding': 'Adding…',
  'app.settings.remove': 'Remove %s',
  'app.settings.no_collaborators': 'No collaborators yet.',
  'app.settings.archived_notice': 'This repository is archived, so its contents are read-only.',
  'app.settings.load_failed': 'The settings could not be loaded.',
  'app.signin.needed': 'Sign in to create a repository.',
  'app.error.generic': 'Something went wrong. Try again.',
  'app.error.network': 'The server could not be reached.',
  'repo.visibility.private': 'Private',
  'repo.visibility.public': 'Public',
  'role.read': 'Read',
  'role.write': 'Write',
  'role.admin': 'Admin',
}

const t: Translate = createTranslator(messages)

/** A fetch double answering in order and recording what it was asked. */
interface Call {
  url: string
  method?: string
  body?: unknown
  headers?: Record<string, string>
}

function stubFetch(responses: { status: number; body: unknown }[]): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({
        url: String(input),
        ...(init?.method ? { method: init.method } : {}),
        ...(init?.body ? { body: JSON.parse(String(init.body)) } : {}),
        headers: (init?.headers ?? {}) as Record<string, string>,
      })
      const next = responses.shift()
      if (!next) throw new Error('no scripted response left')
      return new Response(next.status === 204 ? null : JSON.stringify(next.body), {
        status: next.status,
        headers: { 'Content-Type': 'application/json' },
      })
    }),
  )
  return calls
}

const repository = {
  full_name: 'alice/settings-repo',
  visibility: 'private',
  default_branch: 'main',
  archived: false,
}

function renderSettings(): void {
  render(
    <RepositorySettings
      t={t}
      signedIn
      owner="alice"
      repo="settings-repo"
      confirmationWord="change-visibility"
    />,
  )
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the settings surface', () => {
  it('loads the repository and its collaborators', async () => {
    stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [{ login: 'bob', role_name: 'write' }] },
    ])
    renderSettings()

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Settings for alice/settings-repo' })).toBeInTheDocument())
    expect(screen.getByLabelText('Default branch')).toHaveValue('main')
    expect(screen.getByRole('radio', { name: 'Private' })).toBeChecked()

    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    // The role is rendered from the catalogue rather than as the API's value.
    expect(screen.getByText('Write')).toBeInTheDocument()
  })

  it('submits the settings and reports the saved state', async () => {
    const calls = stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
      { status: 200, body: { ...repository, default_branch: 'develop' } },
    ])
    const user = userEvent.setup()
    renderSettings()

    await waitFor(() => expect(screen.getByLabelText('Default branch')).toHaveValue('main'))
    await user.clear(screen.getByLabelText('Default branch'))
    await user.type(screen.getByLabelText('Default branch'), 'develop')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Settings saved.'))
    const patch = calls.find((call) => call.method === 'PATCH')
    expect(patch?.body).toMatchObject({ default_branch: 'develop', visibility: 'private' })
  })

  it('attaches the CSRF token to a state-changing request', async () => {
    const calls = stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
      { status: 200, body: repository },
    ])
    // The app root sets the token after sign-in; here it is set directly.
    const { setCSRFToken } = await import('../api')
    setCSRFToken('a-token')

    const user = userEvent.setup()
    renderSettings()
    await waitFor(() => expect(screen.getByLabelText('Default branch')).toHaveValue('main'))
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())

    const patch = calls.find((call) => call.method === 'PATCH')
    expect(patch?.headers?.['X-CSRF-Token']).toBe('a-token')
  })

  it('shows a refusal against the field the API named', async () => {
    stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
      {
        status: 422,
        body: {
          message: 'the request is not valid',
          errors: [{ field: 'confirmation', code: 'confirmation_required', message: 'supply the name' }],
        },
      },
    ])
    const user = userEvent.setup()
    renderSettings()

    await waitFor(() => expect(screen.getByLabelText('Default branch')).toHaveValue('main'))
    await user.click(screen.getByRole('radio', { name: 'Public' }))
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(screen.getByText('supply the name')).toBeInTheDocument())
    // The field carries the state too, so a screen reader reports it on the input
    // rather than only in a banner.
    expect(screen.getByLabelText('Confirmation')).toHaveAttribute('aria-invalid', 'true')
  })

  it('takes the confirmation word from the server rather than its own copy', () => {
    stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
    ])
    renderSettings()

    // The hint is rendered from the catalogue with the server's word substituted.
    expect(screen.getByText(/change-visibility/)).toBeInTheDocument()
  })

  it('grants and revokes a collaborator', async () => {
    const calls = stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
      { status: 201, body: { login: 'bob', role_name: 'read' } },
      { status: 200, body: [{ login: 'bob', role_name: 'read' }] },
      { status: 204, body: null },
      { status: 200, body: [] },
    ])
    const user = userEvent.setup()
    renderSettings()

    await waitFor(() => expect(screen.getByText('No collaborators yet.')).toBeInTheDocument())
    await user.type(screen.getByLabelText('Account name'), 'bob')
    await user.click(screen.getByRole('button', { name: 'Add collaborator' }))

    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    const put = calls.find((call) => call.method === 'PUT')
    expect(put?.url).toContain('/collaborators/bob')
    expect(put?.body).toEqual({ role_name: 'read' })

    await user.click(screen.getByRole('button', { name: 'Remove bob' }))
    await waitFor(() => expect(screen.getByText('No collaborators yet.')).toBeInTheDocument())
    expect(calls.some((call) => call.method === 'DELETE')).toBe(true)
  })

  it('says a repository is read-only when it is archived', async () => {
    stubFetch([
      { status: 200, body: { ...repository, archived: true } },
      { status: 200, body: [] },
    ])
    renderSettings()

    await waitFor(() =>
      expect(screen.getByText(/archived, so its contents are read-only/)).toBeInTheDocument(),
    )
    expect(screen.getByLabelText('Archive')).toBeChecked()
  })

  it('asks for a sign-in when the shell reports no session', () => {
    stubFetch([])
    render(
      <RepositorySettings
        t={t}
        signedIn={false}
        owner="alice"
        repo="settings-repo"
        confirmationWord="change-visibility"
      />,
    )
    expect(screen.getByText('Sign in to create a repository.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument()
  })

  it('is keyboard operable: change the branch and save', async () => {
    stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [] },
      { status: 200, body: { ...repository, default_branch: 'release' } },
    ])
    const user = userEvent.setup()
    renderSettings()

    await waitFor(() => expect(screen.getByLabelText('Default branch')).toHaveValue('main'))

    // Everything below is reachable and operable without a pointer.
    const branch = screen.getByLabelText('Default branch')
    branch.focus()
    await user.keyboard('{Control>}a{/Control}release')
    await user.tab()
    expect(branch).toHaveValue('release')

    // Walk to the submit control and activate it from the keyboard.
    const save = screen.getByRole('button', { name: 'Save changes' })
    save.focus()
    expect(save).toHaveFocus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Settings saved.'))
  })
})

describe('accessibility', () => {
  it('has no automatically detectable violations', async () => {
    stubFetch([
      { status: 200, body: repository },
      { status: 200, body: [{ login: 'bob', role_name: 'admin' }] },
    ])
    const { container } = render(
      <RepositorySettings
        t={t}
        signedIn
        owner="alice"
        repo="settings-repo"
        confirmationWord="change-visibility"
      />,
    )
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())

    const results = await axe.run(container)
    expect(results.violations).toEqual([])
  })
})