package main

// lsp.go is a small Language Server Protocol client. For languages other
// than Go, coderead asks the same servers editors use (pyright for Python,
// typescript-language-server for TS/JS, sourcekit-lsp for Swift, clangd for
// C) "where is this defined?", "what is this?" and "who uses it?".
//
// The protocol is JSON-RPC 2.0 over the server's stdin/stdout, each message
// preceded by a "Content-Length: N" header.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type lspSpec struct {
	Name string            // shown in the page, e.g. "pyright"
	Bin  string            // executable name
	Args []string          //
	Exts map[string]string // file extension -> LSP languageId
}

var lspSpecs = []lspSpec{
	{"pyright", "pyright-langserver", []string{"--stdio"}, map[string]string{".py": "python", ".pyi": "python"}},
	{"typescript", "typescript-language-server", []string{"--stdio"}, map[string]string{
		".ts": "typescript", ".tsx": "typescriptreact", ".mts": "typescript", ".cts": "typescript",
		".js": "javascript", ".jsx": "javascriptreact", ".mjs": "javascript", ".cjs": "javascript"}},
	{"sourcekit-lsp", "sourcekit-lsp", nil, map[string]string{".swift": "swift"}},
	{"clangd", "clangd", []string{"--background-index=false"}, map[string]string{
		".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".m": "objective-c", ".mm": "objective-cpp"}},
}

// findBin looks on PATH, then in places PATH often misses when coderead is
// started from a GUI or a different shell (nvm, Homebrew, Xcode).
func findBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	var cands []string
	if m, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin", name)); len(m) > 0 {
		sort.Strings(m)
		cands = append(cands, m[len(m)-1])
	}
	cands = append(cands, "/opt/homebrew/bin/"+name, "/usr/local/bin/"+name, filepath.Join(home, ".local/bin", name))
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if out, err := exec.Command("xcrun", "--find", name).Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

type lspManager struct {
	mu      sync.Mutex
	bins    map[string]string   // spec name -> path ("" = not installed)
	args    map[string][]string // overrides spec.Args
	clients map[string]*lspClient
}

func newLSPManager() *lspManager {
	m := &lspManager{bins: map[string]string{}, args: map[string][]string{}, clients: map[string]*lspClient{}}
	for _, s := range lspSpecs {
		m.bins[s.Name] = findBin(s.Bin)
	}
	// TypeScript 7 (the native port) has a language server built into tsc
	// and no longer ships the tsserver.js that typescript-language-server
	// needs, so prefer it when present.
	if tsc := findBin("tsc"); tsc != "" {
		out, _ := exec.Command(tsc, "--version").Output()
		v := strings.TrimPrefix(strings.TrimSpace(string(out)), "Version ")
		if major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0]); err == nil && major >= 7 {
			m.bins["typescript"] = tsc
			m.args["typescript"] = []string{"--lsp", "--stdio"}
		}
	}
	return m
}

func specFor(path string) (*lspSpec, string) {
	ext := strings.ToLower(filepath.Ext(path))
	for i := range lspSpecs {
		if id, ok := lspSpecs[i].Exts[ext]; ok {
			return &lspSpecs[i], id
		}
	}
	return nil, ""
}

// Status reports which languages have a server, for the page.
func (m *lspManager) Status() map[string]bool {
	out := map[string]bool{}
	for _, s := range lspSpecs {
		out[s.Name] = m.bins[s.Name] != ""
	}
	return out
}

// For returns a running client for this file's language in root, starting
// one if needed. It returns the server name even when none is installed.
func (m *lspManager) For(root, path string) (*lspClient, string, error) {
	spec, _ := specFor(path)
	if spec == nil {
		return nil, "", nil
	}
	m.mu.Lock()
	bin := m.bins[spec.Name]
	if bin == "" {
		m.mu.Unlock()
		return nil, spec.Name, fmt.Errorf("%s is not installed", spec.Bin)
	}
	key := spec.Name + "|" + root
	c := m.clients[key]
	if c == nil || c.isDead() {
		args, ok := m.args[spec.Name]
		if !ok {
			args = spec.Args
		}
		c = startLSP(spec, bin, args, root)
		m.clients[key] = c
	}
	m.mu.Unlock()
	select {
	case <-c.ready:
	case <-time.After(60 * time.Second):
		return nil, spec.Name, errors.New(spec.Name + " is taking too long to start")
	}
	if c.initErr != nil {
		return nil, spec.Name, c.initErr
	}
	return c, spec.Name, nil
}

