// coderead is a local code browser with a Claude tutor built in.
//
//	coderead [-port 7777] [-no-open] [repo-dir]
//
// It serves a single page on localhost. The page lists the repo's files,
// shows code with every Go identifier clickable (resolved by go/types), and
// sends your questions to the `claude` CLI, running inside the repo with
// read-only tools, so answers come from the real code.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML string

type server struct {
	token     string
	root      string // repo passed on the command line, if any
	home      string
	gorootSrc string
	modcache  string
	claude    string // path to the claude CLI
	pastes    string // where images pasted into the chat are saved
	loader    *Loader
	lsp       *lspManager
	ix        *indexer

	allowMu sync.Mutex
	allowed map[string]bool // files outside the repo a language server pointed us to
}

func main() {
	port := flag.Int("port", 7777, "port to listen on (localhost only)")
	noOpen := flag.Bool("no-open", false, "don't open the browser")
	flag.Parse()

	s := &server{}
	if flag.NArg() > 0 {
		abs, err := filepath.Abs(flag.Arg(0))
		if err != nil || !isDir(abs) {
			log.Fatalf("not a directory: %s", flag.Arg(0))
		}
		s.root = abs
	}
	s.home, _ = os.UserHomeDir()
	goroot := goEnv("GOROOT")
	if goroot == "" {
		goroot = runtime.GOROOT()
	}
	s.gorootSrc = filepath.Join(goroot, "src")
	s.modcache = goEnv("GOMODCACHE")
	s.claude, _ = exec.LookPath("claude")
	// Pure-Go files only, so the standard library type-checks without cgo.
	build.Default.CgoEnabled = false
	s.loader = NewLoader(s.gorootSrc, s.modcache)
	s.lsp = newLSPManager()
	s.ix = newIndexer()
	s.allowed = map[string]bool{}
	if cache, err := os.UserCacheDir(); err == nil {
		s.pastes = filepath.Join(cache, "coderead", "pastes")
	} else {
		s.pastes = filepath.Join(os.TempDir(), "coderead-pastes")
	}
	go func() { // stop language servers on Ctrl-C
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		s.lsp.Shutdown()
		os.Exit(0)
	}()

	b := make([]byte, 16)
	rand.Read(b)
	s.token = hex.EncodeToString(b)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/raw", s.handleRaw)
	mux.HandleFunc("GET /api/sym", s.handleSym)
	mux.HandleFunc("GET /api/symbol", s.handleSymbol)
	mux.HandleFunc("GET /api/refs", s.handleRefs)
	mux.HandleFunc("GET /api/outline", s.handleOutline)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("GET /api/suggest", s.handleSuggest)
	mux.HandleFunc("POST /api/paste", s.handlePaste)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // port taken: pick any free one
		if err != nil {
			log.Fatal(err)
		}
	}
	url := fmt.Sprintf("http://localhost:%d/", ln.Addr().(*net.TCPAddr).Port)
	fmt.Println("coderead is running at", url)
	if s.claude == "" {
		fmt.Println("warning: `claude` CLI not found on PATH; the tutor won't work")
	}
	if s.root != "" {
		go func() { // warm up the type-checker so the first click is fast
			s.loader.mu.Lock()
			s.loader.Repo(s.root)
			s.loader.mu.Unlock()
		}()
	}
	if !*noOpen {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, s.guard(mux)))
}

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

// guard keeps other websites out. The server can read any file you can, so:
// it only listens on 127.0.0.1, rejects requests whose Host isn't localhost
// (DNS rebinding), and every API call must carry the random token that is
// only embedded in the page this server serves.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "localhost" && host != "127.0.0.1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		tok := r.Header.Get("X-Token")
		if r.URL.Path == "/api/raw" && tok == "" {
			tok = r.URL.Query().Get("t") // <img> tags can't send headers
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && tok != s.token {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	boot, _ := json.Marshal(map[string]any{
		"token": s.token, "root": s.root, "home": s.home,
		"goroot": s.gorootSrc, "modcache": s.modcache, "claude": s.claude != "",
		"lsp": s.lsp.Status(),
	})
	page := strings.Replace(indexHTML, "/*BOOT*/", "window.BOOT = "+string(boot)+";", 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(page))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}

// rootParam returns the repo folder named in the request, if it is valid.
func rootParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	root := r.URL.Query().Get("root")
	if root == "" || !filepath.IsAbs(root) || !isDir(root) {
		writeErr(w, http.StatusBadRequest, "not a folder: "+root)
		return "", false
	}
	return filepath.Clean(root), true
}

