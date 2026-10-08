package emit

import (
	"fmt"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 表达式发射 (核心设计 §一/§二/§三/§四/§六)。
//
// want = 上下文期望类型 (字面量定宽、nil 零值形态、容器字面量元素类型都由它定)。
// 每个表达式返回一段 C 表达式文本; 需要时用 C 语句表达式承载 check 的传播。
// ---------------------------------------------------------------------------

// expr 发射一个表达式。
func (c *Ctx) expr(e parse.Expr, want types.Type) (string, error) {
	if e == nil {
		return "0", nil
	}
	// defer trampoline 回放：实参节点整棵替换成注册点捕获的局部量。
	if c.subst != nil {
		if rep, ok := c.subst[e]; ok {
			return rep, nil
		}
	}
	// interface 装箱（§四）：期望类型是接口、值类型是具体类 → 生成 {data, vt}。
	// 这一步必须在最外层做（内层各形态只管自己的裸值），否则实参/赋值/字段
	// 初始化三条路都要各写一遍装箱。
	if ifc, isIfc := types.IsInterface(want); isIfc {
		if c.classValueOf(e) != nil {
			return c.interfaceValue(e, ifc)
		}
	}
	switch v := e.(type) {
	case *parse.IntLit:
		return c.intLit(v, want), nil
	case *parse.FloatLit:
		return c.floatLit(v, want), nil
	case *parse.StrLit:
		return c.strLit(unquoteAIC(v.Text)), nil
	case *parse.BoolLit:
		if v.Value {
			return "true", nil
		}
		return "false", nil
	case *parse.NilLit:
		if want == nil {
			return "NULL", nil
		}
		return c.zeroValue(want), nil
	case *parse.ThisExpr:
		return "this__", nil
	case *parse.Ident:
		// std option 的裸 None：优先按**期望类型**定型（doubled(None) 这类实参位
		// 只有形参类型能给 T），再回退到检查器记下的类型。
		if v.Name == "None" && c.Info != nil && c.Info.Imported["option"] {
			inst, ok := types.IsOptionInstance(want)
			if !ok {
				inst, ok = types.IsOptionInstance(c.ti(v))
			}
			if ok {
				ty := c.cTypeName(inst)
				_, none := optionTags(ty)
				return fmt.Sprintf("((%s){ .tag = %s })", ty, none), nil
			}
		}
		return c.ident(v), nil
	case *parse.Unary:
		return c.unary(v, want)
	case *parse.Binary:
		return c.binary(v)
	case *parse.Call:
		// N4 编译期内省：折成常量（typeName/isValueType）或 C 的 sizeof（sizeOf）。
		if cv, ok := c.Info.ComptimeOf(v); ok {
			return c.comptimeExpr(cv, v.Pos.Line)
		}
		return c.call(v, want)
	case *parse.Field:
		return c.field(v, want)
	case *parse.Index:
		return c.index(v, want)
	case *parse.ArrayLit:
		return c.arrayLit(v, want)
	case *parse.CompositeLit:
		return c.compositeLit(v)
	case *parse.CheckExpr:
		return c.checkValueExpr(v, want)
	case *parse.MatchExpr:
		return c.matchExpr(v, want)
	case *parse.LambdaExpr:
		return c.lambdaExpr(v, want)
	case *parse.Assign, *parse.MultiAssign:
		return "", fmt.Errorf("emit: assignment is a statement, not an expression (core design §3, line %d)", nodeLine(e))
	}
	return "", fmt.Errorf("emit: the code generator does not support this expression yet (%s, line %d)", exprKind(e), nodeLine(e))
}

// ident 发射名字: 局部/参数 = 裸名; 包级常量 = 字面量; 类型名 = typedef 名。
func (c *Ctx) ident(v *parse.Ident) string {
	if c.Info == nil {
		return v.Name
	}
	// std option 的裸 None：目标类型由检查器从上下文定型（T 不定 = 检查期已拒绝）。
	if v.Name == "None" && c.Info.Imported["option"] {
		if inst, ok := types.IsOptionInstance(c.ti(v)); ok {
			ty := c.cTypeName(inst)
			_, none := optionTags(ty)
			return fmt.Sprintf("((%s){ .tag = %s })", ty, none)
		}
	}
	if c.Info.IsLocal(c.curFuncName, v.Name) {
		return v.Name
	}
	if sym, ok := c.Info.Consts[v.Name]; ok {
		return c.constLiteral(sym)
	}
	return v.Name
}

// constLiteral 把折叠后的常量发射成 C 字面量。
func (c *Ctx) constLiteral(sym *types.Symbol) string {
	switch sym.Const.Kind {
	case types.ConstInt:
		return fmt.Sprintf("%d", sym.Const.Int)
	case types.ConstUint:
		return fmt.Sprintf("%dULL", sym.Const.Uint)
	case types.ConstFloat:
		return fmt.Sprintf("%g", sym.Const.Float)
	case types.ConstStr:
		return c.strLit(sym.Const.Str)
	case types.ConstBool:
		if sym.Const.Bool {
			return "true"
		}
		return "false"
	}
	return "0"
}

// intLit 发射整数字面量: 按期望类型定宽 (无隐式转换, 字面量超界已在检查期拒绝)。
func (c *Ctx) intLit(v *parse.IntLit, want types.Type) string {
	lw := strings.ToLower(v.Text)
	text := strings.ReplaceAll(lw, "_", "")
	// 期望类型决定后缀: 避免 C 的 int 提升把字面量变成错的宽度。
	if want != nil {
		if b, ok := want.(*types.Basic); ok {
			switch b.Name {
			case "i64":
				return text + "LL"
			case "u64":
				return text + "ULL"
			case "usize":
				return text + "ULL"
			case "f64":
				return text + ".0"
			}
		}
	}
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0b") || strings.HasPrefix(text, "0o") {
		// 二进制字面量 C 不支持 (C11 无 0b, GNU 扩展不保证 tcc): 转十进制。
		if strings.HasPrefix(text, "0b") {
			var n uint64
			for _, ch := range text[2:] {
				n = n*2 + uint64(ch-'0')
			}
			return fmt.Sprintf("%dULL", n)
		}
		if strings.HasPrefix(text, "0o") {
			var n uint64
			for _, ch := range text[2:] {
				n = n*8 + uint64(ch-'0')
			}
			return fmt.Sprintf("%dULL", n)
		}
		return text
	}
	var n int64
	neg := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '-' && i == 0 {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			return text
		}
		n = n*10 + int64(ch-'0')
	}
	if neg {
		n = -n
	}
	// 超出 i32 域时明确加后缀, 让 C 不把它截成 int。
	if n > 2147483647 || n < -2147483648 {
		return fmt.Sprintf("%dLL", n)
	}
	return fmt.Sprintf("%d", n)
}

