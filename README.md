# coderead

A local code browser with a Claude assistant built in: understand any repo precisely, then turn what you want into an exact request for a coding agent.

    go install github.com/izalamea/cs-professor@latest   # installs as cs-professor
    # or, from a clone:
    go build -o coderead .
    ./coderead ~/code/some-repo

It opens http://localhost:7777. Use the repo button at the top left to switch to any other folder.

- Click any name to see what it is and where it's defined, find its uses, and ask the tutor.
  - Go: `go/types` gives the doc comment, methods, the interfaces it implements and every use.
  - Python, TypeScript/JavaScript, Swift and C: coderead asks the language server editors use
    (pyright, `tsc --lsp` from TypeScript 7, sourcekit-lsp, clangd), if it's installed.
    Install with `npm i -g pyright typescript`; sourcekit-lsp and clangd come with Xcode.
  - Anything a server can't answer falls back to a declaration index that matches by name.
- Markdown files and Jupyter notebooks open rendered (toggle Preview / Source).
- **Draft change** mode: describe a change in your own words ("search feels slow"). The assistant
  reads the code and answers with how it works today, options with trade-offs, and a precise,
  self-contained request to copy into a coding agent. Also from a selection (✎ Change them…),
  a clicked name (✎ Change…), or any answer (✎ Draft a change request from this).
- ⌘/Ctrl-click a name to jump to its definition, even into the standard library or a
  dependency. Alt+← goes back.
- Click a line number (Shift-click for a range) or select code to ask about those lines.
- ⌘P filters files, ⌘⇧F searches the repo.

Files: `analyze.go` (Go type-checking), `lsp.go` (a small Language Server Protocol client),
`index.go` (the by-name fallback), `main.go` (HTTP server + tutor), `index.html` (the page).

How it works: `analyze.go` type-checks the repo with `go/types`, the same checker the
compiler uses, so "where is this defined" is exact. `main.go` serves the page (`index.html`,
embedded with `go:embed`) and runs `claude -p` inside the repo, with only the Read, Grep and
Glob tools, streaming the answer back. The tutor uses your Claude Code login.
The server only listens on 127.0.0.1, and every API call needs a random token that the
server embeds in the page.