// resolve turns a page path (repo-relative, or absolute inside GOROOT or the
// module cache) into an absolute filename.
func (s *server) resolve(root, p string) (string, bool) {
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(root, filepath.FromSlash(p))
	}
	s.allowMu.Lock()
	ok := s.allowed[abs]
	s.allowMu.Unlock()
	if ok {
		return abs, true
	}
	for _, base := range []string{root, s.gorootSrc, s.modcache, s.pastes} {
		if base != "" && strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return abs, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- file tree

var skipWalk = map[string]bool{"node_modules": true, "vendor": true, "dist": true, "target": true,
	"__pycache__": true, "build": true, "site-packages": true, "DerivedData": true, "Pods": true}

// venvParts are the folders a Python virtualenv creates.
var venvParts = map[string]bool{"bin": true, "lib": true, "lib64": true, "include": true, "share": true,
	"Scripts": true, "Lib": true, "Include": true, "etc": true, "pyvenv.cfg": true, ".gitignore": true}

// isVirtualenv spots Python virtualenv folders (often committed by
// accident), which hold thousands of library files that aren't the
// project's code. Some projects put their code inside the virtualenv
// folder itself; then only the virtualenv's own folders are skipped.
func isVirtualenv(dir, name string) bool {
	if venvParts[name] {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "pyvenv.cfg")); err == nil {
			return true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); err != nil {
		return false
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !venvParts[e.Name()] {
			return false // has the project's own files in it
		}
	}
	return true
}

func listFiles(root string) []string {
	// In a git repo, let git decide (it knows .gitignore).
	out, err := exec.Command("git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z").Output()
	if err == nil {
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f == "" {
				continue
			}
			if st, err := os.Stat(filepath.Join(root, f)); err == nil && st.Mode().IsRegular() {
				files = append(files, filepath.ToSlash(f))
			}
		}
		if len(files) > 0 {
			sort.Strings(files)
			return files
		}
	}
	var files []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || skipWalk[d.Name()] || isVirtualenv(p, d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= 20000 {
			return filepath.SkipAll
		}
		if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".DS_Store") {
			rel, _ := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	files := listFiles(root)
	if files == nil {
		files = []string{}
	}
	writeJSON(w, map[string]any{"root": root, "name": filepath.Base(root), "files": files})
	go func() { // start type-checking in the background
		s.loader.mu.Lock()
		s.loader.Repo(root)
		s.loader.mu.Unlock()
	}()
}

// ---------------------------------------------------------------- files

func langOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "go"
	case ".js", ".mjs", ".cjs", ".jsx":
		return "js"
	case ".ts", ".tsx":
		return "ts"
	case ".py", ".pyi":
		return "py"
	case ".rs":
		return "rust"
	case ".c", ".h", ".cc", ".cpp", ".hpp":
		return "c"
	case ".swift":
		return "swift"
	case ".ipynb":
		return "ipynb"
	case ".m", ".mm":
		return "c"
	case ".java", ".kt":
		return "java"
	case ".sh", ".bash", ".zsh":
		return "sh"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".md", ".markdown":
		return "md"
	case ".html", ".htm", ".xml", ".svg", ".vue", ".svelte", ".mako", ".jinja", ".j2", ".plist", ".xib", ".storyboard":
		return "html"
	case ".css", ".scss", ".less":
		return "css"
	case ".sql":
		return "sql"
	case ".proto":
		return "proto"
	case ".ini", ".cfg", ".lock":
		return "toml"
	case ".mod", ".sum", ".work":
		return "gomod"
	}
	if strings.EqualFold(filepath.Base(name), "Makefile") {
		return "make"
	}
	if strings.EqualFold(filepath.Base(name), "Dockerfile") {
		return "sh"
	}
	return "text"
}

func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	abs, ok := s.resolve(root, r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusForbidden, "path outside the repo, GOROOT and module cache")
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	resp := map[string]any{"abs": abs, "lang": langOf(abs)}
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		resp["path"] = filepath.ToSlash(rel)
	} else {
		resp["path"] = abs
	}
	if st.Size() > 2<<20 {
		resp["tooBig"] = true
		writeJSON(w, resp)
		return
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bytes.IndexByte(src[:min(len(src), 8000)], 0) >= 0 {
		resp["binary"] = true
		writeJSON(w, resp)
		return
	}
	resp["content"] = string(src)
	if resp["lang"] == "go" {
		s.loader.mu.Lock()
		repo := s.loader.Repo(root)
		resp["go"] = s.loader.GoFile(repo, abs, src)
		s.loader.mu.Unlock()
	}
	writeJSON(w, resp)
}