// floatLit 发射浮点字面量 (f32 需要 f 后缀, 禁 f32/f64 混算由检查器保证)。
func (c *Ctx) floatLit(v *parse.FloatLit, want types.Type) string {
	text := strings.ReplaceAll(v.Text, "_", "")
	if want != nil {
		if b, ok := want.(*types.Basic); ok && b.Name == "f32" {
			return text + "f"
		}
	}
	return text
}

// unary 发射一元运算 (! ~ -)。
func (c *Ctx) unary(v *parse.Unary, want types.Type) (string, error) {
	x, err := c.expr(v.X, want)
	if err != nil {
		return "", err
	}
	if v.Op == "-" {
		return "(-" + x + ")", nil
	}
	return "(" + v.Op + x + ")", nil
}

// binary 发射二元运算; str 比较/连接与 Err 的 nil 比较走运行时函数 (§二.3/§三/§六)。
//
// C15：C **不规定**二元操作数的求值顺序，故带副作用的操作数一律先绑临时量；
// 例外是 `&&` / `||` —— 它们必须短路，提升操作数会改变语义。
func (c *Ctx) binary(v *parse.Binary) (string, error) {
	lt := c.ti(v.Left)
	rt := c.ti(v.Right)
	hoist := v.Op != "&&" && v.Op != "||"
	// Err == nil / != nil = code == 0 的内建比较 (§三)
	if v.Op == "==" || v.Op == "!=" {
		if isNilCompare(v.Left, v.Right) {
			side := v.Left
			if isNilLiteral(v.Left) {
				side = v.Right
			}
			e, err := c.orderedOperand(side, nil, hoist)
			if err != nil {
				return "", err
			}
			// Err 的零值 = nil，比较是 code == 0 的内建比较 (§三)。这里必须
			// 认 Err 变量自身（Err 变量常无类型条目），否则会发射出
			// `aic_Err != NULL` —— C 层的类型错误。
			if types.IsErrType(c.ti(side)) || types.IsErrType(lt) || types.IsErrType(rt) {
				return fmt.Sprintf("((%s).code %s 0)", e, v.Op), nil
			}
			return fmt.Sprintf("((%s) %s NULL)", e, v.Op), nil
		}
		// str == str = 字节字典序 (§三)
		if types.IsStrType(lt) && types.IsStrType(rt) {
			l, err := c.orderedOperand(v.Left, lt, hoist)
			if err != nil {
				return "", err
			}
			r, err := c.orderedOperand(v.Right, rt, hoist)
			if err != nil {
				return "", err
			}
			neg := ""
			if v.Op == "!=" {
				neg = "!"
			}
			return fmt.Sprintf("(%saic_str_eq(%s, %s))", neg, l, r), nil
		}
	}
	l, err := c.orderedOperand(v.Left, lt, hoist)
	if err != nil {
		return "", err
	}
	// str + str = 连接 (§一 优先级表; 视图/字节入任务区域)
	if v.Op == "+" && types.IsStrType(lt) && types.IsStrType(rt) {
		r, err := c.orderedOperand(v.Right, rt, hoist)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_str_concat(%s, %s, %d)", l, r, nodeLine(v)), nil
	}
	op := v.Op
	// AIC 与 C 的运算符同形; && / || 都短路 (§一 优先级表)。
	theOp := op
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		if types.IsStrType(lt) && types.IsStrType(rt) {
			r, err := c.orderedOperand(v.Right, rt, hoist)
			if err != nil {
				return "", err
			}
			cmp := fmt.Sprintf("aic_str_cmp(%s, %s)", l, r)
			return fmt.Sprintf("(%s %s 0)", cmp, op), nil
		}
		// @packed 值：C 的 struct 不能直接比较（`a == b` 是非法 C）⇒ 走逐字段比较助手。
		if cl, ok := packedOperand(lt, rt); ok {
			r, err := c.orderedOperand(v.Right, rt, hoist)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(%s %s 0)", c.packedCompareCall(cl, l, r), op), nil
		}
	}
	r, err := c.orderedOperand(v.Right, rt, hoist)
	if err != nil {
		return "", err
	}
	// 整数除法/取余：除数为 0 = trap（§一）。浮点除零按 IEEE，不 trap（§九）。
	// 除数先绑临时量再查（避免带副作用的右操作数被求值两次）。
	if (op == "/" || op == "%") && isIntLike(lt) && isIntLike(rt) {
		dt := c.tmp("div")
		c.line("%s %s = %s;", c.cTypeName(rt), dt, r)
		c.line("AIC_DIV_CHECK(%s, %s, %d);", dt, cstr(c.Path), nodeLine(v))
		r = dt
	}
	// 移位：计数 ≥ 左操作数位宽（或有符号负计数）= trap（§一「移位语义」）。
	// 计数先绑临时量再查（同除法的理由：右操作数可能带副作用）。
	if (op == "<<" || op == ">>") && isIntLike(lt) && isIntLike(rt) {
		st := c.tmp("sh")
		c.line("%s %s = %s;", c.cTypeName(rt), st, r)
		c.line("AIC_SHIFT_CHECK(%s, %du, %s, %d);", st, bitWidthOf(lt), cstr(c.Path), nodeLine(v))
		r = st
	}
	return fmt.Sprintf("(%s %s %s)", l, theOp, r), nil
}

