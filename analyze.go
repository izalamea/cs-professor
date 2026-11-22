package main

// analyze.go type-checks Go code with the standard library's go/types
// package, so the browser can answer "where is this defined?" exactly.
//
// Three kinds of packages are loaded:
//   - repo packages: the folder you opened. Checked with function bodies,
//     recording every identifier (types.Info), so each one is clickable.
//   - external packages: the standard library (under GOROOT) and your module
//     dependencies (under GOMODCACHE). Checked once, without function bodies,
//     only so repo code that uses them gets real types.
//   - external "full" packages: when you jump into, say, net/http/server.go,
//     that package is re-checked with bodies so its code is clickable too.

import (
	"bufio"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Loader owns everything parsed so far. One mutex guards it all: analysis
// is fast enough that a single lock keeps the code simple.
type Loader struct {
	mu        sync.Mutex
	fset      *token.FileSet
	gorootSrc string
	modcache  string

	ext       map[string]*types.Package // external packages by directory
	extByPath map[string]*types.Package // the same, by import path
	extBusy   map[string]bool           // cycle guard
	extFull   map[string]*pkgData       // fully checked external dirs
	extByFile map[string]*pkgData
	fakes     map[string]*types.Package // packages we could not find
	asts      map[string]*ast.File      // absolute filename -> syntax tree
	astGen    map[string]int            // build generation each AST was parsed in
	gen       int
	srcs      map[string][]byte       // absolute filename -> source
	objs      map[string]types.Object // object key -> object
	repos     map[string]*Repo
}

func NewLoader(gorootSrc, modcache string) *Loader {
	return &Loader{
		fset:      token.NewFileSet(),
		gorootSrc: gorootSrc,
		modcache:  modcache,
		ext:       map[string]*types.Package{},
		extByPath: map[string]*types.Package{},
		extBusy:   map[string]bool{},
		extFull:   map[string]*pkgData{},
		extByFile: map[string]*pkgData{},
		fakes:     map[string]*types.Package{},
		asts:      map[string]*ast.File{},
		astGen:    map[string]int{},
		srcs:      map[string][]byte{},
		objs:      map[string]types.Object{},
		repos:     map[string]*Repo{},
	}
}

// pkgData is one type-checked package plus what we recorded about it.
type pkgData struct {
	dir, path string
	pkg       *types.Package
	info      *types.Info // nil for packages checked only for their API
	files     []*ast.File
}

type Module struct{ Path, Dir string }

type Repo struct {
	l        *Loader
	Root     string
	fp       string
	modules  []Module          // go.mod files inside the repo, longest path first
	requires map[string]string // module path -> version (from go.mod)
	replaces map[string]string // module path -> local directory
	goDirs   map[string]bool
	dirFiles map[string]dirFiles
	imp      map[string]*pkgData // API-only version of each dir (what importers see)
	impBusy  map[string]bool
	full     []*pkgData          // fully checked packages (with Info)
	byFile   map[string]*pkgData // absolute filename -> its full package
	refs     map[string][]Ref    // object key -> every use in the repo
}

type Ref struct {
	P    string `json:"p"`
	L    int    `json:"l"`
	C    int    `json:"c"`
	Text string `json:"text"`
	Def  bool   `json:"def,omitempty"`
}

type dirFiles struct {
	gofiles, tests, xtests []string
}

// ---------------------------------------------------------------- repo scan

var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "testdata": true}

// scanRepo finds every directory holding .go files, every go.mod, and a
// fingerprint that changes whenever a .go file is added, removed or edited.
func scanRepo(root string) (fp string, goDirs map[string]bool, gomods []string) {
	h := fnv.New64a()
	goDirs = map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") || name == "go.mod" {
			if info, err := d.Info(); err == nil {
				h.Write([]byte(p))
				h.Write([]byte(strconv.FormatInt(info.Size(), 10)))
				h.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
			}
			if name == "go.mod" {
				gomods = append(gomods, p)
			} else {
				goDirs[filepath.Dir(p)] = true
			}
		}
		return nil
	})
	return strconv.FormatUint(h.Sum64(), 16), goDirs, gomods
}

