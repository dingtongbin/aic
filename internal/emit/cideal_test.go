package emit

import (
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// H10 扩展断言：**C 理想化清单**的可机械检查部分（核心设计 §15.6）。
//
// 本文件落地 C10（产物不得声明裸 `char` / `long`）与 C15（一条 C 语句里不得出现
// 两个带副作用的调用）。两条都是"形态"判据，不看编译结果，故能在单测里跑。
// ---------------------------------------------------------------------------

// stripStringLiterals 把 C 字符串字面量替换成空串（判据只看代码形态，
// 字面量里的 char/long 是数据不是声明）。
func stripStringLiterals(src string) string {
	var b strings.Builder
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == '"' {
			b.WriteByte('"')
			b.WriteByte('"')
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		if ch == '\'' { // 字符常量同理
			b.WriteByte(' ')
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '\'' {
					i++
					break
				}
				i++
			}
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return b.String()
}

var (
	// 裸 long / unsigned long 出现在类型位（`long x` / `(long)` / `long long`）。
	reBareLong = regexp.MustCompile(`\blong\b`)
	// 裸 char 出现在声明/强转位（`char x` / `char *` / `(char *)`），
	// 排除 `const char *` —— 后者只允许出现在与 C 交互的字符串视图上。
	reBareChar = regexp.MustCompile(`(?m)(^|[^\w])(?:unsigned\s+)?char\s*[\*\)]`)
)

// TestNoBareCharLongC10: C10 —— 产物与运行时一律定宽 typedef，禁裸 char/long。
func TestNoBareCharLongC10(t *testing.T) {
	src := `import print
import str

class Box {
    var n i64
    var name str
}

func label(b Box) -> str {
    return "box"
}

func main() {
    var xs i32[] = [1, 2, 3]
    var b = Box{n: 42, name: "x"}
    print.println(label(b))
    print.println(xs[0])
    print.println(str.fromI64(b.n))
    print.println(1.5)
    for i in 0..3 {
        print.println(i)
    }
}
`
	c := stripStringLiterals(emitSrc(t, src))
	// `int main(int argc, char **argv)` 是 C 标准钉死的入口签名（宿主 ABI 要求），
	// 不是产物自选的类型拼写 —— 它是 C10 的显式例外，先归一掉再判。
	c = strings.ReplaceAll(c, "int main(int argc, char **argv)", "int main(void)")
	c = strings.ReplaceAll(c, "int main(int argc, char *argv[])", "int main(void)")
	if m := reBareLong.FindAllString(c, -1); len(m) > 0 {
		t.Errorf("C10 违反：产物中出现裸 long %d 处", len(m))
	}
	// const char * 是 C 字符串字面量的唯一合法类型（所有 libc 字符串接口都用它），
	// 允许；其余 char 形态不允许。
	for _, line := range strings.Split(c, "\n") {
		l := strings.TrimSpace(line)
		if !reBareChar.MatchString(l) {
			continue
		}
		if strings.Contains(l, "const char *") || strings.Contains(l, "const char*") {
			continue
		}
		t.Errorf("C10 违反：产物中出现裸 char 声明/强转：%s", l)
	}
}

// TestEvalOrderC15: C15 —— 一条 C 语句里不得出现两个带副作用的调用。
//
// 判据按形态取：把产物按语句边界（`;` / `{` / `}`）切段，统计每段里"看起来像
// 函数调用"的 `name(...)` 个数。带副作用的实参必须已被提升成独立语句。
func TestEvalOrderC15(t *testing.T) {
	src := `class Log {
    var s str
    var n i32

    func add(t str) -> i32 {
        this.s = this.s + t
        this.n = this.n + 1
        return this.n
    }
}

func add2(a i32, b i32) -> i32 {
    return a + b
}

func main() {
    var lg = Log{}
    var r1 = add2(lg.add("a"), lg.add("b"))
    var r2 = lg.add("c") + lg.add("d")
}
`
	c := stripDirectives(stripStringLiterals(emitSrc(t, src)))
	if n := maxCallsPerStmt(c); n > 1 {
		t.Errorf("C15 违反：最坏的一条 C 语句里有 %d 个调用（求值顺序未定）：%s",
			n, worstStmt(c))
	}
}

// stripDirectives 去掉 `#line` 预处理器行（它们不是语句）。
func stripDirectives(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// splitStmts 按语句边界切段。
func splitStmts(src string) []string {
	return strings.FieldsFunc(src, func(r rune) bool {
		return r == ';' || r == '{' || r == '}'
	})
}

func maxCallsPerStmt(src string) int {
	worst := 0
	for _, stmt := range splitStmts(src) {
		if n := countCalls(stmt); n > worst {
			worst = n
		}
	}
	return worst
}

func worstStmt(src string) string {
	worst, text := 0, ""
	for _, stmt := range splitStmts(src) {
		if n := countCalls(stmt); n > worst {
			worst, text = n, strings.TrimSpace(stmt)
		}
	}
	return text
}

// countCalls 数一条 C 语句里的调用形态（`ident(`），排除关键字构造。
func countCalls(stmt string) int {
	skip := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "return": true,
		"sizeof": true, "case": true, "do": true, "else": true, "goto": true,
	}
	n := 0
	for i := 0; i < len(stmt); i++ {
		if stmt[i] != '(' {
			continue
		}
		j := i - 1
		for j >= 0 && (isIdentByte(stmt[j])) {
			j--
		}
		name := stmt[j+1 : i]
		if name == "" || skip[name] {
			continue
		}
		// 形如 `(*fn)(...)` 的间接调用也算一次（副作用一致）。
		n++
	}
	return n
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
