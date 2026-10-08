// Package pkg 是 AIC 包管理器的最小面（核心设计 §十一，黄金执行规划 T1）。
//
// 职责（核心设计 §十 阶段 1「收集」）：
//   - import 闭包解析：入口包 → 逐层展开 import，直到闭包封闭；
//   - 目录即包：一个目录里的全部 .aic 合成一个包（*_test.aic 除外，归 aic test）；
//   - 循环检测：包图有环 = 编译错（红线 21）；
//   - 内容哈希缓存：产物按内容哈希存 ~/.cache/aic（§十一 importc 缓存同源）。
//
// 纪律：本包只做「文件系统 → 包图」的收集，不做类型检查（那是 internal/types），
// 也不发射 C（那是 internal/emit）。整程序收集完成后才允许发射（红线 23）。
package pkg

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aic/internal/parse"
)

// stdNames 是 std 包名（§十四）：import 这些名字不找目录，由检查器按 std 面定型。
// 圆 1–4 的包名也先登记在此，避免被误当作用户目录（用到时检查器会报「包内无此函数」）。
var stdNames = map[string]bool{
	"print": true, "str": true, "os": true, "math": true, "option": true,
	"testing": true, "sync": true, "net": true, "time": true, "sort": true,
	"runtime": true,
	// N12：Ctx 约定是**库级**的，包名先登记（类型面在 T-A，实现随标准库在 T-C）。
	"ctx": true,
	// T-C 的 24 包表（§15.3）先登记名字，避免被误当用户目录。
	"json": true, "regex": true, "tls": true, "http": true,
	"log": true, "crypto": true, "encoding": true, "compress": true, "csv": true,
	"process": true,
}

// IsStd 报告 pkg 是否为 std 包名。
func IsStd(name string) bool { return stdNames[name] }

// File 是一个已解析的源文件。
type File struct {
	// Path 是仓库根相对（斜杠分隔）的路径，用于诊断与 #line 回指（红线 9）。
	Path string
	// Src 是源文本（内容哈希缓存的输入）。
	Src string
	// AST 是解析产物。
	AST *parse.File
}

// Package 是一个包（目录即包）。
type Package struct {
	// Name 是包名 = 目录名（唯一所有者，mangling 只读它）。
	Name string
	// Dir 是包目录的绝对路径。
	Dir string
	// Files 是包内源文件，按文件名排序（收集顺序确定性 → H2）。
	Files []*File
	// Imports 是本包 import 的包名，按声明序去重。
	Imports []string
	// ImportPos 是 import 名 → 该 import 声明的位置（诊断回指那行 import）。
	ImportPos map[string]parse.Pos
}

// Unit 是一个完整程序（入口包 + import 闭包）。
type Unit struct {
	// Entry 是入口包（含 main 的包）。
	Entry *Package
	// Order 是拓扑序的包列表：依赖在前，依赖者在后（发射顺序，红线 23）。
	Order []*Package
}