// handleRaw serves a file's bytes, for images inside rendered markdown.
func (s *server) handleRaw(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	abs, ok := s.resolve(root, r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusForbidden, "path outside the repo")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	// Never let a repo file run as a page on this origin (it could read the token).
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func (s *server) handleSym(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	s.loader.mu.Lock()
	defer s.loader.mu.Unlock()
	repo := s.loader.Repo(root)
	d := s.loader.Sym(repo, r.URL.Query().Get("key"))
	if d == nil {
		writeErr(w, http.StatusNotFound, "unknown symbol (the file may have changed; reopen it)")
		return
	}
	writeJSON(w, d)
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, map[string]any{"hits": []any{}})
		return
	}
	// Smart case: lowercase query searches case-insensitively.
	fold := strings.ToLower(q) == q
	needle := q
	if fold {
		needle = strings.ToLower(q)
	}
	type hit struct {
		P    string `json:"p"`
		L    int    `json:"l"`
		Text string `json:"text"`
	}
	hits := []hit{}
	truncated := false
	for _, f := range listFiles(root) {
		if len(hits) >= 500 {
			truncated = true
			break
		}
		abs := filepath.Join(root, filepath.FromSlash(f))
		if st, err := os.Stat(abs); err != nil || st.Size() > 1<<20 {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil || bytes.IndexByte(src[:min(len(src), 8000)], 0) >= 0 {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(src))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			hay := line
			if fold {
				hay = strings.ToLower(line)
			}
			if strings.Contains(hay, needle) {
				t := strings.TrimSpace(line)
				if len(t) > 200 {
					t = t[:200] + "…"
				}
				hits = append(hits, hit{f, n, t})
				if len(hits) >= 500 {
					break
				}
			}
		}
	}
	writeJSON(w, map[string]any{"hits": hits, "truncated": truncated})
}

// handleBrowse lists subfolders, for the "open a repo" dialog.
func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = s.home
	}
	if strings.HasPrefix(dir, "~") {
		dir = filepath.Join(s.home, dir[1:])
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) || !isDir(dir) {
		writeErr(w, http.StatusBadRequest, "not a folder: "+dir)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	type entry struct {
		Name  string `json:"name"`
		Git   bool   `json:"git,omitempty"`
		GoMod bool   `json:"gomod,omitempty"`
	}
	list := []entry{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if !isDir(p) {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(p, ".git"))
		_, modErr := os.Stat(filepath.Join(p, "go.mod"))
		list = append(list, entry{e.Name(), gitErr == nil, modErr == nil})
	}
	_, gitErr := os.Stat(filepath.Join(dir, ".git"))
	_, modErr := os.Stat(filepath.Join(dir, "go.mod"))
	writeJSON(w, map[string]any{
		"dir": dir, "parent": filepath.Dir(dir), "entries": list,
		"git": gitErr == nil, "gomod": modErr == nil,
	})
}

// ---------------------------------------------------------------- tutor

const tutorPrompt = `You are embedded in "coderead", a code browser. The user is reading the repository in your current working directory and asking you about it.

Who you're talking to: an experienced software architect. They know how systems are built; what they don't know yet is this code. They're building their own understanding of it so they can judge and guide changes, including changes made by AI agents. Talk to them as a peer who has just read the code carefully: plain language, no lecturing, no basics explained, no jargon for its own sake.

What a good answer does:
- Describes what the code actually does: the control flow, where state lives and who mutates it, the data structures (what's stored, keyed by what, how it grows), the algorithms (what they cost and on which input), and the patterns or design choices in play, when they're really there. Name a pattern or algorithm only if the code genuinely implements it, and point at where.
- Covers what an architect would check next: ordering and concurrency, error and failure paths, what happens at the boundaries (I/O, network, persistence), invariants the code relies on and where they're enforced or not, and costs on the hot paths in concrete terms (one query per item, a full scan per keystroke, a lock held across a network call).
- Leads with the direct answer, then the supporting detail. Short code excerpts where they carry the point. No filler, no restating the question, no closing summary or praise of the code.

Depth over fluency. This matters most:
- Every claim about this repository must come from code you read in this conversation (Read, Grep, Glob). Read the functions you describe, including the ones they call when the behavior depends on them. Don't describe code from its name, a comment, or what such code usually does.
- Cite the place behind each concrete claim as a backticked path with a line number relative to the repository root, like ` + "`src/app.py:42`" + ` or ` + "`lib/store.ts:10-25`" + `. The browser turns these into links. For library code outside the repo use the absolute path (Go standard library: %s, Go modules: %s; other languages: node_modules, virtualenv site-packages, SDK headers).
- Separate what you verified from what you infer. If you didn't follow a path all the way (a callee you didn't open, a config value set elsewhere, behavior at runtime), say so plainly instead of smoothing over it. "I didn't check X" is better than a confident guess.
- Prefer a narrower answer that holds up to a broad one that doesn't. If the question is too big to answer properly from what you read, answer the core part well and say what's left.
- Language features only when they matter for understanding this code, in that language's own terms. No comparisons with other languages unless asked.

Context: the browser sends context with each question (the open file, a clicked name with what the language tools found, or selected lines). Treat it as a starting point and verify it.
If the question is about changing behavior, say where the change would go, what it would touch, and what could break.
You can only read; you cannot edit files or run commands.`