// parseGoMod reads the few go.mod directives we need. (The real parser lives
// in golang.org/x/mod; we stay standard-library only.)
func parseGoMod(path string, r *Repo) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	dir := filepath.Dir(path)
	block := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == ")" {
			block = ""
			continue
		}
		if len(fields) == 2 && fields[1] == "(" {
			block = fields[0]
			continue
		}
		verb := block
		if verb == "" {
			verb, fields = fields[0], fields[1:]
		}
		switch verb {
		case "module":
			if len(fields) > 0 {
				r.modules = append(r.modules, Module{Path: strings.Trim(fields[0], `"`), Dir: dir})
			}
		case "require":
			if len(fields) >= 2 {
				r.requires[fields[0]] = fields[1]
			}
		case "replace":
			// old [v] => new [v]; we only follow replacements by a local path.
			for i, f := range fields {
				if f == "=>" && i+1 < len(fields) {
					target := fields[i+1]
					if strings.HasPrefix(target, ".") || strings.HasPrefix(target, "/") {
						if !filepath.IsAbs(target) {
							target = filepath.Join(dir, target)
						}
						r.replaces[fields[0]] = target
					} else if i+2 < len(fields) {
						r.requires[fields[0]] = fields[i+2]
					}
				}
			}
		}
	}
}

// Repo returns the analysis of root, redoing it only if Go files changed.
// The caller must hold l.mu.
func (l *Loader) Repo(root string) *Repo {
	fp, goDirs, gomods := scanRepo(root)
	if r := l.repos[root]; r != nil && r.fp == fp {
		return r
	}
	l.gen++
	r := &Repo{
		l: l, Root: root, fp: fp, goDirs: goDirs,
		requires: map[string]string{}, replaces: map[string]string{},
		dirFiles: map[string]dirFiles{}, imp: map[string]*pkgData{}, impBusy: map[string]bool{},
		byFile: map[string]*pkgData{}, refs: map[string][]Ref{},
	}
	for _, m := range gomods {
		parseGoMod(m, r)
	}
	sort.Slice(r.modules, func(i, j int) bool { return len(r.modules[i].Path) > len(r.modules[j].Path) })

	dirs := make([]string, 0, len(goDirs))
	for d := range goDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		df := r.files(dir)
		path := r.importPath(dir)
		im := importer{r}
		r.importable(dir, path)
		if len(df.gofiles)+len(df.tests) > 0 {
			all := append(append([]string{}, df.gofiles...), df.tests...)
			p := l.check(path, dir, all, im, true, true, true)
			r.full = append(r.full, p)
		}
		if len(df.xtests) > 0 {
			p := l.check(path+"_test", dir, df.xtests, im, true, true, true)
			r.full = append(r.full, p)
		}
	}
	for _, p := range r.full {
		for _, f := range p.files {
			r.byFile[l.fset.File(f.Pos()).Name()] = p
		}
		r.index(p)
	}
	for k := range r.refs {
		refs := r.refs[k]
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].P != refs[j].P {
				return refs[i].P < refs[j].P
			}
			return refs[i].L < refs[j].L
		})
	}
	l.repos[root] = r
	return r
}

// index records every identifier's object so "find uses" is a map lookup.
func (r *Repo) index(p *pkgData) {
	l := r.l
	add := func(id *ast.Ident, obj types.Object, def bool) {
		key := l.key(obj)
		if key == "" {
			return
		}
		if _, ok := l.objs[key]; !ok {
			l.objs[key] = obj
		}
		pos := l.fset.Position(id.Pos())
		r.refs[key] = append(r.refs[key], Ref{
			P: r.display(pos.Filename), L: pos.Line, C: pos.Column,
			Text: lineText(l.srcs[pos.Filename], pos.Offset), Def: def,
		})
	}
	for id, obj := range p.info.Defs {
		if obj != nil {
			add(id, obj, true)
		}
	}
	for id, obj := range p.info.Uses {
		add(id, obj, false)
	}
}

func lineText(src []byte, off int) string {
	if off > len(src) {
		return ""
	}
	s, e := off, off
	for s > 0 && src[s-1] != '\n' {
		s--
	}
	for e < len(src) && src[e] != '\n' {
		e++
	}
	t := strings.TrimSpace(string(src[s:e]))
	if len(t) > 160 {
		t = t[:160] + "…"
	}
	return t
}

// files lists which .go files in dir belong to the build (honoring build
// tags such as _windows.go), split into package, in-package test and
// external-test (package foo_test) files.
func (r *Repo) files(dir string) dirFiles {
	if df, ok := r.dirFiles[dir]; ok {
		return df
	}
	var df dirFiles
	bp, err := build.Default.ImportDir(dir, 0)
	var mpe *build.MultiplePackageError
	switch {
	case err == nil:
		df = dirFiles{bp.GoFiles, bp.TestGoFiles, bp.XTestGoFiles}
	case errors.As(err, &mpe):
		df = groupByPackage(dir)
	default:
		if bp != nil {
			df = dirFiles{bp.GoFiles, bp.TestGoFiles, bp.XTestGoFiles}
		}
	}
	r.dirFiles[dir] = df
	return df
}