// packedOperand 报告两个操作数是否是**同一个** @packed 类（比较要走逐字段助手）。
func packedOperand(lt, rt types.Type) (*types.Class, bool) {
	l, ok1 := lt.(*types.Class)
	r, ok2 := rt.(*types.Class)
	if !ok1 || !ok2 || !l.Packed || !r.Packed || l.Name != r.Name || l.Pkg != r.Pkg {
		return nil, false
	}
	return l, true
}

// bitWidthOf 取整型的位宽（移位范围检查用；usize = 64，§16 N13）。
func bitWidthOf(t types.Type) int {
	b, ok := t.(*types.Basic)
	if !ok {
		return 64
	}
	switch b.Name {
	case "i8", "u8":
		return 8
	case "i16", "u16":
		return 16
	case "i32", "u32":
		return 32
	default:
		return 64
	}
}

// exprKind 给诊断用的表达式名：只出现**语言级**名字（`or` / `catch` / `!` / 插值…），
// 绝不把 Go 类型名（*parse.OrExpr）泄漏给使用者。
func exprKind(e parse.Expr) string {
	switch e.(type) {
	case *parse.OrExpr:
		return "`or` (E2 fallback)"
	case *parse.CatchExpr:
		return "`catch` (E3 in-place handling)"
	case *parse.AssertExpr:
		return "postfix `!` (E4 assertion)"
	case *parse.InterpLit:
		return "string interpolation"
	case *parse.LambdaExpr:
		return "closure literal"
	case *parse.CheckExpr:
		return "`check` / `?`"
	case *parse.MatchExpr:
		return "match expression"
	case *parse.CompositeLit:
		return "composite literal"
	case *parse.ArrayLit:
		return "list literal"
	}
	return "this expression"
}

