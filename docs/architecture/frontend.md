# Frontend and rendering strategy

**Status:** Target design. Nothing in this document is implemented yet.
**Scope:** How GitNest renders HTML, where the React application is used, and the accessibility, internationalization, and browser policies that follow from it.

## Purpose

A Git forge's public pages are part of the public web. Repository overviews, file views, issues, and pull requests must be readable without JavaScript, indexable by search engines, and previewable when a link is pasted into a chat client. GitHub behaves this way, so parity requires it. At the same time the authenticated product is highly interactive and benefits from a component-based client.

This document fixes the split, because discovering it after the first browser workstream means rewriting that workstream.

## Decision

**Server-rendered HTML is the default for every read surface. React renders the interactive surfaces.**

| Surface | Rendered by | Notes |
| --- | --- | --- |
| Anonymous and public read pages: user and organization profiles, repository overview, file and directory view, blame, commit and history, issue and pull request read views, releases, packages, wiki, discussions, search results, explore | Go HTML templates in `internal/transport/http/web` | Complete HTML on first byte, no JavaScript required, Open Graph metadata, canonical URLs |
| Authenticated read pages | The same templates | The page varies by the viewer's permission, and the response is `private, no-store` |
| Interactive surfaces: settings, editors, review submission, comment composer, project boards, notification inbox, workspace IDE shell, CI run views, file upload | React under `web/` | Mounted into a server-rendered shell, or served as an application route |
| Real-time surfaces: run logs, terminal, notifications | React | Stream over the API |

Consequences that must hold:

- **No JavaScript is required to read public content, sign in, or sign out.** These are the paths a search engine, a link preview, and a text browser take.
- **One source of truth for data.** Server templates and the API call the same application services. A permission question is answered by the shared service in both paths; the templates never compute access themselves.
- **One route table.** Web routes and API paths derive from the same registry so that a URL cannot exist in one and not the other. See [urls-and-identifiers.md](urls-and-identifiers.md).
- **A page must be usable before hydration.** The React islands enhance a working page; they do not build it.
- **Node.js is a build-time dependency only.** The production instance serves pre-built assets from the Go process. Server-rendering must not require a Node runtime.

## Frontend architecture

- **Language and build:** TypeScript with Vite. Assets are built to a hashed, immutable output directory and served same-origin.
- **Routing:** the Go process owns routing and returns the right shell; the client router handles in-app navigation within an application area. A deep link must work on first load without client-side routing.
- **Data access:** `fetch` against the same-origin API with credentials, and the CSRF token attached to state-changing requests. No tokens in `localStorage`.
- **State:** server data is cached by the client's data layer keyed on the request; client state is local to the feature. No global store until two unrelated features demonstrably need one.
- **Components:** a small in-repository design system built on headless primitives with our own styling, rather than a heavyweight component framework. The visual language is GitNest's own; parity is behavioural, not pixel-level.
- **Styling:** design tokens with a light and dark theme, both driven by one token set. No inline style strings assembled from user data.
- **Markdown and code:** rendered on the server for read surfaces so the client does not ship a Markdown renderer or a syntax highlighter for content it only displays. Raw HTML is disabled and output is escaped.
- **Bundle discipline:** route-level code splitting, a documented bundle-size budget (see [performance.md](performance.md)), and no dependency added for something a few lines of standard code can do.

## Search engine and link preview behaviour

- Public pages emit canonical URLs, `robots` directives, and Open Graph and Twitter card metadata; private and authenticated pages are `noindex` and never cached by a shared cache.
- Renamed entities serve a permanent redirect from the old path; the redirect policy lives in [urls-and-identifiers.md](urls-and-identifiers.md).
- A sitemap covers public repositories and their public content, generated from the database rather than crawled.
- Link previews require the page's title and description to be renderable without JavaScript, which the server-rendered read surfaces satisfy.

## Accessibility

- **Target:** WCAG 2.2 level AA for every shipped surface. This is a release gate, not an aspiration.
- Every workflow is operable by keyboard alone, including sign-in, repository creation, issue and pull request creation, review submission, and the merge flow.
- Focus is managed on navigation and on dialog and menu open and close; a skip link precedes repeated navigation.
- Interactive patterns use established ARIA patterns for dialogs, menus, comboboxes, tabs, and disclosure, and are tested with a screen reader on at least the sign-in, repository, and pull request flows.
- Colour is never the only carrier of meaning; contrast meets AA in both themes; motion respects the reduced-motion preference; error messages are announced and tied to their field.
- Automated accessibility checks run in CI over the primary templates and components, and a failure fails the build.

## Internationalization

**Decision: build i18n-ready from the start; ship en-US first, then zh-CN.**

- No user-visible string is hardcoded in a Go template or a React component. Strings are referenced by identifier from a message catalogue, and CI fails on a new hardcoded literal in a user-visible position.
- Server-rendered and client-rendered surfaces read from the same catalogues and the same identifier namespace, so a message is translated once.
- Locale selection: the account setting wins, then the request's `Accept-Language`, then the instance default.
- Dates, times, numbers, and relative times are formatted per locale rather than with fixed layouts; storage is always UTC.
- Layout rules avoid text baked into images, avoid fixed-width containers that assume Latin text, and avoid string concatenation for sentences with grammar.
- Pluralization uses the platform message format rather than `count == 1` conditionals.
- Right-to-left correctness is a layout rule from the start (logical properties rather than left/right), even though no RTL locale ships initially.

Retrofitting this later is expensive because it touches every template and component; hence the decision now and the lint rule from the first frontend workstream.

## Browser support

- Evergreen Chrome, Edge, Firefox, and Safari, current and previous major version. No support for Internet Explorer.
- Progressive enhancement is the rule: a browser without a feature the client uses still gets the server-rendered read path and the core flows.
- A browser-support table is maintained in the README with the last review date.

## Testing obligations

- Server-rendered read pages: rendered without JavaScript and asserted on meaningful content, not on markup shapes.
- Public pages: canonical URL, Open Graph metadata, and `noindex` on authenticated pages asserted in tests.
- Accessibility: automated checks on primary templates and components in CI, plus a documented manual screen-reader pass per release.
- Internationalization: the hardcoded-string lint fails on a violation; a pseudo-locale build is checked for layout breakage.
- Client: component tests for interactive islands, and end-to-end tests for sign-in, repository creation, issue creation, review submission, and the merge flow.
- Same decision twice: a test asserts that a permission decision returned by the API and the one used by the HTML render agree for the same viewer and resource.

## Open questions

- Whether comment composition and review submission are implemented as React islands on the server-rendered page or as application routes. Either satisfies the decision above; the choice is made in the workstream that first needs it, and recorded there.
