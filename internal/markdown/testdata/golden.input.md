# Golden fixture

This file exercises **every** element the allowlist permits and every control the
sanitizer exists for. It is rendered by `TestTheGoldenFileMatches`, which fails when the
output changes — so a change to an extension, to the policy, or to the escaping has to be
an intentional update here **and** a bump to `markdown.Version`, which is what actually
invalidates a deployed cache.

## Text and emphasis

Plain prose with an ampersand: Tom & Jerry. Text with angle brackets: &lt;not-a-tag&gt;.
*Italic*, **bold**, ***both***, ~~struck through~~, and `an inline code span`.

A soft
break, and a hard break after this line.  
The line after a hard break.

## Links

An [external link](https://example.com "with a title"), a [relative link](/alice/other-repo),
an [anchor](#text-and-emphasis), and a [mailto link](mailto:person@example.com).

An autolinked URL: https://example.com/auto and an autolinked address: <mail@example.com>.

## Lists

- first item
- second item with `code`
  - a nested item

1. ordered one
2. ordered two

### Task list

- [x] a completed task
- [ ] an outstanding task

## Blocks

> A blockquote, which may span
> more than one line.

```go
// A fenced code block with a language.
func main() {
	println("hello")
}
```

```
A fenced block with no language.
```

---

## Table

| Left | Center | Right |
| :--- | :----: | ----: |
| a    |   b    |     c |
| `d`  | **e**  |  *f*  |

## Raw HTML, which is escaped rather than executed

A script block:

<script>alert('this must never execute')</script>

An inline handler: <img src="x" onerror="alert('neither must this')">

An iframe: <iframe src="https://evil.example"></iframe>

A comment: <!-- which must not survive -->

An SVG script: <svg><script>alert('nor this')</script></svg>

An event handler on a div: <div onclick="alert('nor this')">click</div>

## Dangerous link targets, whose attributes must be removed

A [javascript link](javascript:alert('no')), a [data link](data:text/html,<script>no</script>),
and a [vbscript link](vbscript:no).

A markdown image with a data URL: ![x](data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=)

A relative image, which is permitted: ![alt text](/alice/other-repo/raw/main/image.png)

## Characters that must survive

Non-ASCII text: 日本語, ünïcödé, ← arrows, and an emoji.