type askReq struct {
	Root    string `json:"root"`
	Prompt  string `json:"prompt"`
	Session string `json:"session"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

// streamMsg is the part of `claude --output-format stream-json` we read.
type streamMsg struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Event     *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
		} `json:"content_block"`
	} `json:"event"`
	Message *struct {
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Result          string  `json:"result"`
	IsError         bool    `json:"is_error"`
	Cost            float64 `json:"total_cost_usd"`
}

var allowedModels = map[string]bool{"": true, "sonnet": true, "opus": true, "fable": true, "haiku": true}
var allowedEffort = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.claude == "" {
		writeErr(w, http.StatusServiceUnavailable, "the claude CLI was not found on PATH when coderead started")
		return
	}
	if !filepath.IsAbs(req.Root) || !isDir(req.Root) || !allowedModels[req.Model] || !allowedEffort[req.Effort] {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--tools", "Read,Grep,Glob",
		"--allowedTools", "Read,Grep,Glob",
		"--permission-prompts", "none",
		"--append-system-prompt", fmt.Sprintf(tutorPrompt, s.gorootSrc, s.modcache),
		"--add-dir", s.gorootSrc,
	}
	if isDir(s.pastes) {
		args = append(args, "--add-dir", s.pastes) // pasted screenshots
	}
	if s.modcache != "" {
		args = append(args, "--add-dir", s.modcache)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Session != "" {
		args = append(args, "--resume", req.Session)
	}
	// The request's context is cancelled when the page hits Stop or closes,
	// which kills the claude process.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, s.claude, args...)
	cmd.Dir = req.Root
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		w.Write(append(b, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	gotResult := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var m streamMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.ParentToolUseID != nil && *m.ParentToolUseID != "" {
			continue // a subagent's inner messages
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" && m.SessionID != "" {
				emit(map[string]any{"t": "session", "id": m.SessionID})
			}
		case "stream_event":
			if m.Event == nil {
				continue
			}
			switch m.Event.Type {
			case "content_block_start":
				if m.Event.ContentBlock != nil && m.Event.ContentBlock.Type == "text" {
					emit(map[string]any{"t": "block"})
				}
			case "content_block_delta":
				if m.Event.Delta != nil && m.Event.Delta.Type == "text_delta" {
					emit(map[string]any{"t": "text", "d": m.Event.Delta.Text})
				}
			}
		case "assistant":
			if m.Message == nil {
				continue
			}
			for _, c := range m.Message.Content {
				if c.Type != "tool_use" {
					continue
				}
				detail := ""
				for _, k := range []string{"file_path", "pattern", "path"} {
					if v, ok := c.Input[k].(string); ok && v != "" {
						if detail != "" {
							detail += " in "
						}
						detail += v
					}
				}
				emit(map[string]any{"t": "tool", "name": c.Name, "detail": detail})
			}
		case "result":
			gotResult = true
			emit(map[string]any{"t": "done", "error": m.IsError, "result": m.Result,
				"subtype": m.Subtype, "cost": m.Cost, "session": m.SessionID})
		}
	}
	err = cmd.Wait()
	if !gotResult && ctx.Err() == nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		emit(map[string]any{"t": "error", "d": msg})
	}
}

// ---------------------------------------------------------------- other languages

type symbolResp struct {
	Engine     string `json:"engine,omitempty"`     // language server used, if any
	EngineErr  string `json:"engineErr,omitempty"`  // why it couldn't be used
	Hover      string `json:"hover,omitempty"`      // markdown from the server
	Defs       []Ref  `json:"defs"`                 // exact definitions (server)
	Candidates []Decl `json:"candidates,omitempty"` // declarations with the same name (index)
}

func posQuery(r *http.Request) (line, col int, ok bool) {
	var err1, err2 error
	line, err1 = strconv.Atoi(r.URL.Query().Get("line"))
	col, err2 = strconv.Atoi(r.URL.Query().Get("col"))
	return line, col, err1 == nil && err2 == nil && line > 0 && col > 0
}

// toRefs converts server locations to the page's shape, remembering files
// outside the repo so the page may open them.
func (s *server) toRefs(root string, locs []lspLocation) []Ref {
	out := []Ref{}
	cache := map[string][]string{}
	for _, l := range locs {
		abs := uriPath(l.URI)
		if abs == "" {
			continue
		}
		lines, ok := cache[abs]
		if !ok {
			if src, err := os.ReadFile(abs); err == nil {
				lines = strings.Split(string(src), "\n")
			}
			cache[abs] = lines
		}
		text := ""
		if l.Range.Start.Line < len(lines) {
			text = strings.TrimSpace(lines[l.Range.Start.Line])
			if len(text) > 160 {
				text = text[:160] + "…"
			}
		}
		p := abs
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.ToSlash(rel)
		} else {
			s.allowMu.Lock()
			s.allowed[abs] = true
			s.allowMu.Unlock()
		}
		out = append(out, Ref{P: p, L: l.Range.Start.Line + 1, C: l.Range.Start.Character + 1, Text: text})
	}
	return out
}

// handleSymbol answers a click on a name in a non-Go file.
func (s *server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	abs, ok := s.resolve(root, path)
	line, col, okPos := posQuery(r)
	if !ok || !okPos {
		writeErr(w, http.StatusBadRequest, "bad path or position")
		return
	}
	resp := symbolResp{Defs: []Ref{}}
	c, name, err := s.lsp.For(root, abs)
	resp.Engine = name
	if err != nil {
		resp.EngineErr = err.Error()
	}
	if c != nil {
		// Generous: some servers (sourcekit-lsp) index on their first request.
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		var defs []lspLocation
		var defErr error
		wg.Add(2)
		go func() { defer wg.Done(); defs, defErr = c.Definition(ctx, abs, line, col) }()
		go func() { defer wg.Done(); resp.Hover, _ = c.Hover(ctx, abs, line, col) }()
		wg.Wait()
		if defErr != nil {
			resp.EngineErr = defErr.Error()
		}
		resp.Defs = s.toRefs(root, defs)
	}
	if len(resp.Defs) == 0 {
		if name := r.URL.Query().Get("name"); name != "" {
			rel := path
			if filepath.IsAbs(rel) {
				rel = ""
			}
			resp.Candidates = s.ix.Get(root).Lookup(name, rel)
		}
	}
	writeJSON(w, resp)
}

func (s *server) handleRefs(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	abs, ok := s.resolve(root, r.URL.Query().Get("path"))
	line, col, okPos := posQuery(r)
	if !ok || !okPos {
		writeErr(w, http.StatusBadRequest, "bad path or position")
		return
	}
	c, _, err := s.lsp.For(root, abs)
	if c == nil {
		msg := "no language server for this file"
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, http.StatusNotImplemented, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	locs, err := c.References(ctx, abs, line, col)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	refs := s.toRefs(root, locs)
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].P != refs[j].P {
			return refs[i].P < refs[j].P
		}
		return refs[i].L < refs[j].L
	})
	total := len(refs)
	if len(refs) > 300 {
		refs = refs[:300]
	}
	writeJSON(w, map[string]any{"refs": refs, "total": total})
}

// handleOutline lists a non-Go file's declarations: from its language
// server when there is one, otherwise from the name index.
func (s *server) handleOutline(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	abs, ok := s.resolve(root, path)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad path")
		return
	}
	if c, name, _ := s.lsp.For(root, abs); c != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if items, err := c.DocumentSymbols(ctx, abs); err == nil && len(items) > 0 {
			writeJSON(w, map[string]any{"outline": items, "engine": name})
			return
		}
	}
	items := []OutlineItem{}
	if !filepath.IsAbs(path) {
		for _, d := range s.ix.Get(root).byFile[path] {
			items = append(items, OutlineItem{N: d.N, K: d.K, L: d.L})
		}
	}
	writeJSON(w, map[string]any{"outline": items, "engine": "index"})
}

// ---------------------------------------------------------------- suggestions

// handleSuggest asks Claude (a small, fast model, no tools) for starter
// questions specific to this repo, from its README, file list and the start
// of its main files.
func (s *server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	root, ok := rootParam(w, r)
	if !ok {
		return
	}
	if s.claude == "" {
		writeErr(w, http.StatusServiceUnavailable, "claude CLI not found")
		return
	}
	files := listFiles(root)
	var b strings.Builder
	fmt.Fprintf(&b, "Repository folder name: %s\n\nFiles (%d total):\n", filepath.Base(root), len(files))
	for i, f := range files {
		if i == 400 {
			b.WriteString("…\n")
			break
		}
		b.WriteString(f + "\n")
	}
	readme := ""
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f))
		if strings.HasPrefix(base, "readme") && (readme == "" || !strings.Contains(f, "/")) {
			readme = f
		}
	}
	if readme != "" {
		if src, err := os.ReadFile(filepath.Join(root, readme)); err == nil {
			fmt.Fprintf(&b, "\n%s:\n%s\n", readme, clip(string(src), 5000))
		}
	}
	// The beginning of the main code files says what the code actually does.
	for _, f := range mainFiles(root, files, 3) {
		if src, err := os.ReadFile(filepath.Join(root, f)); err == nil {
			fmt.Fprintf(&b, "\nStart of %s:\n%s\n", f, clip(string(src), 1800))
		}
	}
	prompt := b.String() + `
Someone just opened this repository in a code reader with an AI assistant. They want to understand it precisely enough to direct a coding agent to change or improve it. Suggest 4 questions they could click to get there.
- Make every question specific to THIS repository. Cover: its purpose and how the main parts fit together; how its central flow works end to end; the most interesting algorithm, data structure or technique in it; and where a likely change or improvement would go (e.g. a performance hot spot, a fragile part, an extension point). Name real files, functions or concepts from it.
- Write them in the user's voice ("How does…", "Walk me through…", "Where would I…"). Don't ask about tooling, setup or the README itself.
Reply with only a JSON array of 4 objects: {"label": "link text, at most 7 words", "question": "the full question, one or two sentences"}.`

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.claude, "-p", "--model", "haiku", "--tools", "",
		"--no-session-persistence", "--output-format", "text")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claude failed: "+err.Error())
		return
	}
	text := string(out)
	i, j := strings.Index(text, "["), strings.LastIndex(text, "]")
	var qs []struct {
		Label    string `json:"label"`
		Question string `json:"question"`
	}
	if i < 0 || j <= i || json.Unmarshal([]byte(text[i:j+1]), &qs) != nil || len(qs) == 0 {
		writeErr(w, http.StatusBadGateway, "couldn't read suggestions")
		return
	}
	if len(qs) > 5 {
		qs = qs[:5]
	}
	writeJSON(w, map[string]any{"suggestions": qs})
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}

// mainFiles guesses the most telling code files: entry points first, then
// the largest source files.
func mainFiles(root string, files []string, n int) []string {
	type cand struct {
		f     string
		score int64
	}
	var cs []cand
	for _, f := range files {
		lang := langOf(f)
		switch lang {
		case "go", "py", "js", "ts", "swift", "c", "rust", "java", "sh":
		default:
			continue
		}
		if strings.Contains(f, "test") || strings.Contains(f, "node_modules/") || strings.HasSuffix(f, ".d.ts") {
			continue
		}
		st, err := os.Stat(filepath.Join(root, f))
		if err != nil {
			continue
		}
		score := min(st.Size(), 40000)
		base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		switch base {
		case "main", "app", "App", "index", "server", "cli", "__main__", "lib", "mod":
			score += 100000
		}
		score -= int64(strings.Count(f, "/")) * 5000
		cs = append(cs, cand{f, score})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	var out []string
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].f)
	}
	return out
}

// ---------------------------------------------------------------- pasted images

// handlePaste saves an image pasted into the chat (the raw bytes are the
// request body) and returns its path. The assistant reads it with its Read
// tool, which understands images. Files are named by content hash, so pasting
// the same screenshot twice stores it once.
func (s *server) handlePaste(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "image too large (20 MB max)")
		return
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[http.DetectContentType(data)]
	if ext == "" {
		writeErr(w, http.StatusUnsupportedMediaType, "only PNG, JPEG, GIF and WebP images can be pasted")
		return
	}
	if err := os.MkdirAll(s.pastes, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(s.pastes, hex.EncodeToString(sum[:8])+ext)
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, map[string]string{"path": path})
}