func (m *lspManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		if c.cmd != nil && c.cmd.Process != nil {
			c.cmd.Process.Kill()
		}
	}
}

// ---------------------------------------------------------------- client

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type lspClient struct {
	spec       *lspSpec
	root       string
	cmd        *exec.Cmd
	in         io.WriteCloser
	wmu        sync.Mutex // serializes writes
	mu         sync.Mutex
	nextID     int
	pending    map[int]chan rpcMsg
	opened     map[string]time.Time // file -> mtime when sent
	versions   map[string]int
	dead       bool
	ready      chan struct{}
	initErr    error
	pythonPath string
}

func (c *lspClient) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func fileURI(abs string) string { return (&url.URL{Scheme: "file", Path: abs}).String() }

func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

func startLSP(spec *lspSpec, bin string, args []string, root string) *lspClient {
	c := &lspClient{spec: spec, root: root, pending: map[int]chan rpcMsg{},
		opened: map[string]time.Time{}, versions: map[string]int{}, ready: make(chan struct{})}
	c.pythonPath = findVenvPython(root)
	go func() {
		defer close(c.ready)
		c.initErr = c.start(bin, args)
		if c.initErr != nil {
			c.mu.Lock()
			c.dead = true
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *lspClient) start(bin string, args []string) error {
	c.cmd = exec.Command(bin, args...)
	c.cmd.Dir = c.root
	c.cmd.Stderr = io.Discard
	var err error
	if c.in, err = c.cmd.StdinPipe(); err != nil {
		return err
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.cmd.Start(); err != nil {
		return err
	}
	go c.readLoop(out)
	go func() {
		c.cmd.Wait()
		c.mu.Lock()
		c.dead = true
		for id, ch := range c.pending {
			ch <- rpcMsg{Error: &rpcError{Message: c.spec.Name + " exited"}}
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()

	initOpts := map[string]any{}
	if c.spec.Name == "typescript" && filepath.Base(bin) == "typescript-language-server" {
		// Point the TS server at a TypeScript install when the repo has none.
		if ts := globalTSServer(); ts != "" {
			if _, err := os.Stat(filepath.Join(c.root, "node_modules/typescript")); err != nil {
				initOpts["tsserver"] = map[string]any{"path": ts}
			}
		}
	}
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   fileURI(c.root),
		"rootPath":  c.root,
		"workspaceFolders": []map[string]string{
			{"uri": fileURI(c.root), "name": filepath.Base(c.root)},
		},
		"initializationOptions": initOpts,
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
			"workspace": map[string]any{
				"configuration":    true,
				"workspaceFolders": true,
			},
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": false},
				"hover":           map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"definition":      map[string]any{"linkSupport": true},
				"typeDefinition":  map[string]any{"linkSupport": true},
				"references":      map[string]any{},
				"documentSymbol":  map[string]any{"hierarchicalDocumentSymbolSupport": true},
			},
			"window": map[string]any{"workDoneProgress": false},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := c.call(ctx, "initialize", params); err != nil {
		c.cmd.Process.Kill()
		return fmt.Errorf("%s failed to start: %v", c.spec.Name, err)
	}
	return c.notify("initialized", map[string]any{})
}

func globalTSServer() string {
	bin := findBin("typescript-language-server")
	if bin == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	// .../lib/node_modules/typescript-language-server/lib/cli.mjs -> .../lib/node_modules/typescript/lib
	for dir := filepath.Dir(bin); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "node_modules" {
			p := filepath.Join(dir, "typescript", "lib")
			if _, err := os.Stat(filepath.Join(p, "tsserver.js")); err == nil {
				return p
			}
		}
		if filepath.Base(dir) == "bin" {
			p := filepath.Join(filepath.Dir(dir), "lib", "node_modules", "typescript", "lib")
			if _, err := os.Stat(filepath.Join(p, "tsserver.js")); err == nil {
				return p
			}
		}
	}
	return ""
}

// findVenvPython finds a virtualenv at the repo root or one level down, so
// pyright resolves imports the way the project does.
func findVenvPython(root string) string {
	try := func(dir string) string {
		if _, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); err != nil {
			return ""
		}
		for _, py := range []string{"bin/python", "bin/python3", "Scripts/python.exe"} {
			if _, err := os.Stat(filepath.Join(dir, py)); err == nil {
				return filepath.Join(dir, py)
			}
		}
		return ""
	}
	for _, n := range []string{".venv", "venv", "env", ".env"} {
		if p := try(filepath.Join(root, n)); p != "" {
			return p
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if p := try(filepath.Join(root, e.Name())); p != "" {
			return p
		}
		for _, n := range []string{".venv", "venv", "env"} {
			if p := try(filepath.Join(root, e.Name(), n)); p != "" {
				return p
			}
		}
	}
	return ""
}

func (c *lspClient) write(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = c.in.Write(b)
	return err
}

func (c *lspClient) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *lspClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, errors.New(c.spec.Name + " is not running")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.notify("$/cancelRequest", map[string]any{"id": id})
		return nil, ctx.Err()
	}
}

func (c *lspClient) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<16)
	tp := textproto.NewReader(br)
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var m rpcMsg
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // a response to us
			id, err := strconv.Atoi(string(m.ID))
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "" && len(m.ID) > 0: // a request from the server
			c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": c.serverRequest(m)})
		}
		// Notifications (diagnostics, logs, progress) are ignored.
	}
}