// isIntLike 报告类型是否为整型（含 usize；不含 bool）。
func isIntLike(t types.Type) bool {
	b, ok := t.(*types.Basic)
	if !ok {
		return false
	}
	return isIntName(b.Name)
}

// isNilCompare 报告一侧是 nil 字面量。
func isNilCompare(l, r parse.Expr) bool {
	return isNilLiteral(l) || isNilLiteral(r)
}

// field 发射成员访问: 类字段 = ->, Err 字段 = ., 枚举变体 = 常量, 包函数 = 调用点。
func (c *Ctx) field(v *parse.Field, want types.Type) (string, error) {
	// 枚举变体: Color.Red
	if id, ok := v.X.(*parse.Ident); ok && c.Info != nil {
		if en, isEnum := c.Info.Enums[id.Name]; isEnum {
			if idx, has := en.VariantIndex(v.Name); has {
				name := c.namedTypeName(types.EnumPkg(en), en.Name)
				if en.Variants[idx].Payload != nil {
					return "", fmt.Errorf("emit: a variant with a payload cannot be referenced directly (%s.%s, line %d)",
						id.Name, v.Name, v.Pos.Line)
				}
				return c.variantNoData(en, name, v.Name), nil
			}
		}
	}
	// 跨包常量 util.Limit：常量是编译期值，直接发它的字面量（不是符号引用）。
	if id, ok := v.X.(*parse.Ident); ok {
		if dep := c.depInfo(id.Name); dep != nil {
			if sym, has := dep.Consts[v.Name]; has && sym != nil {
				return c.constLiteral(sym), nil
			}
			return "", fmt.Errorf("emit: member %s of package %s is not a value (line %d)",
				id.Name, v.Name, v.Pos.Line)
		}
	}
	// 跨包枚举变体：util.Color.Red（X = util.Color，Y = Red）
	if inner, ok := v.X.(*parse.Field); ok {
		if id, isID := inner.X.(*parse.Ident); isID && c.depInfo(id.Name) != nil {
			if dep := c.depInfo(id.Name); dep != nil {
				if en, has := dep.Enums[inner.Name]; has {
					if idx, isVar := en.VariantIndex(v.Name); isVar {
						name := c.namedTypeName(types.EnumPkg(en), en.Name)
						if en.Variants[idx].Payload != nil {
							return "", fmt.Errorf("emit: a variant with a payload cannot be referenced directly (%s.%s.%s, line %d)",
								id.Name, inner.Name, v.Name, v.Pos.Line)
						}
						return c.variantNoData(en, name, v.Name), nil
					}
				}
			}
		}
	}
	// Err 的 code/msg = 按值字段访问；cause = 指针字段，读它要过判空闸（§九）。
	// **接收者形态不限 Ident**（`e.cause.cause.msg` 这类链式访问的接收者是 Field）。
	if t := c.ti(v.X); t != nil && types.IsErrType(t) {
		e, err := c.expr(v.X, t)
		if err != nil {
			return "", err
		}
		if v.Name == "cause" {
			c.need("aic_deref_or_trap")
			pos := parse.ExprPos(v)
			return fmt.Sprintf("(*(aic_Err *)aic_deref_or_trap((void *)(%s).cause, %s, %d))",
				e, cstr(c.Path), pos.Line), nil
		}
		return fmt.Sprintf("(%s).%s", e, v.Name), nil
	}
	recv, err := c.expr(v.X, nil)
	if err != nil {
		return "", err
	}
	// 引用对象用 ->；值类型（@packed / Err / enum）用 .（§四 @packed = C struct 语义）
	if isValueAccess(c.ti(v.X)) {
		return fmt.Sprintf("(%s).%s", recv, v.Name), nil
	}
	// **按语义类型补一次转换**：容器的元素槽是 void*（`aic_list_get_Box` 等），
	// 直接接 `->字段` 会发出 `void *` 解引用（C 层报 "request for member … in
	// something not a structure"）。语义类型是引用类/类实例时，把接收者显式转成
	// 该类的 C 类型 —— 已经是该类型时多一次转换无害，槽位是 void* 时正好补上。
	recv = c.castRefReceiver(v.X, recv)
	return fmt.Sprintf("%s->%s", paren("", recv), v.Name), nil
}

