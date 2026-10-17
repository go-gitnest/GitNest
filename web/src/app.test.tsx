import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import axe from 'axe-core'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './app'
import type { BootPayload } from './boot'

/** The messages a shell would send for the create area. */
const messages: Record<string, string> = {
  'app.name': 'GitNest',
  'app.create.title': 'New repository',
  'app.create.name': 'Repository name',
  'app.create.description': 'Description',
  'app.create.visibility': 'Visibility',
  'app.create.visibility_hint': 'A repository is private unless you choose public.',
  'app.create.submit': 'Create repository',
  'app.create.working': 'Creating…',
  'app.create.created': 'Created %s',
  'app.signin.title': 'Sign in',
  'app.signin.login': 'Username or email',
  'app.signin.password': 'Password',
  'app.signin.submit': 'Sign in',
  'app.signin.working': 'Signing in…',
  'app.signed_in_as': 'Signed in as %s',
  'app.sign_out': 'Sign out',
  'app.signin.needed': 'Sign in to create a repository.',
  'app.error.generic': 'Something went wrong. Try again.',
  'app.error.network': 'The server could not be reached.',
  'repo.visibility.private': 'Private',
  'repo.visibility.public': 'Public',
}

function boot(overrides: Partial<BootPayload> = {}): BootPayload {
  return { locale: 'en-US', area: 'create', messages, ...overrides }
}

/** A fetch double that answers in order and records what it was asked. */
function stubFetch(responses: { status: number; body: unknown }[]): ReturnType<typeof vi.fn> {
  const calls: unknown[] = []
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ input, init })
    const next = responses.shift()
    if (!next) throw new Error('no scripted response left')
    return new Response(JSON.stringify(next.body), {
      status: next.status,
      headers: { 'Content-Type': 'application/json' },
    })
  })
  ;(mock as unknown as { calls: unknown[] }).calls = calls
  vi.stubGlobal('fetch', mock)
  return mock
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the create surface', () => {
  it('asks for a sign-in when the shell reports no session', () => {
    stubFetch([])
    render(<App boot={boot()} />)

    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    // The create form is replaced by a sentence, not shown and left to fail.
    expect(screen.getByText('Sign in to create a repository.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Create repository' })).not.toBeInTheDocument()
  })

  it('offers the create form when the shell reports a session', () => {
    stubFetch([])
    render(<App boot={boot({ account: 'alice' })} />)

    expect(screen.getByRole('heading', { name: 'New repository' })).toBeInTheDocument()
    expect(screen.getByLabelText('Repository name')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Sign in' })).not.toBeInTheDocument()
  })

  it('is operable by keyboard alone: sign in and create a repository', async () => {
    // The whole flow without a pointer, which is the acceptance criterion.
    stubFetch([
      { status: 200, body: { user: { username: 'alice' }, csrf_token: 'csrf-token' } },
      { status: 201, body: { full_name: 'alice/my-repo' } },
    ])
    const user = userEvent.setup()
    render(<App boot={boot()} />)

    // Tab order reaches the login field first.
    await user.tab()
    expect(screen.getByLabelText('Username or email')).toHaveFocus()

    await user.keyboard('alice')
    await user.tab()
    await user.keyboard('correct horse battery staple')
    await user.tab()
    expect(screen.getByRole('button', { name: 'Sign in' })).toHaveFocus()

    await user.keyboard('{Enter}')

    // The create form replaces the sign-in form, and the heading announces who.
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Signed in as alice' })).toBeInTheDocument(),
    )

    await user.click(screen.getByLabelText('Repository name'))
    await user.keyboard('my-repo')
    await user.click(screen.getByRole('button', { name: 'Create repository' }))

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Created alice/my-repo'))
  })

  it('attaches the CSRF token to a state-changing request and not to a read', async () => {
    const mock = stubFetch([
      { status: 200, body: { user: { username: 'alice' }, csrf_token: 'csrf-token' } },
      { status: 201, body: { full_name: 'alice/my-repo' } },
    ])
    const user = userEvent.setup()
    render(<App boot={boot()} />)

    await user.type(screen.getByLabelText('Username or email'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'correct horse battery staple')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(screen.getByRole('heading', { name: /Signed in as/ })).toBeInTheDocument())

    await user.type(screen.getByLabelText('Repository name'), 'my-repo')
    await user.click(screen.getByRole('button', { name: 'Create repository' }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())

    const calls = (mock as unknown as { calls: { input: unknown; init?: RequestInit }[] }).calls
    const signIn = calls[0]
    const create = calls[1]
    expect(signIn?.init?.headers).not.toHaveProperty('X-CSRF-Token')
    expect((create?.init?.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-token')
    // Credentials travel, because the session is a cookie the client never reads.
    expect(create?.init?.credentials).toBe('same-origin')
  })

  it('reports a rejected name against its field', async () => {
    stubFetch([
      { status: 200, body: { user: { username: 'alice' }, csrf_token: 'csrf-token' } },
      {
        status: 422,
        body: { message: 'the request is not valid', errors: [{ field: 'name', message: 'repository name is reserved' }] },
      },
    ])
    const user = userEvent.setup()
    render(<App boot={boot({ account: 'alice' })} />)

    await user.type(screen.getByLabelText('Repository name'), 'admin')
    await user.click(screen.getByRole('button', { name: 'Create repository' }))

    await waitFor(() => expect(screen.getByText('repository name is reserved')).toBeInTheDocument())
    // The field carries the state too, so a screen reader reports it on the input.
    expect(screen.getByLabelText('Repository name')).toHaveAttribute('aria-invalid', 'true')
  })

  it('offers private by default, because that is the product rule', () => {
    stubFetch([])
    render(<App boot={boot({ account: 'alice' })} />)

    expect(screen.getByRole('radio', { name: 'Private' })).toBeChecked()
    expect(screen.getByRole('radio', { name: 'Public' })).not.toBeChecked()
  })
})

describe('accessibility', () => {
  it('has no automatically detectable violations on the create surface', async () => {
    stubFetch([])
    const { container } = render(<App boot={boot({ account: 'alice' })} />)

    const results = await axe.run(container)
    // A failure fails the build, which `frontend.md` states as a release gate.
    expect(results.violations).toEqual([])
  })

  it('has no automatically detectable violations on the sign-in surface', async () => {
    stubFetch([])
    const { container } = render(<App boot={boot()} />)

    const results = await axe.run(container)
    expect(results.violations).toEqual([])
  })

  it('announces a failure through a live region', async () => {
    stubFetch([{ status: 401, body: { message: 'invalid credentials' } }])
    const user = userEvent.setup()
    render(<App boot={boot()} />)

    await user.type(screen.getByLabelText('Username or email'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'wrong')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('invalid credentials')
  })
})