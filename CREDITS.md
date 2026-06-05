# Credits & Acknowledgements

## Ergo (ergochat/ergo)

Lurk's development draws on **[Ergo](https://github.com/ergochat/ergo)** — a
modern, production-grade IRCv3 server written in Go — as a primary reference for
correct protocol behaviour, and as the live server we integration-test against.

- **Project:** https://github.com/ergochat/ergo
- **License:** MIT (© Ergo / Oragono contributors)
- **The `irc-go` library:** https://github.com/ergochat/irc-go — the canonical
  Go implementation of IRCv3 message parsing / tag escaping, which informed our
  `irc` package design.

No Ergo code is copied into Lurk: its implementation is studied as a reference
for correct protocol behaviour and we write our own. Where Ergo's design directly
informs a non-obvious decision, the relevant Lurk source notes it in a comment.

Our thanks to the Ergo maintainers and the wider IRCv3 working group for building
and documenting such a clean reference implementation.

## Bubble Tea / Charm (terminal UI)

Lurk's terminal UI is built on the **[Charm](https://charm.land)** ecosystem:

- **[Bubble Tea](https://github.com/charmbracelet/bubbletea)** (`charm.land/bubbletea/v2`) — the Elm-architecture TUI framework.
- **[Bubbles](https://github.com/charmbracelet/bubbles)** (`charm.land/bubbles/v2`) — components (viewport, textinput, list, …).
- **[Lip Gloss](https://github.com/charmbracelet/lipgloss)** (`charm.land/lipgloss/v2`) — layout & styling.

All MIT-licensed. These are Lurk's runtime dependencies for the `tui/` package.

## Catppuccin (color palette)

Lurk's TUI theme uses the **[Catppuccin Mocha](https://catppuccin.com)** palette
— a dark, harmonious 24-bit color scheme — for its nick colorization and
semantic styles (`tui/style.go`). Catppuccin is MIT-licensed. We render the
colors as truecolor and let Bubble Tea downsample for terminals with smaller
color profiles.

## senpai (UX reference)

**[senpai](https://git.sr.ht/~delthas/senpai)** — an IRCv3 TUI client in Go — is
studied as a UX/decomposition reference (buffers, editor, completion, nick
colorization, command set). No code is copied; it informs design only.