// castRefReceiver 在接收者的 C 表达式可能是 `void *`（容器元素槽）时，
// 按语义类型补一次指针转换；不是引用类型则原样返回。
func (c *Ctx) castRefReceiver(x parse.Expr, recv string) string {
	t := c.ti(x)
	if t == nil {
		return recv
	}
	ct := ""
	if cl, ok := types.IsClass(t); ok && !cl.Packed {
		// 泛型**基类**没有 C 符号（发出去的只有实例名 `aic_pkg_Box_i32`）：方法体里的
		// `this` 在检查期就是基类 `Box`，这里绝不能按基类名补转换（否则发出未声明的
		// `aic_pkg_Box *`）。实例形态在下面那条分支里按实例名处理。
		if len(cl.TypeParams) == 0 {
			ct = c.namedTypeName(cl.Pkg, cl.Name) + " *"
		}
	} else if base, _, ok := types.InstanceParts(t); ok {
		if cl, isCl := types.IsClass(base); isCl && !cl.Packed {
			ct = c.cTypeName(t) // 实例名（已带 ` *`）
		}
	}
	if ct == "" {
		return recv
	}
	return fmt.Sprintf("((%s)%s)", ct, recv)
}

// isValueAccess 报告成员访问该用 `.`（值类型）还是 `->`（引用对象）。
func isValueAccess(t types.Type) bool {
	if t == nil {
		return false
	}
	if types.IsErrType(t) {
		return true
	}
	if _, ok := types.IsClass(t); ok {
		return types.IsValueType(t) // @packed
	}
	if _, ok := types.IsEnum(t); ok {
		return true
	}
	if _, _, ok := types.InstanceParts(t); ok {
		// 泛型实例：枚举实例按值承载 {tag, union}；**类实例**要看基类是否 @packed
		// （普通类实例仍是指针 → 用 `->`）。
		if base, _, ok2 := types.InstanceParts(t); ok2 {
			if cl, isCl := types.IsClass(base); isCl {
				return cl.Packed
			}
		}
		return true
	}
	return false
}