// groupByPackage handles folders that mix packages (often stray main
// programs): keep the most common package name.
func groupByPackage(dir string) dirFiles {
	entries, _ := os.ReadDir(dir)
	names := map[string][]string{}
	count := map[string]int{}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.PackageClauseOnly)
		if err != nil {
			continue
		}
		names[f.Name.Name] = append(names[f.Name.Name], n)
		if !strings.HasSuffix(n, "_test.go") {
			count[f.Name.Name]++
		}
	}
	best := ""
	for n, c := range count {
		if c > count[best] || (c == count[best] && n < best) {
			best = n
		}
	}
	var df dirFiles
	for _, n := range names[best] {
		if strings.HasSuffix(n, "_test.go") {
			df.tests = append(df.tests, n)
		} else {
			df.gofiles = append(df.gofiles, n)
		}
	}
	df.xtests = names[best+"_test"]
	return df
}

func (r *Repo) importPath(dir string) string {
	for _, m := range r.modules {
		if dir == m.Dir {
			return m.Path
		}
		if rel, err := filepath.Rel(m.Dir, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return m.Path + "/" + filepath.ToSlash(rel)
		}
	}
	rel, _ := filepath.Rel(r.Root, dir)
	return "_/" + filepath.ToSlash(rel)
}

// display turns an absolute filename into what the page shows: relative to
// the repo when inside it, absolute otherwise.
func (r *Repo) display(abs string) string {
	if rel, err := filepath.Rel(r.Root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return abs
}

// ---------------------------------------------------------------- checking

func (l *Loader) parse(abs string, fresh bool) *ast.File {
	// fresh files are re-read once per repo rebuild; external files never.
	if f, ok := l.asts[abs]; ok && (!fresh || l.astGen[abs] == l.gen) {
		return f
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	f, _ := parser.ParseFile(l.fset, abs, src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil {
		return nil
	}
	l.srcs[abs] = src
	l.asts[abs] = f
	l.astGen[abs] = l.gen
	return f
}

// check type-checks one package. Type errors are ignored on purpose: a
// missing dependency should not stop the rest of the file from resolving.
func (l *Loader) check(path, dir string, names []string, im importer, bodies, withInfo, fresh bool) *pkgData {
	var files []*ast.File
	for _, n := range names {
		if f := l.parse(filepath.Join(dir, n), fresh); f != nil {
			files = append(files, f)
		}
	}
	conf := types.Config{
		Importer:         im,
		IgnoreFuncBodies: !bodies,
		FakeImportC:      true,
		Error:            func(error) {},
	}
	var info *types.Info
	if withInfo {
		info = &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Implicits:  map[ast.Node]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
	}
	pkg, _ := conf.Check(path, l.fset, files, info)
	return &pkgData{dir: dir, path: path, pkg: pkg, info: info, files: files}
}

// importer resolves import paths for go/types. It implements
// types.ImporterFrom so it learns which directory is importing (needed for
// the standard library's vendored golang.org/x packages).
type importer struct{ r *Repo }

func (im importer) Import(path string) (*types.Package, error) {
	return im.ImportFrom(path, im.r.Root, 0)
}

func (im importer) ImportFrom(path, srcDir string, _ types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	r, l := im.r, im.r.l
	if dir := r.repoDir(path); dir != "" {
		if p := r.importable(dir, path); p != nil {
			return p, nil
		}
		return l.fake(path), nil
	}
	if dir := l.resolveExt(path, srcDir, r); dir != "" {
		return l.extPkg(path, dir, r), nil
	}
	return l.fake(path), nil
}

// repoDir maps an import path to a folder inside the repo, if it is one.
func (r *Repo) repoDir(path string) string {
	for _, m := range r.modules {
		if path == m.Path || strings.HasPrefix(path, m.Path+"/") {
			dir := filepath.Join(m.Dir, filepath.FromSlash(strings.TrimPrefix(path, m.Path)))
			if r.goDirs[dir] {
				return dir
			}
		}
	}
	return ""
}

// importable is the API-only (no function bodies) version of a repo
// package. Using a separate, lighter check here avoids import cycles that
// test files can create.
func (r *Repo) importable(dir, path string) *types.Package {
	if p, ok := r.imp[dir]; ok {
		return p.pkg
	}
	if r.impBusy[dir] {
		return nil
	}
	r.impBusy[dir] = true
	defer delete(r.impBusy, dir)
	p := r.l.check(path, dir, r.files(dir).gofiles, importer{r}, false, false, true)
	r.imp[dir] = p
	return p.pkg
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// escapeModPath applies the module cache's case encoding: "Azure" -> "!azure".
func escapeModPath(p string) string {
	var b strings.Builder
	for _, c := range p {
		if 'A' <= c && c <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(c + 'a' - 'A')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// resolveExt finds the source folder of a standard-library or dependency
// package, or returns "".
func (l *Loader) resolveExt(path, srcDir string, r *Repo) string {
	inStd := strings.HasPrefix(srcDir, l.gorootSrc+string(filepath.Separator))
	if inStd {
		if v := filepath.Join(l.gorootSrc, "vendor", path); isDir(v) {
			return v
		}
	}
	first := strings.SplitN(path, "/", 2)[0]
	if !strings.Contains(first, ".") {
		if d := filepath.Join(l.gorootSrc, path); isDir(d) {
			return d
		}
		return ""
	}
	if v := filepath.Join(r.Root, "vendor", path); isDir(v) {
		return v
	}
	best := ""
	for m := range r.replaces {
		if (path == m || strings.HasPrefix(path, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best != "" {
		if d := filepath.Join(r.replaces[best], strings.TrimPrefix(path, best)); isDir(d) {
			return d
		}
	}
	best = ""
	for m := range r.requires {
		if (path == m || strings.HasPrefix(path, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best != "" && l.modcache != "" {
		d := filepath.Join(l.modcache, escapeModPath(best)+"@"+r.requires[best], strings.TrimPrefix(path, best))
		if isDir(d) {
			return d
		}
	}
	// Not in go.mod (or a different version is cached): take the newest
	// cached version of the longest matching module path.
	if l.modcache == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	for i := len(parts); i >= 2; i-- {
		prefix := strings.Join(parts[:i], "/")
		matches, _ := filepath.Glob(filepath.Join(l.modcache, escapeModPath(prefix)+"@*"))
		if len(matches) > 0 {
			sort.Strings(matches)
			d := filepath.Join(append([]string{matches[len(matches)-1]}, parts[i:]...)...)
			if isDir(d) {
				return d
			}
		}
	}
	return ""
}

func (l *Loader) extPkg(path, dir string, r *Repo) *types.Package {
	if p, ok := l.ext[dir]; ok {
		return p
	}
	if l.extBusy[dir] {
		return l.fake(path)
	}
	l.extBusy[dir] = true
	defer delete(l.extBusy, dir)
	bp, _ := build.Default.ImportDir(dir, 0)
	if bp == nil || len(bp.GoFiles) == 0 {
		return l.fake(path)
	}
	p := l.check(path, dir, bp.GoFiles, importer{r}, false, false, false)
	l.ext[dir] = p.pkg
	l.extByPath[path] = p.pkg
	return p.pkg
}

// fake stands in for a package whose source isn't on disk (dependencies not
// downloaded yet). Its name is a guess from the path.
func (l *Loader) fake(path string) *types.Package {
	if p, ok := l.fakes[path]; ok {
		return p
	}
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) >= 2 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	name = strings.TrimPrefix(name, "go-")
	if i := strings.IndexAny(name, "-."); i > 0 {
		name = name[:i]
	}
	p := types.NewPackage(path, name)
	p.MarkComplete()
	l.fakes[path] = p
	return p
}

// extFullFor fully checks the external package holding abs, so a standard
// library file you jumped into is clickable too.
func (l *Loader) extFullFor(abs string, r *Repo) *pkgData {
	if p, ok := l.extByFile[abs]; ok {
		return p
	}
	dir := filepath.Dir(abs)
	if p, ok := l.extFull[dir]; ok {
		return p
	}
	path := ""
	if rel, err := filepath.Rel(l.gorootSrc, dir); err == nil && !strings.HasPrefix(rel, "..") {
		path = strings.TrimPrefix(filepath.ToSlash(rel), "vendor/")
	} else if rel, err := filepath.Rel(l.modcache, dir); err == nil && !strings.HasPrefix(rel, "..") {
		segs := strings.Split(filepath.ToSlash(rel), "/")
		for i, s := range segs {
			if j := strings.Index(s, "@"); j >= 0 {
				segs[i] = s[:j]
			}
		}
		path = strings.Join(segs, "/")
		var b strings.Builder
		for i := 0; i < len(path); i++ {
			if path[i] == '!' && i+1 < len(path) {
				i++
				b.WriteByte(path[i] - 'a' + 'A')
			} else {
				b.WriteByte(path[i])
			}
		}
		path = b.String()
	} else {
		return nil
	}
	bp, _ := build.Default.ImportDir(dir, 0)
	if bp == nil {
		return nil
	}
	p := l.check(path, dir, bp.GoFiles, importer{r}, true, true, false)
	l.extFull[dir] = p
	for _, f := range p.files {
		l.extByFile[l.fset.File(f.Pos()).Name()] = p
	}
	return p
}

// key identifies an object by where it is declared, which stays the same
// across re-checks of the same file.
func (l *Loader) key(obj types.Object) string {
	if obj == nil || !obj.Pos().IsValid() {
		return ""
	}
	p := l.fset.Position(obj.Pos())
	return p.Filename + ":" + strconv.Itoa(p.Offset)
}

// ---------------------------------------------------------------- file view

type Loc struct {
	P string `json:"p"`
	L int    `json:"l"`
	C int    `json:"c"`
}

// Ident is what the page knows about one clickable identifier.
type Ident struct {
	K   string `json:"k"`             // var, param, field, func, method, type, const, pkg, label, builtin, nil, ext
	N   string `json:"n"`             // name
	T   string `json:"t,omitempty"`   // type or signature
	Def bool   `json:"def,omitempty"` // this occurrence is the declaration
	Loc *Loc   `json:"loc,omitempty"` // where it is declared
	Key string `json:"key,omitempty"` // for /api/sym
	Pkg string `json:"pkg,omitempty"` // package path of the declaration
	U   bool   `json:"u,omitempty"`   // predeclared (universe) object
}

type OutlineItem struct {
	N string `json:"n"`
	K string `json:"k"`
	L int    `json:"l"`
}

type GoView struct {
	Tokens  [][4]int      `json:"tokens"`
	Idents  []Ident       `json:"idents"`
	Outline []OutlineItem `json:"outline"`
	Pkg     string        `json:"pkg,omitempty"`
	Note    string        `json:"note,omitempty"`
}

// Token classes, mirrored in the page's JavaScript.
const (
	cKeyword = iota
	cString
	cNumber
	cComment
	cOperator
	cIdent
)

func qualifierFor(pkg *types.Package) types.Qualifier {
	return func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		return p.Name()
	}
}

func (l *Loader) identFor(r *Repo, obj types.Object, def bool, from *types.Package) Ident {
	id := Ident{N: obj.Name(), Def: def, Key: l.key(obj)}
	if id.Key != "" {
		if _, ok := l.objs[id.Key]; !ok {
			l.objs[id.Key] = obj
		}
	}
	q := qualifierFor(from)
	switch o := obj.(type) {
	case *types.PkgName:
		id.K, id.Pkg = "pkg", o.Imported().Path()
	case *types.Var:
		id.K, id.T = "var", types.TypeString(o.Type(), q)
		if o.IsField() {
			id.K = "field"
		}
	case *types.Func:
		id.K, id.T = "func", types.TypeString(o.Type(), q)
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			id.K = "method"
		}
	case *types.TypeName:
		id.K, id.T = "type", shortUnderlying(o, q)
	case *types.Const:
		id.K, id.T = "const", types.TypeString(o.Type(), q)
		if v := o.Val().ExactString(); len(v) < 60 {
			id.T += " = " + v
		}
	case *types.Label:
		id.K = "label"
	case *types.Builtin:
		id.K = "builtin"
	case *types.Nil:
		id.K = "nil"
	}
	if obj.Parent() == types.Universe {
		id.U = true
	}
	if obj.Pkg() != nil && id.K != "pkg" {
		id.Pkg = obj.Pkg().Path()
	}
	if obj.Pos().IsValid() {
		p := l.fset.Position(obj.Pos())
		id.Loc = &Loc{P: r.display(p.Filename), L: p.Line, C: p.Column}
	}
	return id
}

func shortUnderlying(o *types.TypeName, q types.Qualifier) string {
	if o.IsAlias() {
		return "= " + types.TypeString(o.Type(), q)
	}
	switch u := o.Type().Underlying().(type) {
	case *types.Struct:
		return "struct (" + strconv.Itoa(u.NumFields()) + " fields)"
	case *types.Interface:
		return "interface (" + strconv.Itoa(u.NumMethods()) + " methods)"
	default:
		s := types.TypeString(u, q)
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	}
}

// GoFile builds the view of one Go file: tokens for highlighting, and for
// every identifier, what it refers to.
func (l *Loader) GoFile(r *Repo, abs string, src []byte) *GoView {
	v := &GoView{}
	p := r.byFile[abs]
	if p == nil && !strings.HasPrefix(abs, r.Root+string(filepath.Separator)) {
		p = l.extFullFor(abs, r)
	}
	var f *ast.File
	if p != nil {
		for _, pf := range p.files {
			if l.fset.File(pf.Pos()).Name() == abs {
				f = pf
				break
			}
		}
	}
	identAt := map[int]int{} // byte offset -> index in v.Idents
	if f != nil && p.info != nil {
		v.Pkg = p.path
		tf := l.fset.File(f.Pos())
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				// pkg.Name where pkg's source is missing: still say which package.
				if xid, ok := x.X.(*ast.Ident); ok {
					if pn, ok := p.info.Uses[xid].(*types.PkgName); ok && p.info.Uses[x.Sel] == nil {
						identAt[tf.Offset(x.Sel.Pos())] = len(v.Idents)
						v.Idents = append(v.Idents, Ident{K: "ext", N: x.Sel.Name, Pkg: pn.Imported().Path()})
					}
				}
			case *ast.Ident:
				obj, def := p.info.Defs[x], true
				if obj == nil {
					obj, def = p.info.Uses[x], false
				}
				if obj == nil {
					return true
				}
				off := tf.Offset(x.Pos())
				if _, done := identAt[off]; done {
					return true
				}
				identAt[off] = len(v.Idents)
				v.Idents = append(v.Idents, l.identFor(r, obj, def, p.pkg))
			}
			return true
		})
		v.Outline = outline(l.fset, f)
	} else {
		v.Note = "No type information for this file (it may be excluded by build tags or have syntax errors)."
	}
	v.Tokens = tokenize(src, identAt)
	return v
}

func outline(fset *token.FileSet, f *ast.File) []OutlineItem {
	var out []OutlineItem
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			item := OutlineItem{N: d.Name.Name, K: "func", L: fset.Position(d.Pos()).Line}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				item.K = "method"
				item.N = recvName(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			out = append(out, item)
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					out = append(out, OutlineItem{N: s.Name.Name, K: "type", L: fset.Position(s.Pos()).Line})
				case *ast.ValueSpec:
					k := "var"
					if d.Tok == token.CONST {
						k = "const"
					}
					for _, n := range s.Names {
						if n.Name != "_" {
							out = append(out, OutlineItem{N: n.Name, K: k, L: fset.Position(n.Pos()).Line})
						}
					}
				}
			}
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// tokenize runs Go's own scanner over the file. Offsets are converted to
// UTF-16 units because that is how JavaScript indexes strings.
func tokenize(src []byte, identAt map[int]int) [][4]int {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, src, func(token.Position, string) {}, scanner.ScanComments)
	conv := utf16Index(src)
	var out [][4]int
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit != ";" {
			continue // inserted automatically at line ends; not in the text
		}
		off := file.Offset(pos)
		n := len(lit)
		if n == 0 {
			n = len(tok.String())
		}
		end := off + n
		if end > len(src) {
			end = len(src)
		}
		cls, idx := cOperator, -1
		switch {
		case tok == token.COMMENT:
			cls = cComment
		case tok == token.STRING || tok == token.CHAR:
			cls = cString
		case tok == token.INT || tok == token.FLOAT || tok == token.IMAG:
			cls = cNumber
		case tok == token.IDENT:
			cls = cIdent
			if i, ok := identAt[off]; ok {
				idx = i
			}
		case tok.IsKeyword():
			cls = cKeyword
		}
		out = append(out, [4]int{conv(off), conv(end), cls, idx})
	}
	return out
}

func utf16Index(src []byte) func(int) int {
	ascii := true
	for _, b := range src {
		if b >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return func(i int) int { return i }
	}
	m := make([]int, len(src)+1)
	u := 0
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		for j := 0; j < size; j++ {
			m[i+j] = u
		}
		if r >= 0x10000 {
			u += 2
		} else {
			u++
		}
		i += size
	}
	m[len(src)] = u
	return func(i int) int { return m[i] }
}

// ---------------------------------------------------------------- symbol detail

type Member struct {
	N   string `json:"n"`
	T   string `json:"t,omitempty"`
	Loc *Loc   `json:"loc,omitempty"`
	Ptr bool   `json:"ptr,omitempty"`
}

type SymDetail struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Type          string   `json:"type,omitempty"`
	Pkg           string   `json:"pkg,omitempty"`
	Loc           *Loc     `json:"loc,omitempty"`
	Doc           string   `json:"doc,omitempty"`
	Decl          string   `json:"decl,omitempty"`
	DeclLine      int      `json:"declLine,omitempty"`
	DocURL        string   `json:"docURL,omitempty"`
	Methods       []Member `json:"methods,omitempty"`
	Implements    []Member `json:"implements,omitempty"`
	ImplementedBy []Member `json:"implementedBy,omitempty"`
	Refs          []Ref    `json:"refs"`
	RefCount      int      `json:"refCount"`
}

func (l *Loader) Sym(r *Repo, key string) *SymDetail {
	obj := l.objs[key]
	if obj == nil {
		return nil
	}
	id := l.identFor(r, obj, false, obj.Pkg())
	d := &SymDetail{Name: obj.Name(), Kind: id.K, Type: id.T, Pkg: id.Pkg, Loc: id.Loc}
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			d.Type = "(" + types.TypeString(sig.Recv().Type(), qualifierFor(obj.Pkg())) + ") " + d.Type
		}
	}
	if tn, ok := obj.(*types.TypeName); ok && !tn.IsAlias() {
		d.Type = types.TypeString(obj.Type().Underlying(), qualifierFor(obj.Pkg()))
		if len(d.Type) > 300 {
			d.Type = d.Type[:300] + "…"
		}
	}
	d.Doc, d.Decl, d.DeclLine = l.declOf(obj)
	inRepo := id.Loc != nil && !filepath.IsAbs(id.Loc.P)
	if obj.Pkg() != nil && !inRepo {
		d.DocURL = "https://pkg.go.dev/" + obj.Pkg().Path() + "#" + docAnchor(obj)
	}
	if tn, ok := obj.(*types.TypeName); ok {
		l.typeRelations(r, tn, d)
	}
	refs := r.refs[key]
	d.RefCount = len(refs)
	if len(refs) > 300 {
		refs = refs[:300]
	}
	d.Refs = refs
	if d.Refs == nil {
		d.Refs = []Ref{}
	}
	return d
}

