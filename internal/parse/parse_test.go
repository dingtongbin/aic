package parse

import (
	"strings"
	"testing"
)

// --- helpers ---------------------------------------------------------------

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, lexErrs, parseErrs := Source("t.aic", src)
	if len(lexErrs) != 0 {
		for _, e := range lexErrs {
			t.Errorf("意外的词法错误：\n%s", e.Format())
		}
		t.FailNow()
	}
	if len(parseErrs) != 0 {
		for _, e := range parseErrs {
			t.Errorf("意外的语法错误：\n%s", e.Format())
		}
		t.FailNow()
	}
	return f
}

func parseErrors(t *testing.T, src string) []ParseError {
	t.Helper()
	_, _, errs := Source("t.aic", src)
	return errs
}

func mustFail(t *testing.T, src, wantDesc string) {
	t.Helper()
	errs := parseErrors(t, src)
	if len(errs) == 0 {
		t.Fatalf("期望错误 %q，实际语法通过", wantDesc)
	}
	for _, e := range errs {
		assertThreePart(t, e.Format())
	}
	joined := descList(errs)
	if !strings.Contains(joined, wantDesc) {
		t.Fatalf("期望错误含 %q，实际：%s", wantDesc, joined)
	}
}

func descList(errs []ParseError) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, e.Desc)
	}
	return strings.Join(parts, " | ")
}

func assertThreePart(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("三段式应为 3 行，实际 %d 行：\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], ": error: ") {
		t.Fatalf("第一段格式不符：\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "    ") {
		t.Fatalf("第二段（上下文）须缩进：\n%s", out)
	}
	if !strings.HasPrefix(lines[2], "    fix: ") || strings.TrimSpace(strings.TrimPrefix(lines[2], "    fix: ")) == "" {
		t.Fatalf("第三段（修复）缺失或为空：\n%s", out)
	}
}

// --- 用例组 1：hello 锚点 + 全部 23 关键字都有文法 --------------------------------

func TestHelloAnchorParses(t *testing.T) {
	f := mustParse(t, "import print\n\nfunc main() {\n    print.println(\"hello, aic\")\n}\n")
	if len(f.Decls) != 2 {
		t.Fatalf("期望 2 个顶层声明（import + func），实际 %d", len(f.Decls))
	}
	imp, ok := f.Decls[0].(*ImportDecl)
	if !ok || imp.Name != "print" {
		t.Fatalf("第一个声明应为 import print，实际 %T", f.Decls[0])
	}
	fn, ok := f.Decls[1].(*FuncDecl)
	if !ok || fn.Name != "main" || fn.Body == nil {
		t.Fatalf("第二个声明应为 func main")
	}
	if n := len(fn.Body.Stmts); n != 1 {
		t.Fatalf("main 体内应 1 条语句，实际 %d", n)
	}
}

// TestAllKeywordsHaveGrammar：v2 全表面一段程序走通（语法冻结从第一天起为真）。
func TestAllKeywordsHaveGrammar(t *testing.T) {
	src := `import print

const LIMIT = 10

interface Greeter {
    func greet(name str) -> str
}

enum Color { Red, Green }

class Box[T] {
    var v T
    func get() -> T {
        return this.v
    }
}

func make() -> Box[i32] {
    return Box[i32]{v: 1}
}

func main() -> Err {
    var b = make()
    region {
        var xs i32[] = []
        xs.append(1)
        for i, x in xs {
            if x > 0 {
                print.println(i)
            } else if x < 0 {
                print.println(-x)
            } else {
                print.println("zero")
            }
        }
    }
    for i in 0..LIMIT {
        if i == 3 {
            continue
        }
        if i > 5 {
            break
        }
    }
    var c Color = Color.Green
    var name str = match c {
        Color.Red => "red",
        Color.Green => "green",
    }
    match c {
        Color.Red => { print.println("r") }
        Color.Green => print.println("g")
    }
    scope {
        spawn work()
    }
    defer print.println("bye")
    check workErr()
    return Err(0, "")
}

func work() {
}

func workErr() -> Err {
    return Err(0, "")
}

extern func puts(s str) -> i32

export func aic_version() -> i32 {
    return 1
}
`
	mustParse(t, src)
}

// --- 用例组 2：for 三形态与 `..` --------------------------------------------------

func TestForForms(t *testing.T) {
	// for cond
	mustParse(t, "func f() {\n    for x < 3 {\n    }\n}\n")
	// C-style
	mustParse(t, "func f() {\n    var i i32 = 0\n    for i = 0; i < 3; i += 1 {\n    }\n}\n")
	// range 单名/双名/`_`/a..b
	f := mustParse(t, "func f(xs i32[]) {\n    for x in xs {\n    }\n    for i, x in xs {\n    }\n    for _, x in xs {\n    }\n    for i in 0..10 {\n    }\n}\n")
	fn := f.Decls[0].(*FuncDecl)
	ranges := 0
	for _, s := range fn.Body.Stmts {
		if fs, ok := s.(*ForStmt); ok && fs.IsRange {
			ranges++
			if fs.RangeEnd == nil && ranges == 4 {
				t.Fatalf("第 4 个循环应为 a..b 形")
			}
		}
	}
	if ranges != 4 {
		t.Fatalf("期望 4 个 range 循环，实际 %d", ranges)
	}
	// `..` 紧跟数字：0..n 不得吞进浮点
	mustParse(t, "func f(n i32) {\n    for i in 0..n {\n    }\n}\n")
}

// --- 用例组 3：match 双形态与模式 ------------------------------------------------

func TestMatchExprAndPatterns(t *testing.T) {
	f := mustParse(t, `enum Option { Some(i32), None }
func f(o Option) -> i32 {
    var x = match o {
        Option.Some(v) => v,
        Option.None => 0,
    }
    return x
}
`)
	fn := f.Decls[1].(*FuncDecl)
	init := fn.Body.Stmts[0].(*VarDecl)
	me, ok := init.Init.(*MatchExpr)
	if !ok {
		t.Fatalf("var x = match …：初始化应为 MatchExpr，实际 %T", init.Init)
	}
	if len(me.Arms) != 2 {
		t.Fatalf("match 表达式应 2 分支，实际 %d", len(me.Arms))
	}
	pat := me.Arms[0].Patterns[0]
	if pat.Kind != "payload" || pat.Binding != "v" {
		t.Fatalf("Some(v) 应为 payload 模式绑定 v，实际 %+v", pat)
	}
}

func TestMatchStatementArms(t *testing.T) {
	mustParse(t, `enum E { A, B }
func f(e E) {
    match e {
        E.A => { }
        E.B => g()
    }
}
func g() {
}
`)
	// 块形分支后可选逗号
	mustParse(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        E.A => { },\n        E.B => { }\n    }\n}\n")
}