// serverRequest answers the few things servers ask clients.
func (c *lspClient) serverRequest(m rpcMsg) any {
	if m.Method != "workspace/configuration" {
		return nil
	}
	var p struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	json.Unmarshal(m.Params, &p)
	out := make([]any, len(p.Items))
	for i, it := range p.Items {
		switch it.Section {
		case "python":
			if c.pythonPath != "" {
				out[i] = map[string]any{"pythonPath": c.pythonPath}
			} else {
				out[i] = map[string]any{}
			}
		case "python.analysis":
			out[i] = map[string]any{"autoSearchPaths": true, "useLibraryCodeForTypes": true, "diagnosticMode": "openFilesOnly"}
		default:
			out[i] = nil
		}
	}
	return out
}

// sync makes sure the server has the current text of abs.
func (c *lspClient) sync(abs string) error {
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	c.mu.Lock()
	sent, ok := c.opened[abs]
	c.mu.Unlock()
	if ok && sent.Equal(st.ModTime()) {
		return nil
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	_, langID := specFor(abs)
	c.mu.Lock()
	c.opened[abs] = st.ModTime()
	c.versions[abs]++
	v := c.versions[abs]
	c.mu.Unlock()
	if !ok {
		return c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": fileURI(abs), "languageId": langID, "version": v, "text": string(src)}})
	}
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(abs), "version": v},
		"contentChanges": []map[string]any{{"text": string(src)}},
	})
}

type lspPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type lspRange struct {
	Start lspPos `json:"start"`
	End   lspPos `json:"end"`
}
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
	// LocationLink fields
	TargetURI            string    `json:"targetUri"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

func posParams(abs string, line, col int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": fileURI(abs)},
		"position":     lspPos{Line: line - 1, Character: col - 1},
	}
}

// parseLocations accepts Location, Location[], LocationLink[] or null.
func parseLocations(raw json.RawMessage) []lspLocation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []lspLocation
	if raw[0] == '{' {
		var one lspLocation
		if json.Unmarshal(raw, &one) == nil {
			list = []lspLocation{one}
		}
	} else {
		json.Unmarshal(raw, &list)
	}
	for i := range list {
		if list[i].TargetURI != "" {
			list[i].URI = list[i].TargetURI
			if list[i].TargetSelectionRange != nil {
				list[i].Range = *list[i].TargetSelectionRange
			}
		}
	}
	return list
}

func (c *lspClient) Definition(ctx context.Context, abs string, line, col int) ([]lspLocation, error) {
	if err := c.sync(abs); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "textDocument/definition", posParams(abs, line, col))
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

func (c *lspClient) References(ctx context.Context, abs string, line, col int) ([]lspLocation, error) {
	if err := c.sync(abs); err != nil {
		return nil, err
	}
	p := posParams(abs, line, col)
	p["context"] = map[string]any{"includeDeclaration": false}
	raw, err := c.call(ctx, "textDocument/references", p)
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// Hover returns the server's description as markdown.
func (c *lspClient) Hover(ctx context.Context, abs string, line, col int) (string, error) {
	if err := c.sync(abs); err != nil {
		return "", err
	}
	raw, err := c.call(ctx, "textDocument/hover", posParams(abs, line, col))
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "", err
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	json.Unmarshal(raw, &h)
	return markedToMarkdown(h.Contents), nil
}

func markedToMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		json.Unmarshal(raw, &s)
		return s
	case '[':
		var parts []json.RawMessage
		json.Unmarshal(raw, &parts)
		var out []string
		for _, p := range parts {
			if s := markedToMarkdown(p); s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n\n")
	case '{':
		var m struct {
			Kind     string `json:"kind"`
			Language string `json:"language"`
			Value    string `json:"value"`
		}
		json.Unmarshal(raw, &m)
		if m.Language != "" {
			return "```" + m.Language + "\n" + m.Value + "\n```"
		}
		return m.Value
	}
	return ""
}

// DocumentSymbols returns a flat outline of abs.
func (c *lspClient) DocumentSymbols(ctx context.Context, abs string) ([]OutlineItem, error) {
	if err := c.sync(abs); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(abs)}})
	if err != nil {
		return nil, err
	}
	type docSym struct {
		Name           string    `json:"name"`
		Kind           int       `json:"kind"`
		SelectionRange *lspRange `json:"selectionRange"`
		Location       *struct {
			Range lspRange `json:"range"`
		} `json:"location"`
		ContainerName string   `json:"containerName"`
		Children      []docSym `json:"children"`
	}
	var syms []docSym
	json.Unmarshal(raw, &syms)
	var out []OutlineItem
	var walk func(list []docSym, parent string, depth int)
	walk = func(list []docSym, parent string, depth int) {
		for _, s := range list {
			k := symbolKind(s.Kind)
			if depth > 0 && (k == "var" || k == "") { // skip locals
				continue
			}
			line := 0
			if s.SelectionRange != nil {
				line = s.SelectionRange.Start.Line + 1
			} else if s.Location != nil {
				line = s.Location.Range.Start.Line + 1
			}
			name := s.Name
			if parent != "" {
				name = parent + "." + name
			} else if s.ContainerName != "" && (k == "method" || k == "field") {
				name = s.ContainerName + "." + name
			}
			if k != "" {
				out = append(out, OutlineItem{N: name, K: k, L: line})
			}
			if depth < 2 && (k == "type" || k == "module") {
				walk(s.Children, name, depth+1)
			}
		}
	}
	walk(syms, "", 0)
	sort.SliceStable(out, func(i, j int) bool { return out[i].L < out[j].L })
	return out, nil
}

// symbolKind maps LSP SymbolKind numbers to the page's outline kinds.
func symbolKind(k int) string {
	switch k {
	case 2, 3, 4: // module, namespace, package
		return "module"
	case 5, 10, 11, 23, 26: // class, enum, interface, struct, type parameter
		return "type"
	case 6, 9: // method, constructor
		return "method"
	case 12: // function
		return "func"
	case 7, 8, 22: // property, field, enum member
		return "field"
	case 14: // constant
		return "const"
	case 13: // variable
		return "var"
	}
	return ""
}