func docAnchor(obj types.Object) string {
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			t := sig.Recv().Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			if n, ok := t.(*types.Named); ok {
				return n.Obj().Name() + "." + obj.Name()
			}
		}
	}
	return obj.Name()
}

// declOf finds the doc comment and declaration source of obj.
func (l *Loader) declOf(obj types.Object) (doc, decl string, line int) {
	if !obj.Pos().IsValid() {
		return
	}
	pos := obj.Pos()
	fname := l.fset.Position(pos).Filename
	f, src := l.asts[fname], l.srcs[fname]
	if f == nil || src == nil {
		return
	}
	var docGroup *ast.CommentGroup
	var node ast.Node
	var start, end token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		if node != nil || n == nil {
			return false
		}
		if n.Pos() > pos || n.End() < pos {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Pos() == pos {
				docGroup, node, start, end = x.Doc, x, x.Pos(), x.Type.End()
				return false
			}
		case *ast.GenDecl:
			for _, s := range x.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.Pos() == pos {
						docGroup = s.Doc
						if docGroup == nil && !x.Lparen.IsValid() {
							docGroup = x.Doc
						}
						node, start, end = s, s.Pos(), s.End()
						if !x.Lparen.IsValid() {
							start = x.Pos()
						}
					}
				case *ast.ValueSpec:
					for _, nm := range s.Names {
						if nm.Pos() == pos {
							docGroup = s.Doc
							if docGroup == nil {
								docGroup = s.Comment
							}
							if docGroup == nil && !x.Lparen.IsValid() {
								docGroup = x.Doc
							}
							node, start, end = s, s.Pos(), s.End()
							if !x.Lparen.IsValid() {
								start = x.Pos()
							}
						}
					}
				case *ast.ImportSpec:
					if s.Pos() <= pos && pos <= s.End() {
						node, start, end = s, s.Pos(), s.End()
					}
				}
			}
			if node != nil {
				return false
			}
		case *ast.Field:
			for _, nm := range x.Names {
				if nm.Pos() == pos {
					docGroup = x.Doc
					if docGroup == nil {
						docGroup = x.Comment
					}
					node, start, end = x, x.Pos(), x.End()
					return false
				}
			}
			if len(x.Names) == 0 && x.Type.Pos() <= pos && pos < x.Type.End() { // embedded
				node, start, end = x, x.Pos(), x.End()
				return false
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if lhs.Pos() == pos {
					node, start, end = x, x.Pos(), x.End()
					return false
				}
			}
		case *ast.RangeStmt:
			if (x.Key != nil && x.Key.Pos() == pos) || (x.Value != nil && x.Value.Pos() == pos) {
				node, start, end = x, x.Pos(), x.X.End()
				return false
			}
		}
		return true
	})
	if docGroup != nil {
		doc = strings.TrimSpace(docGroup.Text())
	}
	tf := l.fset.File(pos)
	if node == nil {
		// Fall back to the declaring line.
		start, end = pos, pos
	}
	s, e := tf.Offset(start), tf.Offset(end)
	for s > 0 && src[s-1] != '\n' { // include the line's indentation
		s--
	}
	for e < len(src) && src[e] != '\n' && node == nil {
		e++
	}
	text := string(src[s:e])
	lines := strings.Split(text, "\n")
	if len(lines) > 40 {
		lines = append(lines[:40], "\t// …")
	}
	return doc, dedent(lines), tf.Line(start)
}