func TestMatchRejects(t *testing.T) {
	// 表达式形分支不能是块
	mustFail(t, "func f(e i32) {\n    var x = match e { A => { } }\n}\n", "not a `{ ... }` block")
	// 或模式不在表面
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        E.A | E.B => { }\n    }\n}\n", "`|` and or-patterns")
	// 通配符 `_` 不是模式
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        _ => { }\n    }\n}\n", "invalid match pattern")
	// 无字面量模式
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        1 => { }\n    }\n}\n", "invalid match pattern")
}

// --- 用例组 4：lambda ------------------------------------------------------------

func TestLambdaForms(t *testing.T) {
	// 单参表达式体
	f := mustParse(t, "func f() {\n    var g = x => x + 1\n    g(1)\n}\n")
	fn := f.Decls[0].(*FuncDecl)
	init := fn.Body.Stmts[0].(*VarDecl)
	lm, ok := init.Init.(*LambdaExpr)
	if !ok || len(lm.Params) != 1 || lm.Params[0] != "x" || lm.Body == nil {
		t.Fatalf("x => x + 1 解析不符：%T", init.Init)
	}
	// 括号参数 + 块体
	mustParse(t, "func f() {\n    var g = (a, b) => {\n        return a + b\n    }\n    g(1, 2)\n}\n")
	// 函数类型：v2 无 func 类型关键字，写作 (P) -> R（核心设计 §三）
	mustParse(t, "func apply(g (i32, i32) -> i32) -> i32 {\n    return g(1, 2)\n}\n")
	// lambda 作实参
	mustParse(t, "func take(g (i32) -> i32) {\n}\nfunc f() {\n    take(x => x * 2)\n}\n")
}