// ByName 按包名取包（std 包不在表内）。
func (u *Unit) ByName(name string) *Package {
	for _, p := range u.Order {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// Load 从入口文件出发收集整个程序（import 闭包 + 包图 + 循环检测）。
//
// 入口包的判定（两条规则，覆盖"单文件程序"与"多文件包"两种常态）：
//   - 入口文件名是 main.aic → 该文件所在目录即包（main.aic 是包根标记，
//     目录内全部 .aic 合成一个包）；
//   - 否则 → 只有入口文件构成包（单文件程序；语料 ok/err/trap 全是这种）。
//
// 被 import 的包一律是**整个目录**（目录即包，§四）。这条区分是必要的：
// testdata/ok/ 下 87 个互不相干的单文件程序共处一目录，若入口也按目录成包，
// 它们会被并成一个包（重复 import / 多个 main），语料立刻全灭。
func Load(entry string) (*Unit, error) {
	abs, err := filepath.Abs(entry)
	if err != nil {
		return nil, fmt.Errorf("aic: cannot locate %s: %v", entry, err)
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return nil, fmt.Errorf("aic: the entry must be an .aic file: %s", filepath.ToSlash(entry))
	}
	root := filepath.Dir(abs)
	entryPos := parse.Pos{File: relPath(abs), Line: 1, Col: 1}
	var entryPkg *Package
	if filepath.Base(abs) == "main.aic" {
		entryPkg, err = loadPackage(root, entryPos)
	} else {
		entryPkg, err = loadSingleFile(abs)
	}
	if err != nil {
		return nil, err
	}
	l := &loader{
		entryPos: entryPos,
		byDir:    map[string]*Package{root: entryPkg},
		state:    map[string]int{}, // 0 未访问 / 1 在栈上 / 2 已完成
		stack:    nil,
	}
	if err := l.walk(entryPkg); err != nil {
		return nil, err
	}
	return &Unit{Entry: entryPkg, Order: l.order}, nil
}

// loadSingleFile 把单个文件装成一个包（单文件程序；包名 = 目录名）。
func loadSingleFile(abs string) (*Package, error) {
	dir := filepath.Dir(abs)
	f, err := parseFileAt(dir, filepath.Base(abs), "")
	if err != nil {
		return nil, err
	}
	p := &Package{
		Name:      filepath.Base(dir),
		Dir:       dir,
		Files:     []*File{f},
		ImportPos: map[string]parse.Pos{},
	}
	for _, d := range f.AST.Decls {
		im, ok := d.(*parse.ImportDecl)
		if !ok {
			continue
		}
		if _, dup := p.ImportPos[im.Name]; !dup {
			p.ImportPos[im.Name] = im.Pos
			p.Imports = append(p.Imports, im.Name)
		}
	}
	return p, nil
}

// loader 是收集过程的全部状态（无包级可变状态）。
type loader struct {
	entryPos parse.Pos // 入口文件位置：无 import 位的诊断用它
	byDir    map[string]*Package
	state    map[string]int
	stack    []string // 当前 DFS 栈（循环检测的路径证据）
	order    []*Package
}

// walk 深度优先展开一个包的 import 闭包。
func (l *loader) walk(p *Package) error {
	l.state[p.Dir] = 1
	l.stack = append(l.stack, p.Name)
	for _, name := range p.Imports {
		if IsStd(name) {
			continue
		}
		at := p.ImportPos[name]
		if at.File == "" {
			at = l.entryPos
		}
		dir, err := resolveImport(p.Dir, name)
		if err != nil {
			return diag(at, "package not found: "+name, err.Error(),
				"a directory is a package: a bare import name maps to a same-named directory (core design §4)")
		}
		if dep, seen := l.byDir[dir]; seen {
			if l.state[dir] == 1 {
				return l.cycleError(dep.Name, at)
			}
			if l.state[dir] == 2 {
				continue
			}
		}
		dep, err := loadPackage(dir, at)
		if err != nil {
			return err
		}
		if dep.Name != name {
			return diag(at, "import name does not match the directory name",
				"import "+name+" resolved to directory "+filepath.ToSlash(dir)+" (directory name "+dep.Name+")",
				"a directory is a package: the directory name must equal the import name (core design §4)")
		}
		l.byDir[dir] = dep
		if err := l.walk(dep); err != nil {
			return err
		}
	}
	l.stack = l.stack[:len(l.stack)-1]
	l.state[p.Dir] = 2
	l.order = append(l.order, p) // 后序 = 拓扑序（依赖在前）
	return nil
}

// cycleError 构造循环 import 的诊断（红线 21：包循环 import = 编译错）。
// at = 闭合环的那行 import（诊断回指它）。
func (l *loader) cycleError(back string, at parse.Pos) error {
	i := 0
	for j, n := range l.stack {
		if n == back {
			i = j
			break
		}
	}
	path := append(append([]string{}, l.stack[i:]...), back)
	return diag(at, "cyclic package import", strings.Join(path, " → "),
		"extract the shared part into a third package (core design §4: a cyclic import is a compile error, red line 21)")
}

// resolveImport 把 import 名解析为目录：先从导入者目录向下找 <dir>/<name>，
// 再逐级向上找 <ancestor>/<name>（目录即包，§四）。
func resolveImport(fromDir, name string) (string, error) {
	dir := fromDir
	for i := 0; i < 12; i++ {
		cand := filepath.Join(dir, name)
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("searching upward from %s found no %s/ directory", filepath.ToSlash(fromDir), name)
}

// loadPackage 读入一个目录的全部 .aic（排序；*_test.aic 归 aic test，不入包）。
// at = 引入该目录的那行 import（诊断位置；入口包用 entryPos）。
func loadPackage(dir string, at parse.Pos) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, diag(at, "cannot read the package directory", filepath.ToSlash(dir)+": "+err.Error(),
			"confirm the directory exists and is readable; a directory is a package (core design §4)")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".aic") || strings.HasSuffix(e.Name(), "_test.aic") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, diag(at, "the package directory holds no .aic source file",
			"directory "+filepath.ToSlash(dir)+" is empty or holds no .aic file",
			"a directory is a package: at least one .aic file (*_test.aic belongs to aic test, not to the package)")
	}
	sort.Strings(names) // 收集顺序确定性（H2：两次构建逐字节一致）
	p := &Package{Name: filepath.Base(dir), Dir: dir, ImportPos: map[string]parse.Pos{}}
	seen := map[string]bool{}
	for _, n := range names {
		f, err := parseFileAt(dir, n, "")
		if err != nil {
			return nil, err
		}
		p.Files = append(p.Files, f)
		for _, d := range f.AST.Decls {
			im, ok := d.(*parse.ImportDecl)
			if !ok {
				continue
			}
			if _, dup := p.ImportPos[im.Name]; !dup {
				p.ImportPos[im.Name] = im.Pos
			}
			if !seen[im.Name] {
				seen[im.Name] = true
				p.Imports = append(p.Imports, im.Name)
			}
		}
	}
	return p, nil
}

// parseFileAt 解析目录下的一个源文件（相对路径按仓库根渲染）。
func parseFileAt(dir, name, relOverride string) (*File, error) {
	full := filepath.Join(dir, name)
	src, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("aic: cannot read %s: %v", filepath.ToSlash(full), err)
	}
	rel := relOverride
	if rel == "" {
		rel = relPath(full)
	}
	ast, lexErrs, parseErrs := parse.Source(rel, string(src))
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		return nil, &SyntaxError{Path: rel, Lex: lexErrs, Parse: parseErrs}
	}
	return &File{Path: rel, Src: string(src), AST: ast}, nil
}

// importsOf 按声明序取一个文件的 import 名（去重）。
func importsOf(f *parse.File) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range f.Decls {
		im, ok := d.(*parse.ImportDecl)
		if !ok || seen[im.Name] {
			continue
		}
		seen[im.Name] = true
		out = append(out, im.Name)
	}
	return out
}

// relPath 取斜杠分隔的仓库根相对路径（不能取到时回退绝对路径）。
func relPath(abs string) string {
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return filepath.ToSlash(abs)
}