func dedent(lines []string) string {
	prefix, first := "", true
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		ind := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		if first {
			prefix, first = ind, false
		} else {
			for !strings.HasPrefix(ind, prefix) {
				prefix = prefix[:len(prefix)-1]
			}
		}
	}
	for i := range lines {
		lines[i] = strings.TrimPrefix(lines[i], prefix)
	}
	return strings.Join(lines, "\n")
}

// typeRelations lists a type's methods and which interfaces it satisfies
// (or, for an interface, which repo types satisfy it).
func (l *Loader) typeRelations(r *Repo, tn *types.TypeName, d *SymDetail) {
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}
	// Use the API-only copy of the type, so it compares equal with the
	// types other packages see.
	if obj := l.canonical(r, tn); obj != nil {
		if n, ok := obj.Type().(*types.Named); ok {
			named = n
		}
	}
	q := qualifierFor(tn.Pkg())
	ms := types.NewMethodSet(types.NewPointer(named))
	if types.IsInterface(named) {
		ms = types.NewMethodSet(named)
	}
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i).Obj()
		sig := strings.TrimPrefix(types.TypeString(m.Type(), q), "func")
		mem := Member{N: m.Name(), T: sig}
		if m.Pos().IsValid() {
			p := l.fset.Position(m.Pos())
			mem.Loc = &Loc{P: r.display(p.Filename), L: p.Line, C: p.Column}
		}
		if len(ms.At(i).Index()) > 1 {
			mem.T += "   (promoted from embedded field)"
		}
		d.Methods = append(d.Methods, mem)
	}
	if named.TypeParams().Len() > 0 {
		return
	}
	member := func(obj types.Object, ptr bool) Member {
		m := Member{N: obj.Pkg().Name() + "." + obj.Name(), Ptr: ptr}
		if obj.Pkg() == nil {
			m.N = obj.Name()
		}
		if obj.Pos().IsValid() {
			p := l.fset.Position(obj.Pos())
			m.Loc = &Loc{P: r.display(p.Filename), L: p.Line, C: p.Column}
		}
		return m
	}
	if iface, ok := named.Underlying().(*types.Interface); ok {
		if iface.NumMethods() == 0 {
			return
		}
		for _, t := range l.repoTypes(r) {
			if types.IsInterface(t.Type()) || t == named.Obj() {
				continue
			}
			if types.Implements(t.Type(), iface) {
				d.ImplementedBy = append(d.ImplementedBy, member(t, false))
			} else if types.Implements(types.NewPointer(t.Type()), iface) {
				d.ImplementedBy = append(d.ImplementedBy, member(t, true))
			}
		}
		return
	}
	cands := l.repoTypes(r)
	for _, w := range wellKnown {
		if p := l.extByPath[w[0]]; p != nil {
			if o, ok := p.Scope().Lookup(w[1]).(*types.TypeName); ok {
				cands = append(cands, o)
			}
		}
	}
	errObj := types.Universe.Lookup("error").(*types.TypeName)
	cands = append(cands, errObj)
	for _, c := range cands {
		iface, ok := c.Type().Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 || c == named.Obj() {
			continue
		}
		var m Member
		switch {
		case types.Implements(named, iface):
			m = member(c, false)
		case types.Implements(types.NewPointer(named), iface):
			m = member(c, true)
		default:
			continue
		}
		if c == errObj {
			m.N = "error"
		}
		d.Implements = append(d.Implements, m)
	}
}