// --- 用例组 5：复合字面量与条件位歧义 ----------------------------------------------

func TestCompositeLiteral(t *testing.T) {
	f := mustParse(t, "class P {\n    var x i32\n}\nfunc f() {\n    var p = P{x: 1}\n    g(p)\n}\nfunc g(p P) {\n}\n")
	fn := f.Decls[1].(*FuncDecl)
	init := fn.Body.Stmts[0].(*VarDecl)
	cl, ok := init.Init.(*CompositeLit)
	if !ok || len(cl.Fields) != 1 || cl.Fields[0].Name != "x" {
		t.Fatalf("P{x: 1} 应解析为 CompositeLit，实际 %T", init.Init)
	}
	// `if ready {` — 条件后的 `{` 是块体，不是字面量
	mustParse(t, "func f(ready bool) {\n    if ready {\n    }\n}\n")
	// 条件位复合字面量被定向拒绝（语句位 `:` 提示）
	mustFail(t, "class P {\n    var x i32\n}\nfunc f(p P) {\n    if P{x: 1}.x > 0 {\n    }\n}\n", "composite literal")
}

// --- 用例组 6：region / scope / spawn / defer ------------------------------------

func TestRegionScopeSpawnDefer(t *testing.T) {
	mustParse(t, `class C {
    var v i32
}
func work(c C) {
}
func f() {
    region {
        var c = C{v: 1}
        work(c)
    }
    scope {
        spawn work(nil)
    }
    defer work(nil)
    defer {
        work(nil)
    }
}
`)
	// spawn 外于 scope = 词法级编译错（红线 5）
	mustFail(t, "func work() {\n}\nfunc f() {\n    spawn work()\n}\n", "scope")
}

// --- 用例组 7：`_` 弃位与多返回绑定 ------------------------------------------------

func TestBlankBinding(t *testing.T) {
	mustParse(t, "func g() -> (i32, Err) {\n    return 1, Err(0, \"\")\n}\nfunc f() {\n    var a, _ = g()\n    a, _ = g()\n    var _, err = g()\n    h(err)\n}\nfunc h(e Err) {\n}\n")
	// 单个 _ 目标无意义
	mustFail(t, "func g() -> i32 {\n    return 1\n}\nfunc f() {\n    var _ = g()\n}\n", "meaningless")
	// `_` 不可作独立赋值目标
	mustFail(t, "func f() {\n    _ = 1\n}\n", "standalone assignment target")
	// `_` 不是值
	mustFail(t, "func g(x i32) {\n}\nfunc f() {\n    g(_)\n}\n", "cannot be used as a value")
}

// --- 用例组 8：str 视图与下标 ------------------------------------------------------

func TestStrView(t *testing.T) {
	f := mustParse(t, "func f(s str) -> str {\n    return s[1..3]\n}\n")
	fn := f.Decls[0].(*FuncDecl)
	ret := fn.Body.Stmts[0].(*ReturnStmt)
	idx, ok := ret.Results[0].(*Index)
	if !ok || idx.End == nil {
		t.Fatalf("s[1..3] 应解析为带 End 的 Index，实际 %T", ret.Results[0])
	}
	mustParse(t, "func f(xs i32[], i i32) -> i32 {\n    return xs[i]\n}\n")
}

// --- 用例组 9：顶层规则（红线 21 / export） ----------------------------------------

func TestTopLevelRules(t *testing.T) {
	// 顶层 var 禁
	mustFail(t, "var g i32 = 0\n", "top-level var declaration is not allowed")
	// export 只用于函数
	mustFail(t, "export class C {\n}\n", "export is only for exporting functions")
	mustFail(t, "export const K = 1\n", "export is only for exporting functions")
	// export + extern 矛盾
	mustFail(t, "extern func puts(s str) -> i32\nexport extern func puts2(s str) -> i32\n", "export and extern")
	// import 裸名
	mustParse(t, "import print\nimport os\n")
}

// --- 用例组 10：无继承、无 implements ----------------------------------------------

func TestNoInheritanceNoImplements(t *testing.T) {
	mustFail(t, "class A {\n}\nclass B: A {\n}\n", "no inheritance")
	mustFail(t, "interface I {\n    func m()\n}\nclass C implements I {\n    func m() {\n    }\n}\n", "`implements` is retired")
}