// index 发射下标读: 列表 get / 定长数组 [] / str 字节 / str 视图 (§二.2/§二.3)。
func (c *Ctx) index(v *parse.Index, want types.Type) (string, error) {
	xt := c.ti(v.X)
	// C15：接收者与下标都是求值点，两者在 C 语句里无先后约束 → 有副作用者先绑临时量。
	recv, err := c.orderedOperand(v.X, xt, true)
	if err != nil {
		return "", err
	}
	idx, err := c.orderedOperand(v.Index, types.TUsize, true)
	if err != nil {
		return "", err
	}
	// str 视图 s[i..j] = 视图 (字节恒任务区域, 零拷贝)
	if v.End != nil {
		end, err := c.orderedOperand(v.End, types.TUsize, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_str_sub(%s, %s, %s, %s, %d)",
			recv, idx, end, cstr(c.Path), v.Pos.Line), nil
	}
	switch {
	case types.IsSlice(xt):
		suf, ok := c.containerSuffix(types.SliceElem(xt))
		if !ok {
			return "", fmt.Errorf("emit: list element type %s is not instantiated (line %d)",
				types.SliceElem(xt).String(), v.Pos.Line)
		}
		if id, isID := v.X.(*parse.Ident); isID {
			c.noteIndexAccess(id.Name)
			if cache, ok := c.cached[id.Name]; ok {
				// F5：缓存形态（热路径只比较 + 直接读；越界走慢路径并刷新缓存）
				return fmt.Sprintf("AIC_LIST_GET_CACHED(%s, %s, %s, %s, %d)",
					suf, cache, idx, cstr(c.Path), v.Pos.Line), nil
			}
		}
		if c.provedInBounds(v.X, v.Index) {
			// §10.4 第 12 条②：已证在界内（F2 常量长度 / F3 len 上界）→ unchecked
			c.unchecked++
			return fmt.Sprintf("aic_list_get_unchecked_%s(%s, %s)", suf, recv, idx), nil
		}
		return fmt.Sprintf("aic_list_get_%s(%s, %s, %s, %d)", suf, recv, idx, cstr(c.Path), v.Pos.Line), nil
	case types.IsStrType(xt):
		// 越界 = trap（§二.3）；裸 `(s).p[i]` 是 UB，H3/H4 都会挂
		return fmt.Sprintf("aic_str_at(%s, %s, %s, %d)", paren("", recv), idx, cstr(c.Path), v.Pos.Line), nil
	default:
		// 定长数组: 内联存储; 写入只受边界检查 (§二.2)
		// 下标先绑临时量再查（避免带副作用的索引表达式被求值两次）。
		elem, n, ok := types.ArrayElem(xt)
		if !ok {
			return fmt.Sprintf("%s[%s]", paren("", recv), idx), nil
		}
		_ = elem
		it := c.tmp("idx")
		c.line("aic_usize %s = %s;", it, idx)
		c.line("AIC_IDX_CHECK(%s, %du, %s, %d);", it, uint64(n), cstr(c.Path), v.Pos.Line)
		return fmt.Sprintf("%s[%s]", paren("", recv), it), nil
	}
}

// arrayLit 发射列表/定长数组字面量 (元素类型必须写在声明处, §二.2)。
func (c *Ctx) arrayLit(v *parse.ArrayLit, want types.Type) (string, error) {
	if want == nil {
		return "", fmt.Errorf("emit: the list literal has no target type (line %d)", v.Pos.Line)
	}
	if types.IsArray(want) {
		elem, n, _ := types.ArrayElem(want)
		// C15：复合字面量的初始化列表求值顺序未规定 → 按源码序绑定。
		wants := make([]types.Type, 0, len(v.Elems))
		for range v.Elems {
			wants = append(wants, elem)
		}
		parts, err := c.orderedList(v.Elems, wants)
		if err != nil {
			return "", err
		}
		_ = n
		return fmt.Sprintf("(%s){ %s }", c.cTypeName(want), joinComma(parts)), nil
	}
	elem := types.SliceElem(want)
	if elem == nil {
		return "", fmt.Errorf("emit: the target type of the list literal is not T[] (line %d)", v.Pos.Line)
	}
	suf, ok := c.containerSuffix(elem)
	if !ok {
		return "", fmt.Errorf("emit: list element type %s is not instantiated (line %d)", elem.String(), v.Pos.Line)
	}
	tmp := c.tmp("lit")
	c.line("%s %s = aic_list_new_%s(%du);", "aic_list_"+suf+" *", tmp, suf, v.Pos.Line)
	for _, el := range v.Elems {
		e, err := c.expr(el, elem)
		if err != nil {
			return "", err
		}
		c.line("aic_list_append_%s(%s, %s, %s, %d);", suf, tmp, e, cstr(c.Path), v.Pos.Line)
	}
	return tmp, nil
}