var wellKnown = [][2]string{
	{"fmt", "Stringer"}, {"io", "Reader"}, {"io", "Writer"}, {"io", "Closer"},
	{"io", "ReadCloser"}, {"io", "ReadWriter"}, {"sort", "Interface"},
	{"net/http", "Handler"}, {"net/http", "RoundTripper"}, {"encoding/json", "Marshaler"},
	{"encoding/json", "Unmarshaler"}, {"encoding", "TextMarshaler"}, {"context", "Context"},
}

func (l *Loader) canonical(r *Repo, tn *types.TypeName) types.Object {
	if tn.Pkg() == nil {
		return nil
	}
	if dir := r.repoDir(tn.Pkg().Path()); dir != "" {
		if p := r.imp[dir]; p != nil {
			return p.pkg.Scope().Lookup(tn.Name())
		}
	}
	if p := l.extByPath[tn.Pkg().Path()]; p != nil {
		return p.Scope().Lookup(tn.Name())
	}
	return nil
}

// repoTypes is every package-level named type in the repo (API copies).
func (l *Loader) repoTypes(r *Repo) []*types.TypeName {
	var out []*types.TypeName
	dirs := make([]string, 0, len(r.imp))
	for d := range r.imp {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		p := r.imp[d]
		if p.pkg == nil {
			continue
		}
		for _, n := range p.pkg.Scope().Names() {
			if tn, ok := p.pkg.Scope().Lookup(n).(*types.TypeName); ok && !tn.IsAlias() {
				if nt, ok := tn.Type().(*types.Named); ok && nt.TypeParams().Len() == 0 {
					out = append(out, tn)
				}
			}
		}
	}
	return out
}