// --- 用例组 11：退役词定向报错（附录 A / 防坑清单 1） --------------------------------

func TestRetiredKeywordsGetTargetedErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{"func f() {\n    while x < 3 {\n    }\n}\n", "`while` is retired"},
		{"func f(b bool) {\n    if b {\n    } elseif b {\n    }\n}\n", "`elseif` is retired"},
		{"func f() {\n    handle err {\n    }\n}\n", "`handle` is retired"},
		{"func f() {\n    await g()\n}\nfunc g() {\n}\n", "`await` is retired"},
		{"func f() {\n    del x\n}\n", "`del` is retired"},
		{"func f() {\n    var mut x = 1\n    _ = x\n}\n", "`mut` is retired"},
		{"func f(x i32) {\n    return mut + 1\n}\n", "`mut` is retired"},
	}
	for _, c := range cases {
		mustFail(t, c.src, c.want)
	}
}

// --- 用例组 12：Err 末位（红线 2）与 check 位置 -------------------------------------

func TestErrLastAndCheckPosition(t *testing.T) {
	mustFail(t, "func f() -> (Err, i32) {\n    return Err(0, \"\"), 1\n}\n", "last result position")
	mustParse(t, "func f() -> (i32, Err) {\n    return 0, Err(0, \"\")\n}\n")
	// check 最外层合法
	mustParse(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc f(p str) -> Err {\n    var e = check open(p)\n    return e\n}\n")
	// check 嵌套在实参里 = 错
	mustFail(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc g(x i32) {\n}\nfunc f() -> Err {\n    g(check open(\"\"))\n    return Err(0, \"\")\n}\n", "check may only appear")
	// lambda 内 check 合法（lambda 参数不带类型；类型写在参数位 (P) -> R）
	mustParse(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc f() {\n    var g = (p) => check open(p)\n    g(\"\")\n}\n")
	// const 初始化禁 check
	mustFail(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nconst K = check open(\"\")\n", "const")
}

// --- 用例组 13：类型语法 ------------------------------------------------------------

func TestTypeSyntax(t *testing.T) {
	mustParse(t, `import sync

class C {
    var a i8
    var b u64
    var c f32
    var d str
    var e bool
    var f i32[]
    var g map[str]i32
    var h set[i32]
    var arr [i32;4]
    var m sync.Mutex
    var fn (i32) -> bool
}
`)
	// 元组类型不存在
	mustFail(t, "func f() {\n    var p (i32, str)\n}\n", "there is no tuple type")
	// 泛型实参
	mustParse(t, "func f(b Box[i32]) {\n}\n")
}

// --- 用例组 14：多错一次报（红线 14）------------------------------------------------

func TestMultipleErrorsOneRun(t *testing.T) {
	// 两个错误分别位于独立函数：恢复停在函数边界，第二次编译报出全部。
	errs := parseErrors(t, "func f() {\n    while 1 {\n    }\n}\n\nfunc g() {\n    handle e {\n    }\n}\n")
	if len(errs) < 2 {
		t.Fatalf("一次编译应报出全部错误，实际 %d：%s", len(errs), descList(errs))
	}
	for _, e := range errs {
		assertThreePart(t, e.Format())
	}
}

// --- 用例组 15：运算符与优先级抽查 ---------------------------------------------------

func TestOperatorPrecedence(t *testing.T) {
	f := mustParse(t, "func f(a bool, b bool, c bool) -> bool {\n    return a || b && c\n}\n")
	fn := f.Decls[0].(*FuncDecl)
	ret := fn.Body.Stmts[0].(*ReturnStmt)
	or, ok := ret.Results[0].(*Binary)
	if !ok || or.Op != "||" {
		t.Fatalf("顶层应为 ||，实际 %T", ret.Results[0])
	}
	if r, ok := or.Right.(*Binary); !ok || r.Op != "&&" {
		t.Fatalf("&& 应比 || 紧")
	}
	// 移位与比较
	mustParse(t, "func f(x i32) -> bool {\n    return x << 2 > 8\n}\n")
	// 一元负号
	mustParse(t, "func f() -> i32 {\n    return -9223372036854775808 / 2\n}\n")
}
