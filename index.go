package main

// index.go is the fallback when no language server can answer: a quick,
// regex-based index of declarations (def/class/function/struct/...) by name.
// It can't tell two things with the same name apart, so the page labels its
// answers as "by name".

import (
	"bufio"
	"bytes"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Decl struct {
	N    string `json:"n"`
	K    string `json:"k"`
	P    string `json:"p"`
	L    int    `json:"l"`
	C    int    `json:"c"`
	Text string `json:"text"`
}

// declRule: group 1 = indentation, group NameGroup = the name.
type declRule struct {
	re        *regexp.Regexp
	kind      string // fixed kind, or "" to use group 2 as the kind
	nameGroup int
	topLevel  bool // only at column 0
	maxIndent int  // >0: only up to this indentation (type members)
}

func rule(re, kind string, nameGroup int) declRule {
	return declRule{re: regexp.MustCompile(re), kind: kind, nameGroup: nameGroup}
}

var notNames = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "function": true, "else": true, "do": true, "sizeof": true, "with": true, "elif": true}

var declRules = map[string][]declRule{
	"py": {
		rule(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)`, "func", 2),
		rule(`^(\s*)class\s+([A-Za-z_]\w*)`, "type", 2),
		{re: regexp.MustCompile(`^()([A-Za-z_]\w*)\s*(?::[^=\n]*)?=[^=]`), kind: "var", nameGroup: 2, topLevel: true},
	},
	"js": {
		rule(`^(\s*)(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`, "func", 2),
		rule(`^(\s*)(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`, "type", 2),
		rule(`^(\s*)(?:export\s+)?(?:declare\s+)?(?:interface|type|enum)\s+([A-Za-z_$][\w$]*)`, "type", 2),
		{re: regexp.MustCompile(`^(\s*)(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)`), kind: "var", nameGroup: 2, maxIndent: 0, topLevel: true},
		{re: regexp.MustCompile(`^(\s+)(?:(?:public|private|protected|static|async|readonly|get|set|override)\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::[^{=]*)?\{`), kind: "method", nameGroup: 2, maxIndent: 4},
	},
	"swift": {
		{re: regexp.MustCompile(`^(\s*)(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|private|fileprivate|internal|open|final|static|class|override|mutating|nonisolated|convenience|required|lazy|weak|unowned|indirect|dynamic)\s+)*(func|class|struct|enum|protocol|extension|actor|typealias|let|var|case)\s+([A-Za-z_]\w*)`), nameGroup: 3, maxIndent: 8},
	},
	"c": {
		rule(`^()#\s*define\s+([A-Za-z_]\w*)`, "const", 2),
		rule(`^()(?:typedef\s+)?(?:struct|enum|union)\s+([A-Za-z_]\w*)\s*\{`, "type", 2),
		rule(`^()\}\s*([A-Za-z_]\w*)\s*;`, "type", 2),
		rule(`^()(?:[A-Za-z_][\w]*[\s\*]+)+([A-Za-z_]\w*)\s*\([^;]*$`, "func", 2),
	},
	"sh": {
		rule(`^()(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\)\s*\{?`, "func", 2),
		{re: regexp.MustCompile(`^()(?:export\s+)?([A-Z_][A-Z0-9_]*)=`), kind: "var", nameGroup: 2, topLevel: true},
	},
	"proto": {
		rule(`^(\s*)(?:message|enum|service)\s+([A-Za-z_]\w*)`, "type", 2),
		rule(`^(\s*)rpc\s+([A-Za-z_]\w*)`, "method", 2),
	},
}

func init() {
	declRules["ts"] = declRules["js"]
}

func swiftKind(k string) string {
	switch k {
	case "func":
		return "func"
	case "let", "var":
		return "var"
	case "case":
		return "field"
	}
	return "type"
}

type symIndex struct {
	fp      string
	checked time.Time
	byName  map[string][]Decl
	byFile  map[string][]Decl
}

type indexer struct {
	mu  sync.Mutex
	idx map[string]*symIndex
}

func newIndexer() *indexer { return &indexer{idx: map[string]*symIndex{}} }

// Get returns root's index, rebuilding it when files changed. The change
// check runs at most every few seconds so clicks stay fast.
func (ix *indexer) Get(root string) *symIndex {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	cur := ix.idx[root]
	if cur != nil && time.Since(cur.checked) < 3*time.Second {
		return cur
	}
	files := listFiles(root)
	h := fnv.New64a()
	var indexed []string
	for _, f := range files {
		if declRules[langOf(f)] == nil {
			continue
		}
		st, err := os.Stat(filepath.Join(root, f))
		if err != nil || st.Size() > 1<<20 {
			continue
		}
		indexed = append(indexed, f)
		h.Write([]byte(f + strconv.FormatInt(st.Size(), 10) + strconv.FormatInt(st.ModTime().UnixNano(), 10)))
	}
	fp := strconv.FormatUint(h.Sum64(), 16)
	if cur != nil && cur.fp == fp {
		cur.checked = time.Now()
		return cur
	}
	si := &symIndex{fp: fp, checked: time.Now(), byName: map[string][]Decl{}, byFile: map[string][]Decl{}}
	for _, f := range indexed {
		for _, d := range scanDecls(filepath.Join(root, f), f) {
			si.byName[d.N] = append(si.byName[d.N], d)
			si.byFile[f] = append(si.byFile[f], d)
		}
	}
	ix.idx[root] = si
	return si
}

func scanDecls(abs, rel string) []Decl {
	src, err := os.ReadFile(abs)
	if err != nil || bytes.IndexByte(src[:min(len(src), 8000)], 0) >= 0 {
		return nil
	}
	lang := langOf(rel)
	rules := declRules[lang]
	var out []Decl
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	inDoc := false // inside a Python triple-quoted string
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if lang == "py" {
			if c := strings.Count(line, `"""`) + strings.Count(line, `'''`); c%2 == 1 {
				inDoc = !inDoc
				continue
			}
			if inDoc {
				continue
			}
		}
		for _, r := range rules {
			m := r.re.FindStringSubmatchIndex(line)
			if m == nil {
				continue
			}
			indent := m[3] - m[2]
			if r.topLevel && indent > 0 || r.maxIndent > 0 && indent > r.maxIndent {
				continue
			}
			ns, ne := m[2*r.nameGroup], m[2*r.nameGroup+1]
			name := line[ns:ne]
			if notNames[name] {
				continue
			}
			kind := r.kind
			if kind == "" {
				kind = swiftKind(line[m[4]:m[5]])
			}
			if lang == "py" && kind == "func" && indent > 0 {
				kind = "method"
			}
			t := strings.TrimSpace(line)
			if len(t) > 160 {
				t = t[:160] + "…"
			}
			out = append(out, Decl{N: name, K: kind, P: rel, L: n, C: utf16Col(line, ns), Text: t})
			break
		}
	}
	return out
}

func utf16Col(line string, byteOff int) int {
	n := 1
	for _, r := range line[:byteOff] {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Lookup returns declarations named name, nearest to fromFile first.
func (si *symIndex) Lookup(name, fromFile string) []Decl {
	ds := append([]Decl(nil), si.byName[name]...)
	dir := filepath.Dir(fromFile)
	score := func(d Decl) int {
		s := 0
		if d.P == fromFile {
			s -= 100
		} else if filepath.Dir(d.P) == dir {
			s -= 50
		}
		if d.K == "type" || d.K == "func" {
			s -= 5
		}
		return s + strings.Count(d.P, "/")
	}
	sort.SliceStable(ds, func(i, j int) bool { return score(ds[i]) < score(ds[j]) })
	if len(ds) > 30 {
		ds = ds[:30]
	}
	return ds
}