// compositeLit 发射 C{f: v} 复合字面量 (未列字段取零值, §四)。
// 普通 class = 区域分配引用对象 (带头); @packed = C 复合字面量 (值语义, 无头)。
// 类型头可以是裸类名、跨包限定名 pkg.C，或泛型实例 Box[i32]（字段按实参代换）。
func (c *Ctx) compositeLit(v *parse.CompositeLit) (string, error) {
	cl, inst := c.compositeClassOf(v.TypeName)
	if cl == nil {
		return "", fmt.Errorf("emit: the composite literal type name is not a known class (line %d)", v.Pos.Line)
	}
	ct := c.namedTypeName(cl.Pkg, cl.Name)
	if inst != nil {
		// 对象类型（typedef 名，不带指针）：下面自己拼 ` *` 与 sizeof。
		ct = c.registerInstance(inst)
	}
	fieldType := func(name string) types.Type {
		ft := c.fieldTypeOf(cl, name)
		if inst != nil {
			ft = types.Subst(ft, cl.TypeParams, inst.Args)
		}
		return ft
	}
	if cl.Packed {
		// 值类型: 一条 C 复合字面量表达式, 不产生语句 (可在任意表达式位使用)。
		// C15：初始化列表求值顺序未规定 → 有副作用的字段值先按声明序绑临时量。
		exprs := make([]parse.Expr, 0, len(v.Fields))
		wants := make([]types.Type, 0, len(v.Fields))
		for _, f := range v.Fields {
			exprs = append(exprs, f.Value)
			wants = append(wants, fieldType(f.Name))
		}
		vals, err := c.orderedList(exprs, wants)
		if err != nil {
			return "", err
		}
		parts := make([]string, 0, len(v.Fields))
		for i, f := range v.Fields {
			parts = append(parts, fmt.Sprintf(".%s = %s", f.Name, vals[i]))
		}
		return fmt.Sprintf("((%s){ %s })", ct, joinComma(parts)), nil
	}
	// 引用对象: 区域分配 + 对象头 + 字段存 (逐字段发射 → 需要语句位)
	tmp := c.tmp("obj")
	alloc := "aic_alloc_hdr"
	if c.liveNext {
		alloc = "aic_alloc_live" // @live：外一层区域（§五 R4）
	}
	c.line("%s *%s = (%s *)%s(sizeof(%s), %du);", ct, tmp, ct, alloc, ct, uint32(v.Pos.Line))
	// 未列字段取零值 (§四)。**不能 memset 整个对象**：对象头 (hdr.reg/hdr.line)
	// 刚由 aic_alloc_hdr 写好，清零会把区域归属与创建行一起抹掉。
	listed := map[string]bool{}
	for _, f := range v.Fields {
		listed[f.Name] = true
	}
	for i := range cl.Fields {
		fd := cl.Fields[i]
		if listed[fd.Name] {
			continue
		}
		c.line("%s->%s = %s;", tmp, fd.Name, c.zeroValue(fieldType(fd.Name)))
	}
	for _, f := range v.Fields {
		e, err := c.expr(f.Value, fieldType(f.Name))
		if err != nil {
			return "", err
		}
		c.line("%s->%s = %s;", tmp, f.Name, e)
	}
	return tmp, nil
}

