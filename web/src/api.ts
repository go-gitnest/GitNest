// The same-origin API client.
//
// `frontend.md` fixes three rules this file implements: requests are same-origin,
// they carry credentials, and a state-changing request carries the CSRF token.
// The token lives in a variable rather than in storage, so a reload loses it —
// which is why the shell reports whether the caller is signed in and the surface
// asks for a sign-in when it is not.

/** The error envelope the API returns, per `architecture.md` section 11. */
export interface ApiErrorBody {
  message?: string
  errors?: { field?: string; code?: string; message?: string }[]
}

/** ApiError is a failed request, carrying the status and the envelope. */
export class ApiError extends Error {
  readonly status: number
  readonly body: ApiErrorBody

  constructor(status: number, body: ApiErrorBody) {
    super(body.message ?? `the request failed with status ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }

  /** fieldError returns the message for one field, if the API named it. */
  fieldError(field: string): string | undefined {
    return this.body.errors?.find((problem) => problem.field === field)?.message
  }
}

/** The CSRF token a state-changing request must carry. */
let csrfToken: string | undefined

export function setCSRFToken(token: string | undefined): void {
  csrfToken = token
}

export function currentCSRFToken(): string | undefined {
  return csrfToken
}

interface RequestOptions {
  readonly method?: string
  readonly body?: unknown
}

/**
 * request sends one API call.
 *
 * `credentials: 'same-origin'` is what carries the session cookie; the token
 * itself is never read by this code, because it is `HttpOnly`.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  // A state-changing method carries the token. It is attached here rather than by
  // each caller, so a new call site cannot forget it.
  if (method !== 'GET' && method !== 'HEAD' && csrfToken) {
    headers['X-CSRF-Token'] = csrfToken
  }

  let response: Response
  try {
    response = await fetch(path, {
      method,
      headers,
      credentials: 'same-origin',
      ...(options.body !== undefined ? { body: JSON.stringify(options.body) } : {}),
    })
  } catch (cause) {
    // A transport failure is distinct from a rejection: the surface says the
    // server could not be reached rather than that the request was refused.
    throw new ApiError(0, { message: 'network' })
  }

  if (response.status === 204) {
    return undefined as T
  }

  const text = await response.text()
  let parsed: unknown
  try {
    parsed = text ? JSON.parse(text) : {}
  } catch {
    parsed = {}
  }

  if (!response.ok) {
    throw new ApiError(response.status, (parsed ?? {}) as ApiErrorBody)
  }
  return parsed as T
}