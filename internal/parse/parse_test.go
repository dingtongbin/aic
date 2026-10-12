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
	// 或模式不存在
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        E.A | E.B => { }\n    }\n}\n", "or-patterns")
	// 通配符 `_` 不是模式
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        _ => { }\n    }\n}\n", "invalid match pattern")
	// 无字面量模式
	mustFail(t, "enum E { A, B }\nfunc f(e E) {\n    match e {\n        1 => { }\n    }\n}\n", "invalid match pattern")
}

// --- 用例组 4：函数值与闭包 ------------------------------------------------------

// TestFuncLitIsTheOnlyForm：`func(...)` 字面量是唯一拼写。
// `=>` 不是运算符：同一语义不允许两种拼写（P1）。
func TestFuncLitIsTheOnlyForm(t *testing.T) {
	// 判据 = 可解析（AST 形状由更下层的用例看住，这里看形态覆盖面）。
	// 单参 + 紧凑形态
	mustParse(t, "func f() {\n    var g = func(x i32) -> i32 { return x + 1 }\n    g(1)\n}\n")
	// 多参
	mustParse(t, "func f() {\n    var g = func(a i32, b i32) -> i32 { return a + b }\n    g(1, 2)\n}\n")
	// 无参（空形参表 `()`）
	mustParse(t, "func f() {\n    var g = func() { }\n}\n")
	// fallible 闭包：多结果 + Err 末位
	mustParse(t, "func f() {\n    var g = func(p str) -> (str, Err) { return p, nil }\n}\n")
	// 零参函数类型作参数（`() -> (str, Err)`）
	mustParse(t, "func f(g () -> (str, Err)) -> str {\n    return g() catch { \"d\" }\n}\n")
	// 函数类型写作 (P) -> R（无 func 类型关键字）
	mustParse(t, "func apply(g (i32, i32) -> i32) -> i32 {\n    return g(1, 2)\n}\n")
	// 闭包作实参
	mustParse(t, "func take(g (i32) -> i32) {\n}\nfunc f() {\n    take(func(x i32) -> i32 { return x * 2 })\n}\n")
}

// TestArrowLambdaRejected：`x => e` 与 `(a, b) => a + b` 必须被拒绝。
func TestArrowLambdaRejected(t *testing.T) {
	// `=>` 不是任何合法拼写。判据 = **必须被拒绝**（具体文案随解析路径而
	// 定：`x => …` 在标识符后遇到 `=>`；`(a, b) => …` 在括号表达式后遇到 `=>`）。
	// 两条都是"解析无法继续"类的错误。
	mustFail(t, "func f() {\n    var g = x => x + 1\n}\n", "")
	mustFail(t, "func f() {\n    var g = (a, b) => a + b\n}\n", "")
}

// TestCatchBindingOptional：catch 绑定可选（v7 §5.6）。
func TestCatchBindingOptional(t *testing.T) {
	// 不绑定：失败取默认值
	mustParse(t, "func f(g () -> (str, Err)) -> str {\n    return g() catch { \"default\" }\n}\n"+
		"func g() -> (str, Err) { return \"x\", nil }\n")
	// 绑定：就地处理
	mustParse(t, "func f(g () -> (str, Err)) -> str {\n    return g() catch e { return e.msg }\n}\n"+
		"func g() -> (str, Err) { return \"x\", nil }\n")
}

// TestQuestionRejected 与 TestArrowLambdaRejected 只断言**本层**能判定的拒绝：
//   `?` 不在运算符表 → 词法层必须报错；
//   `x => e` 的 `=>` 不是任何合法拼写 → 语法层必须报错。
// `f() or d` 的拒绝发生在检查器（`or` 沦为未声明标识符），
// 由 testdata/err/003_or_fallback.aic 在 golden 层断言——parse 包不能 import
// types（会成环），不在这里重复断言。
func TestQuestionRejected(t *testing.T) {
	_, lexErrs, _ := Source("t.aic", "func f(g () -> (str, Err)) {\n    var v = g()?\n}\n")
	if len(lexErrs) == 0 {
		t.Error("`?` 不在运算符表，词法层应报错")
	}
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
	// spawn 外于 scope = 词法级编译错
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

// --- 用例组 9：顶层规则（export） ----------------------------------------

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

// `implements` 是**硬关键字**，且是接口满足的唯一途径
// （《AIC 语言规范》§3.1 / C10）。结构式满足不存在——大项目里"谁实现了这个
// 接口"必须可 grep。
func TestImplementsIsTheOnlySatisfactionPath(t *testing.T) {
	// implements 子句必须解析干净
	mustParse(t, "interface I {\n    func m() -> i32\n}\nclass C implements I {\n    func m() -> i32 {\n        return 1\n    }\n}\n")
	// struct 也可 implements
	mustParse(t, "interface I {\n    func m() -> i32\n}\nstruct S implements I {\n    var v i32\n    func m() -> i32 {\n        return this.v\n    }\n}\n")
	// 类级 `:` 继承仍然拒绝（无继承）
	mustFail(t, "class A {\n}\nclass B: A {\n}\n", "no inheritance")
	// implements 后必须是接口名
	mustFail(t, "interface I {\n    func m() -> i32\n}\nclass C implements 42 {\n    func m() -> i32 {\n        return 1\n    }\n}\n", "")
}

// --- 用例组 11：普通标识符（《AIC 语言规范》§1.3） --------------------------------

// while/elseif/handle/await/del/mut 不是关键字：能放标识符的位置就能放它们，
// 解析层不为这些拼写定制任何诊断。
//
// 反面（`while x < 3 {}` 这类写法不被接受）由 testdata/err/023_not_a_loop.aic
// 在**全管线**层锁定：解析层把它们读成三条普通语句，拒绝发生在检查器。
func TestNonKeywordSpellingsArePlainIdentifiers(t *testing.T) {
	// 变量 / 函数 / 类 / 字段 / 形参 / 局部量
	mustParse(t, "func f() -> i32 {\n    var while i32 = 1\n    return while\n}\n")
	mustParse(t, "func handle() {\n}\nfunc f() {\n    handle()\n}\n")
	mustParse(t, "class mut {\n    var del i32\n}\n")
	mustParse(t, "func f(elseif i32) -> i32 {\n    var await = elseif\n    return await\n}\n")
}

// --- 用例组 12：Err 末位与 check 位置 -------------------------------------

func TestErrLastAndCheckPosition(t *testing.T) {
	mustFail(t, "func f() -> (Err, i32) {\n    return Err(0, \"\"), 1\n}\n", "last result position")
	mustParse(t, "func f() -> (i32, Err) {\n    return 0, Err(0, \"\")\n}\n")
	// check 最外层合法
	mustParse(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc f(p str) -> Err {\n    var e = check open(p)\n    return e\n}\n")
	// check 嵌套在实参里 = 错
	mustFail(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc g(x i32) {\n}\nfunc f() -> Err {\n    g(check open(\"\"))\n    return Err(0, \"\")\n}\n", "check may only appear")
	// 闭包内 check 合法（唯一形态是 func 字面量，形参带类型）
	mustParse(t, "func open(p str) -> Err {\n    return Err(0, \"\")\n}\nfunc f() {\n    var g = func(p str) -> Err { return check open(p) }\n    g(\"\")\n}\n")
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

// --- 用例组 14：多错一次报------------------------------------------------

func TestMultipleErrorsOneRun(t *testing.T) {
	// 两个错误分别位于独立函数：恢复停在函数边界，第二次编译报出全部。
	errs := parseErrors(t, "func f() {\n    var p (i32, str)\n}\n\nfunc g() {\n    var q (i32, str)\n}\n")
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