// fieldTypeOf 取类字段的语义类型。
func (c *Ctx) fieldTypeOf(cl *types.Class, name string) types.Type {
	if idx, ok := cl.FieldIndex(name); ok && idx < len(cl.Fields) {
		return cl.Fields[idx].Type
	}
	return nil
}

// lambdaExpr 发射纯 lambda (编译为静态函数, 无上下文结构, §三)。
func (c *Ctx) lambdaExpr(v *parse.LambdaExpr, want types.Type) (string, error) {
	// 纯 lambda、不可捕获 → 提升为静态函数（§三）；字面量的值 = 函数名。
	return c.lambdaValue(v)
}

// matchExpr 发射 match 表达式 (全分支为表达式, 穷尽性保证必有值, §三)。
//
// 形态：先发 `switch (主体.tag)` 把各分支的值写进结果临时量，再返回该临时量
// （与 check 的 checkValueExpr 同一手法：语句前置 + 表达式取临时量）。
func (c *Ctx) matchExpr(v *parse.MatchExpr, want types.Type) (string, error) {
	if len(v.Arms) == 0 {
		return "", fmt.Errorf("emit: match has no arms (line %d)", v.Pos.Line)
	}
	subj, err := c.expr(v.Subject, nil)
	if err != nil {
		return "", err
	}
	st := c.ti(v.Subject)
	en, hasData := c.enumOf(st)
	if en == nil {
		return "", fmt.Errorf("emit: the subject of match must be an enum (line %d)", v.Pos.Line)
	}
	rt := want
	if rt == nil {
		rt = c.ti(v.Arms[0].Value)
	}
	if rt == nil {
		rt = c.literalType(v.Arms[0].Value)
	}
	if rt == nil {
		return "", fmt.Errorf("emit: the value type of the match expression is unknown (line %d)", v.Pos.Line)
	}
	tmp := c.tmp("subj")
	res := c.tmp("mexpr")
	c.line("%s %s = %s;", c.cTypeName(st), tmp, subj)
	c.line("%s %s;", c.cTypeName(rt), res)
	c.srcLine(v.Pos)
	c.line("switch (%s) {", matchSwitchExpr(tmp, hasData))
	for _, arm := range v.Arms {
		if len(arm.Patterns) == 0 {
			continue
		}
		patName := arm.Patterns[0].Name
		if i := strings.LastIndex(patName, "."); i >= 0 {
			patName = patName[i+1:]
		}
		idx, ok := en.VariantIndex(patName)
		if !ok {
			return "", fmt.Errorf("emit: unknown variant %s (line %d)", patName, arm.Pos.Line)
		}
		c.srcLine(arm.Pos)
		c.line("case %d: {", idx)
		if hasData && arm.Patterns[0].Kind == "payload" {
			bind := arm.Patterns[0].Binding
			if bind != "" && bind != "_" {
				payload, _ := en.VariantPayload(patName)
				if payload != nil {
					if inst, isInst := st.(*types.Instance); isInst {
						payload = types.Subst(payload, en.TypeParams, inst.Args)
					}
					c.line("    %s %s = %s.u.%s;", c.cTypeName(payload), bind, tmp, patName)
				}
			}
		}
		if arm.Value != nil {
			e, err := c.expr(arm.Value, rt)
			if err != nil {
				return "", err
			}
			c.line("    %s = %s;", res, e)
		}
		c.line("    break;")
		c.line("}")
	}
	c.line("}")
	return res, nil
}

// ti 取表达式节点的检查类型 (缺失时返回 nil, 由调用方按上下文兜底)。
// 实现见 names.go (含 pass 1 名字表兜底)。

// nodeLine 取表达式所在行 (诊断用)。
func nodeLine(e parse.Expr) int {
	if e == nil {
		return 0
	}
	return parse.ExprPos(e).Line
}

// unquoteAIC 去掉字符串字面量的引号并还原转义 (§一: 仅 \n \t \r \" \\)。
func unquoteAIC(raw string) string {
	if len(raw) < 2 || raw[0] != '"' {
		return raw
	}
	body := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 >= len(body) {
			b.WriteByte(body[i])
			continue
		}
		i++
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}
