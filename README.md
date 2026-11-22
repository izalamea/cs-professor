# coderead

A local code browser with a Claude assistant built in, for building your own intuition about a codebase.

Coding agents can change code faster than anyone can read it. What they can't give you is your
own picture of how the code works: where state lives, what calls what, which paths are hot,
which parts are fragile. With that picture you guide an agent better. You ask for the right
change, notice when it heads the wrong way, and can judge what it hands back. coderead is for
building that picture: you read the real code, and you ask questions right where you're looking.

    go install github.com/izalamea/cs-professor@latest   # installs as cs-professor
    # or, from a clone:
    go build -o coderead .
    ./coderead ~/code/some-repo

It opens http://localhost:7777. Use the repo button at the top left to switch to any other folder.

- Click any name to see what it is and where it's defined, find its uses, and ask about it.
  - Go: `go/types` gives the doc comment, methods, the interfaces it implements and every use.
  - Python, TypeScript/JavaScript, Swift and C: coderead asks the language server editors use
    (pyright, `tsc --lsp` from TypeScript 7, sourcekit-lsp, clangd), if it's installed.
    Install with `npm i -g pyright typescript`; sourcekit-lsp and clangd come with Xcode.
  - Anything a server can't answer falls back to a declaration index that matches by name.
- Click a line number (Shift-click for a range) or select code to ask about those lines.
- The assistant answers from the code itself, with links to the exact lines, and names the
  pattern, algorithm or cost involved, so you learn the vocabulary to talk about the code.
- **Draft change** mode: say what you'd like to change in plain words, and the assistant shows
  you how that part works today and what the options are. That's usually where the understanding
  comes from. It also writes a short summary you can paste to an agent if you want a starting point.
- ⌘/Ctrl-click a name to jump to its definition, even into the standard library or a
  dependency. Alt+← goes back.
- Markdown files and Jupyter notebooks open rendered (toggle Preview / Source).
- Paste (or drop) screenshots into the chat box; the assistant looks at them alongside the code.
  They're saved in your user cache folder (`~/Library/Caches/coderead/pastes` on macOS).
- Chat box: Option/Alt+←/→ jumps words, Ctrl+A / Ctrl+E (or Ctrl+B) go to the start / end of the line.
- ⌘P filters files, ⌘⇧F searches the repo.

Files: `analyze.go` (Go type-checking), `lsp.go` (a small Language Server Protocol client),
`index.go` (the by-name fallback), `main.go` (HTTP server + assistant), `index.html` (the page).

How it works: `analyze.go` type-checks Go code with `go/types`, the same checker the compiler
uses, so "where is this defined" is exact. Other languages go through their language servers.
`main.go` serves the page (`index.html`, embedded with `go:embed`) and runs `claude -p` inside
the repo, with only the Read, Grep and Glob tools, streaming the answer back. It uses your
Claude Code login. The server only listens on 127.0.0.1, and every API call needs a random
token that the server embeds in the page.

License: MIT (see `LICENSE`).
