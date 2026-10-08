package air

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 表达式 → AIR 值（lower_expr.go）。
//
// 纪律（§10.1）：**有副作用的子表达式一律提升为独立 let**，实参与二元操作数的
// 左到右由语句序编码 —— C 层不得依赖 C 的求值顺序。
// 类型名/符号名复用 §八 mangling 单表（禁第二份拼法）。
// ---------------------------------------------------------------------------

// value 把表达式降级成一个值名（必要时先发 let）。
func (l *lowerer) value(e parse.Expr) (string, error) {
	switch v := e.(type) {
	case *parse.IntLit:
		return "const " + v.Text, nil
	case *parse.FloatLit:
		return "const " + v.Text, nil
	case *parse.StrLit:
		return "const " + v.Text, nil
	case *parse.InterpLit:
		return l.interpLit(v)
	case *parse.BoolLit:
		if v.Value {
			return "const true", nil
		}
		return "const false", nil
	case *parse.NilLit:
		return "nil", nil
	case *parse.Ident:
		// N1 闭包：捕获引用读环境槽（按节点判定，同名局部量遮蔽不受影响）。
		if l.envCur != "" {
			if slot, ok := l.info.CapRef(v); ok {
				t := l.tmp("t")
				l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v),
					Rhs: &FieldRHS{Place: &VarPlace{Name: l.envCur}, Idx: slot}, Loc: LocOf(v.Pos)})
				return t, nil
			}
		}
		// 裸 None（std Option 的无负载变体）= 符号常量，不是未定义名字。
		if v.Name == "None" {
			return "const None", nil
		}
		// 包级常量：AIR 里是**字面量**（与 emit 同一口径：常量不占运行时存储）。
		if l.info != nil {
			if sym, ok := l.info.Consts[v.Name]; ok && sym != nil {
				return constText(sym.Const), nil
			}
		}
		return v.Name, nil
	case *parse.ThisExpr:
		return "this__", nil
	case *parse.Unary:
		a, err := l.value(v.X)
		if err != nil {
			return "", err
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v), Rhs: &Unop{Op: v.Op, A: a}, Loc: LocOf(v.Pos)})
		return t, nil
	case *parse.Binary:
		a, err := l.value(v.Left)
		if err != nil {
			return "", err
		}
		b, err := l.value(v.Right)
		if err != nil {
			return "", err
		}
		t := l.tmp("t")
		ty := l.tyOf(v)
		var rhs RHS
		switch v.Op {
		case "+", "-", "*", "/", "%", "&", "|", "^", "<<", ">>":
			rhs = &Binop{Op: v.Op, A: a, B: b}
		default:
			rhs = &Cmp{Op: v.Op, A: a, B: b}
		}
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: ty, Rhs: rhs, Loc: LocOf(v.Pos)})
		return t, nil
	case *parse.Call:
		return l.call(v)
	case *parse.Field:
		// **枚举变体引用**（`Color.Red` / `lib.Color.green`）：基座是**类型名**，不是值。
		// 判据 = 该表达式在检查器里的类型是 enum —— enum 没有字段，字段读不可能定型成
		// enum，所以这条判定不会与真字段读相撞（唯一判据，不另立名字表）。
		// 曾经这里直接走通用字段路径：`Color.Green` 被降成 `field Color, 0`（fieldIndex
		// 对非类接收者返回 0），**静默错译成第一个变体**（026 实测 red/red/red）。
		if en, ok := types.IsEnum(l.typeOf(v)); ok {
			idx, _, found := types.VariantByName(en, v.Name)
			if !found {
				return "", fmt.Errorf("air: %s is not a variant of enum %s (line %d)", v.Name, en.Name, parse.ExprPos(v).Line)
			}
			t := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.ty(en),
				Rhs: &FieldRHS{Place: &VarPlace{Name: l.ty(en)}, Idx: idx}, Loc: LocOf(v.Pos)})
			return t, nil
		}
		// **跨包常量**（`base.Limit`）：常量是字面量，AIR 里也是字面量（与
		// 本包常量同口径：不占运行时存储）。基座是 import 的包名 —— 走字段路径
		// 会把它当对象成员（chain3 实测：字段下标解析失败）。
		if id, isID := v.X.(*parse.Ident); isID && l.info != nil {
			if sym, ok := l.info.Consts[id.Name+"."+v.Name]; ok && sym != nil {
				return constText(sym.Const), nil
			}
		}
		// **接收者的 place** + 本字段下标 —— 不能写 `place(v)`：那已经是一次
		// `field base, idx` 的 place，再套一层 `FieldRHS{Place: 它, Idx: idx}` 就成了
		// `field (field x, i), i`（**读两次字段**）。此前 AIR 的每个字段读都被降成双层，
		// 与直译路径不一致；因为发射走直译路径、verifier 只查结构，这个洞一直没暴露。
		pl, err := l.basePlace(v.X)
		if err != nil {
			return "", err
		}
		t := l.tmp("t")
		idx, ok := l.fieldIndex(v)
		if !ok {
			return "", fmt.Errorf("air: cannot resolve the field index of %s (receiver type unknown, line %d)",
				exprFieldText(v), parse.ExprPos(v).Line)
		}
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v), Rhs: &FieldRHS{Place: pl, Idx: idx}, Loc: LocOf(v.Pos)})
		return t, nil
	case *parse.Index:
		// str 视图 `s[lo..hi]`：显式化成 StrViewRHS（零拷贝 + 运行期边界检查）。
		// 曾经这里直接走 place()，StrViewPlace 又把 Lo/Hi 丢掉 ⇒ emit 只剩一个裸基座，
		// "视图未发射"其实是"视图的边界在降级时被丢了"（720 实测）。
		if v.End != nil {
			base, err := l.value(v.X)
			if err != nil {
				return "", err
			}
			lo, err := l.value(v.Index)
			if err != nil {
				return "", err
			}
			hi, err := l.value(v.End)
			if err != nil {
				return "", err
			}
			t := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "str",
				Rhs: &StrViewRHS{Base: base, Lo: lo, Hi: hi}, Loc: LocOf(v.Pos)})
			return t, nil
		}
		pl, err := l.place(v)
		if err != nil {
			return "", err
		}
		idx, err := l.value(v.Index)
		if err != nil {
			return "", err
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v), Rhs: &ElemRHS{Place: pl, Idx: idx}, Loc: LocOf(v.Pos)})
		return t, nil
	case *parse.CompositeLit:
		return l.composite(v)
	case *parse.CheckExpr:
		return l.checkExpr(v)
	case *parse.OrExpr:
		return l.orExpr(v)
	case *parse.CatchExpr:
		return l.catchExpr(v)
	case *parse.AssertExpr:
		return l.assertExpr(v)
	case *parse.MatchExpr:
		return l.matchExpr(v)
	case *parse.LambdaExpr:
		return l.lambdaExpr(v)
	case *parse.ArrayLit:
		return l.arrayLit(v)
	}
	return "", fmt.Errorf("air: unsupported expression %T", e)
}

// call：自由函数 / 方法 / 构造 / 转换 → call / call.ind（接口）/ alloc（构造）。
//
// **方法调用的接收者必须是第一个实参**：AIR 里方法体的首形参就是 `this__`
// （见 funcDecl），调用点不带接收者会让 AIR 与符号签名不一致 —— 那是"能过
// verifier 但语义错"的形态，T-B 的 C 发射无法复原。
func (l *lowerer) call(v *parse.Call) (string, error) {
	// N4 编译期内省：折成常量或 sizeof，**绝不**把它当普通调用降级
	// （`typeName(Pair)` 的实参是类型名，当值用会触发 V2.4）。
	if e, handled, err := l.comptimeCall(v); handled {
		return e, err
	}
	// 闭包值调用（N1）：被调是函数类型 → 值自身携带环境。
	if id, ok := v.Fn.(*parse.Ident); ok {
		if _, isFn := l.typeOf(id).(*types.FuncT); isFn {
			val, err := l.value(id)
			if err != nil {
				return "", err
			}
			args := make([]string, 0, len(v.Args))
			for _, a := range v.Args {
				x, err := l.value(a)
				if err != nil {
					return "", err
				}
				args = append(args, x)
			}
			t := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v),
				Rhs: &CallClosure{Val: val, Args: args}, Loc: LocOf(v.Pos)})
			return t, nil
		}
	}
	// 接口方法：接收者是接口类型 → call.ind（按 vt 槽位，§10.2）
	if field, ok := v.Fn.(*parse.Field); ok {
		if recvTy := l.typeOf(field.X); recvTy != nil {
			if ifc, isIfc := types.IsInterface(recvTy); isIfc {
				recv, err := l.value(field.X)
				if err != nil {
					return "", err
				}
				args := make([]string, 0, len(v.Args))
				for _, a := range v.Args {
					val, err := l.value(a)
					if err != nil {
						return "", err
					}
					args = append(args, val)
				}
				slot := ifc.SlotOf(field.Name)
				if slot < 0 {
					return "", fmt.Errorf("air: interface %s has no slot for %s (line %d)", ifc.Name, field.Name, v.Pos.Line)
				}
				args = l.coerceArgs(v.Args, args, ifcSlotParams(ifc, field.Name))
				t := l.tmp("t")
				l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(v),
					Rhs: &CallInd{Recv: recv, Slot: slot, Args: args}, Loc: LocOf(v.Pos)})
				return t, nil
			}
		}
	}
	// 方法调用（类 / 实例 / 容器 / chan / mutex）：接收者作为首个实参。
	recvVal := ""
	isMethod := false
	if field, ok := v.Fn.(*parse.Field); ok {
		if rt := l.typeOf(field.X); rt != nil {
			isMethod = l.methodRecv(field.X)
			if isMethod {
				val, err := l.value(field.X)
				if err != nil {
					return "", err
				}
				recvVal = val
			}
		}
	}
	args := make([]string, 0, len(v.Args)+1)
	if isMethod {
		args = append(args, recvVal)
	}
	for _, a := range v.Args {
		val, err := l.value(a)
		if err != nil {
			return "", err
		}
		args = append(args, val)
	}
	// 实参处的隐式装箱（`describe(rect)` 的形参是接口）。
	params := l.callParams(v)
	if isMethod {
		args = append(args[:1], l.coerceArgs(v.Args, args[1:], params)...)
	} else {
		args = l.coerceArgs(v.Args, args, params)
	}
	// N10 委托方法：接收者换成 `recv.<字段>`，被调 = 目标类的同名方法。
	if f, ok := v.Fn.(*parse.Field); ok && isMethod {
		if inner, sym2, handled := l.delegateCall(v, f, recvVal); handled {
			args[0] = inner
			return l.emitCall(v, sym2, nil, args)
		}
	}
	sym, ta := l.callee(v)
	return l.emitCall(v, sym, ta, args)
}

// delegateCall 处理 N10 的委托方法调用：`a.m()` → 目标类方法 + 接收者换成 `a.<字段>`。
// 返回 (值名, 符号名, 是否已处理)。
func (l *lowerer) delegateCall(v *parse.Call, field *parse.Field, recvVal string) (string, string, bool) {
	rt := l.typeOf(field.X)
	cl, isCl := types.IsClass(rt)
	if !isCl {
		return "", "", false
	}
	sig, ok := cl.Method(field.Name)
	if !ok || sig.DelegateField == "" {
		return "", "", false
	}
	target, ok := l.info.Classes[sig.DelegateClass]
	if !ok {
		return "", "", false
	}
	idx, ok := cl.FieldIndex(sig.DelegateField)
	if !ok {
		return "", "", false
	}
	inner := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: inner, Ty: l.ty(target),
		Rhs: &FieldRHS{Place: &VarPlace{Name: recvVal}, Idx: idx}, Loc: LocOf(v.Pos)})
	return inner, mangleFunc(target.Pkg, target.Name, field.Name), true
}

// emitCall 发一条调用：有返回位才建结果临时量；无返回位 = `void`（AIR 里仍是一条
// let，好让「调用发生过」这件事在 IR 里显式可见 —— C 侧发射成裸调用语句）。
func (l *lowerer) emitCall(v *parse.Call, sym string, ta, args []string) (string, error) {
	t := l.tmp("t")
	ty := l.tyOf(v)
	if len(l.callResults(v)) == 0 {
		ty = "void"
	}
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: ty,
		Rhs: &Call{Sym: sym, TypeArgs: ta, Args: args}, Loc: LocOf(v.Pos)})
	return t, nil
}

// callResults 取被调签名/类型的返回位（用于区分 void 调用）。
func (l *lowerer) callResults(v *parse.Call) []types.Type {
	switch fn := v.Fn.(type) {
	case *parse.Ident:
		if sig, ok := l.info.Funcs[fn.Name]; ok {
			return sig.Results
		}
		if _, isFn := l.typeOf(fn).(*types.FuncT); isFn {
			if inst := types.MultiElems(l.typeOf(v)); inst != nil {
				return inst
			}
			if t := l.typeOf(v); t != nil {
				return []types.Type{t}
			}
		}
	case *parse.Field:
		if rt := l.typeOf(fn.X); rt != nil {
			if cl, isCl := types.IsClass(rt); isCl {
				if sig, ok := cl.Method(fn.Name); ok {
					return sig.Results
				}
				return nil
			}
		}
	}
	// 兜底：按调用点记下的类型判空（多返回 = MultiType，单返回 = 具体类型）
	if t := l.typeOf(v); t != nil {
		if elems := types.MultiElems(t); elems != nil {
			return elems
		}
	}
	if l.typeOf(v) == nil {
		return nil
	}
	return []types.Type{l.typeOf(v)}
}

// constText 把一个折叠后的常量渲染成 AIR 常量值（与源码字面量同形）。
func constText(cv types.ConstVal) string {
	switch cv.Kind {
	case types.ConstInt:
		return fmt.Sprintf("const %d", cv.Int)
	case types.ConstUint:
		return fmt.Sprintf("const %d", cv.Uint)
	case types.ConstFloat:
		return fmt.Sprintf("const %g", cv.Float)
	case types.ConstStr:
		return "const " + strconv.Quote(cv.Str)
	case types.ConstBool:
		if cv.Bool {
			return "const true"
		}
		return "const false"
	}
	return "const 0"
}

// comptimeCall 降级内省原语（N4）：typeName / isValueType 已在 checker 折成常量，
// sizeOf 可折则常量、不可折则 `sizeof <ty>`（真尺寸只由 C 给）。
func (l *lowerer) comptimeCall(v *parse.Call) (string, bool, error) {
	cv, ok := l.info.ComptimeOf(v)
	if !ok {
		return "", false, nil
	}
	switch cv.Kind {
	case "typeName":
		return "const \"" + cv.Const.Str + "\"", true, nil
	case "isValueType":
		if cv.Const.Bool {
			return "const true", true, nil
		}
		return "const false", true, nil
	case "sizeOf":
		if cv.Const.Kind == types.ConstInt {
			return fmt.Sprintf("const %d", cv.Const.Int), true, nil
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "i32",
			Rhs: &SizeOf{Ty: l.ty(cv.Ty)}, Loc: LocOf(v.Pos)})
		return t, true, nil
	}
	return "", false, fmt.Errorf("air: unknown comptime intrinsic %s (line %d)", cv.Kind, v.Pos.Line)
}

// callee 取被调符号名（mangling 单表）与泛型实参表。
//
// 运行时构造（chan/set/map/sync.Mutex）与类的 init 工厂也在这里归名：
// 符号名 = 运行时实例化名的 AIC 拼写（chan_new_i32 / map_new_str_i32 /
// sync_Mutex_new / <pkg>_C_init），C 后端按同一张后缀表映射到 aic_* 符号。
func (l *lowerer) callee(v *parse.Call) (string, []string) {
	switch fn := v.Fn.(type) {
	case *parse.Ident:
		if sig, ok := l.info.Funcs[fn.Name]; ok {
			return mangleFunc(l.in.Pkg, "", sig.Name), l.typeArgsOf(v)
		}
		// 类工厂：C(args) → <pkg>_C_init（init 是至多一个的构造函数，§四）
		if cl, ok := l.info.Classes[fn.Name]; ok {
			return mangleFunc(cl.Pkg, cl.Name, "init"), nil
		}
		return fn.Name, nil
	case *parse.Index:
		// **显式类型实参的调用** `id[i32](x)` / `Box[Box[i32]](...)`：Fn 是索引表达式，
		// 此前落到 `unknown`（AIR 里根本无法复原要哪个实例）。实参表同样由检查器登记
		// （CallTypeArgs 的键是**调用节点**，两条路共用一份）。
		switch base := fn.X.(type) {
		case *parse.Ident:
			if sig, ok := l.info.Funcs[base.Name]; ok {
				return mangleFunc(l.in.Pkg, "", sig.Name), l.typeArgsOf(v)
			}
			if cl, ok := l.info.Classes[base.Name]; ok {
				return mangleFunc(cl.Pkg, cl.Name, "init"), l.typeArgsOf(v)
			}
			return base.Name, l.typeArgsOf(v)
		case *parse.Field:
			if id, isID := base.X.(*parse.Ident); isID {
				return id.Name + "_" + base.Name, l.typeArgsOf(v)
			}
		}
		return "unknown", nil
	case *parse.Field:
		// 容器 / 通道 / 同步原语的构造
		if fn.Name == "new" && !isValueRecv(l, fn.X) {
			if sym, ok := l.runtimeCtor(v); ok {
				return sym, nil
			}
		}
		// 方法：接收者类型决定所属包
		if rt := l.typeOf(fn.X); rt != nil {
			if cl, isCl := types.IsClass(rt); isCl {
				return mangleFunc(cl.Pkg, cl.Name, fn.Name), nil
			}
			if inst, isInst := rt.(*types.Instance); isInst {
				if cl, isCl := types.IsClass(inst.Base); isCl {
					return mangleFunc(cl.Pkg, cl.Name, fn.Name), l.typeArgsOfInstance(inst)
				}
			}
			// 内建方法（容器/chan/str/mutex）：符号 = <接收者类型段>_<方法名>
			return recvTypeSym(rt) + "_" + fn.Name, nil
		}
		if id, isID := fn.X.(*parse.Ident); isID {
			// 类型级方法（N4）：`Class.default()` → <pkg>_<Cls>_default
			if cl, isCl := l.info.Classes[id.Name]; isCl {
				if _, has := cl.Method(fn.Name); has {
					return mangleFunc(cl.Pkg, cl.Name, fn.Name), nil
				}
			}
			return id.Name + "_" + fn.Name, nil // 包函数（std）按 包_函数 记名
		}
	}
	return "unknown", nil
}

// isValueRecv 报告 `X.new()` 里的 X 是不是一个**值**（局部变量），
// 而不是类型名 —— 值上没有 new 方法，出现即检查器已报错。
func isValueRecv(l *lowerer, x parse.Expr) bool {
	if id, ok := x.(*parse.Ident); ok {
		if _, isLocal := l.info.Funcs[id.Name]; isLocal {
			return true
		}
		if _, isCl := l.info.Classes[id.Name]; isCl {
			return false
		}
		if id.Name == "chan" {
			return false
		}
	}
	return false
}

// runtimeCtor 给运行时容器/通道构造命名（后缀表唯一所有者 = types.RuntimeSuffix）。
func (l *lowerer) runtimeCtor(v *parse.Call) (string, bool) {
	t := l.typeOf(v)
	switch {
	case t == nil:
		return "", false
	case isChanType(t):
		ch, _ := t.(*types.ChanT)
		suf, ok := types.RuntimeSuffix(ch.Elem)
		if !ok {
			return "", false
		}
		return "chan_new_" + suf, true
	case types.IsSet(t):
		suf, ok := types.RuntimeSuffix(types.SetElem(t))
		if !ok {
			return "", false
		}
		return "set_new_" + suf, true
	case types.IsMap(t):
		k, val, ok := types.MapParts(t)
		if !ok {
			return "", false
		}
		ks, ok1 := types.RuntimeMapKeySuffix(k)
		vs, ok2 := types.RuntimeMapValueSuffix(val)
		if !ok1 || !ok2 {
			return "", false
		}
		return "map_new_" + ks + "_" + vs, true
	case isMutexType(t):
		return "sync_Mutex_new", true
	}
	return "", false
}

func isChanType(t types.Type) bool {
	_, ok := t.(*types.ChanT)
	return ok
}

func isMutexType(t types.Type) bool {
	_, ok := t.(*types.MutexT)
	return ok
}

// recvTypeSym 是内建方法接收者的符号段（容器/chan/str/bytes 等）。
func recvTypeSym(t types.Type) string {
	if types.IsBytes(t) {
		return "bytes"
	}
	if ch, ok := t.(*types.ChanT); ok {
		if suf, ok2 := types.RuntimeSuffix(ch.Elem); ok2 {
			return "chan_" + suf
		}
		return "chan"
	}
	if _, ok := t.(*types.MutexT); ok {
		return "sync_Mutex"
	}
	if types.IsStrType(t) {
		return "str"
	}
	if types.IsSlice(t) {
		if suf, ok := types.RuntimeSuffix(types.SliceElem(t)); ok {
			return "list_" + suf
		}
		return "list"
	}
	if types.IsSet(t) {
		if suf, ok := types.RuntimeSuffix(types.SetElem(t)); ok {
			return "set_" + suf
		}
		return "set"
	}
	if types.IsMap(t) {
		k, val, ok := types.MapParts(t)
		if ok {
			ks, ok1 := types.RuntimeMapKeySuffix(k)
			vs, ok2 := types.RuntimeMapValueSuffix(val)
			if ok1 && ok2 {
				return "map_" + ks + "_" + vs
			}
		}
		return "map"
	}
	if types.IsArray(t) {
		if e, _, ok := types.ArrayElem(t); ok {
			if suf, ok2 := types.RuntimeSuffix(e); ok2 {
				return "array_" + suf
			}
		}
		return "array"
	}
	return "opaque"
}

// composite：创建表达式 → alloc（唯一分配来源）+ 逐字段 store。
func (l *lowerer) composite(v *parse.CompositeLit) (string, error) {
	ty := l.tyOf(v)
	if ty == "" {
		return "", fmt.Errorf("air: composite literal without a type (line %d)", v.Pos.Line)
	}
	obj := l.tmp("obj")
	kind := "region"
	if l.depth == 0 {
		kind = "task" // 深度 0 = 任务区域（§五 R1）
	}
	// alloc 分配的是**对象类型**（不带引用后缀）；变量/返回位才是指针形态。
	objTy := strings.TrimSuffix(ty, "*")
	ptr := ""
	if objTy != ty {
		ptr = "*"
	}
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: obj, Ty: objTy + ptr,
		Rhs: &Alloc{Kind: kind, Ty: objTy}, Loc: LocOf(v.Pos)})
	// 字段存储：下标 = **类声明序**（不是字面量里的位置 —— `Point{y:2, x:1}`
	// 按位置写会静默把 y 存进 x 的槽）。未列出的字段 = 零值（§二.3）：区域分配
	// 不清零（只复位 bump 指针），不显式存零 = 新对象读到该内存上一个对象的
	// 残留 —— 716/752 实测（`Log{}` 的 s/n 是垃圾，`this.s + t` 拿垃圾长度
	// 去分配，只在特定内存布局下不炸）。这是 soundness 洞不是性能问题。
	// 泛型**实例**（`Box[i32]{…}`）的类型是 Instance，字段表在基类上（类型按
	// 实例实参代换）—— 782 实测：只认 Class 会把 instance 的存储整段跳过。
	cl, isCl := types.IsClass(l.typeOf(v))
	if !isCl {
		if inst, isInst := l.typeOf(v).(*types.Instance); isInst {
			cl, isCl = types.IsClass(inst.Base)
		}
	}
	if isCl {
		listed := map[string]parse.Expr{}
		for _, f := range v.Fields {
			listed[f.Name] = f.Value
		}
		for i := range cl.Fields {
			fd := cl.Fields[i]
			place := &FieldPlace{Base: &VarPlace{Name: obj}, Idx: i}
			valExpr, has := listed[fd.Name]
			if !has {
				l.cur.Insts = append(l.cur.Insts, &Store{
					Place: place, Val: "nil", Loc: LocOf(v.Pos), // emit 按目标字段类型取零值
				})
				continue
			}
			val, err := l.value(valExpr)
			if err != nil {
				return "", err
			}
			l.cur.Insts = append(l.cur.Insts, &Store{
				Place: place, Val: val, Loc: LocOf(v.Pos),
			})
		}
	} else {
		// 非类/实例（理论上到不了）：保守按字面量位置存，至少不丢写入。
		for i, f := range v.Fields {
			val, err := l.value(f.Value)
			if err != nil {
				return "", err
			}
			l.cur.Insts = append(l.cur.Insts, &Store{
				Place: &FieldPlace{Base: &VarPlace{Name: obj}, Idx: i},
				Val:   val, Loc: LocOf(f.Pos),
			})
		}
	}
	return obj, nil
}

// place：lvalue → 显式 place 节点（可嵌套）。
func (l *lowerer) place(e parse.Expr) (Place, error) {
	switch v := e.(type) {
	case *parse.Ident:
		return &VarPlace{Name: v.Name}, nil
	case *parse.Field:
		base, err := l.basePlace(v.X)
		if err != nil {
			return nil, err
		}
		idx, ok := l.fieldIndex(v)
		if !ok {
			// 下标取不到 = 接收者类型不在检查产物里（记录缺陷），绝不能默认 0：
			// 那会把 `this.b = 2` 静默写进字段 a（752 实测，红线 23）。
			return nil, fmt.Errorf("air: cannot resolve the field index of %s (receiver type unknown, line %d)",
				exprFieldText(v), parse.ExprPos(v).Line)
		}
		return &FieldPlace{Base: base, Idx: idx}, nil
	case *parse.Index:
		base, err := l.basePlace(v.X)
		if err != nil {
			return nil, err
		}
		if v.End != nil {
			// 视图的 place 形态：Lo/Hi 必须带着（丢了边界 = 丢了语义，720 实测）。
			lo, err := l.value(v.Index)
			if err != nil {
				return nil, err
			}
			hi, err := l.value(v.End)
			if err != nil {
				return nil, err
			}
			return &StrViewPlace{Base: base, Lo: lo, Hi: hi}, nil
		}
		idx, err := l.value(v.Index)
		if err != nil {
			return nil, err
		}
		return &ElemPlace{Base: base, Idx: idx}, nil
	case *parse.ThisExpr:
		return &VarPlace{Name: "this__"}, nil
	}
	return nil, fmt.Errorf("air: not an lvalue (%T at line %d)", e, parse.ExprPos(e).Line)
}

// exprFieldText 渲染字段访问的源码形态（诊断用：与 types 的 exprText 同规则，
// 但 air 不依赖 types 的未导出实现 —— 各写一份渲染会让两边的文案分叉）。
func exprFieldText(v *parse.Field) string {
	switch b := v.X.(type) {
	case *parse.ThisExpr:
		return "this." + v.Name
	case *parse.Ident:
		return b.Name + "." + v.Name
	}
	return v.Name
}

// basePlace 取字段/下标访问的基址 place：不是 lvalue（如调用结果）时先把值物化成
// 临时量再当 place 用 —— AIR 的 place 只认名字，故这一步是显式化的必要代价。
func (l *lowerer) basePlace(e parse.Expr) (Place, error) {
	if p, err := l.place(e); err == nil {
		return p, nil
	}
	val, err := l.value(e)
	if err != nil {
		return nil, err
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.tyOf(e),
		Rhs: &TmpRef{Name: val}, Loc: LocOf(parse.ExprPos(e))})
	return &VarPlace{Name: t}, nil
}

// placeValue 读一个 place 的当前值（复合赋值用）。
func (l *lowerer) placeValue(p Place, at parse.Pos) (string, error) {
	t := l.tmp("t")
	var rhs RHS
	switch v := p.(type) {
	case *VarPlace:
		return v.Name, nil
	case *FieldPlace:
		rhs = &FieldRHS{Place: v.Base, Idx: v.Idx}
	case *ElemPlace:
		rhs = &ElemRHS{Place: v.Base, Idx: v.Idx}
	default:
		return "", fmt.Errorf("air: cannot read this place")
	}
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "i32", Rhs: rhs, Loc: LocOf(at)})
	return t, nil
}

// fieldIndex 取字段在声明序中的下标（ABI 冻结：禁重排，V1.4）。
func (l *lowerer) fieldIndex(v *parse.Field) (int, bool) {
	rt := l.typeOf(v.X)
	// Err 的字段（code/msg/cause）：冻结下标见 types.ErrFieldIndex（唯一实现）。
	if types.IsErrType(rt) {
		return types.ErrFieldIndex(v.Name)
	}
	if cl, isCl := types.IsClass(rt); isCl {
		return cl.FieldIndex(v.Name)
	}
	if inst, isInst := rt.(*types.Instance); isInst {
		if cl, isCl := types.IsClass(inst.Base); isCl {
			return cl.FieldIndex(v.Name)
		}
	}
	return 0, false
}

// tyOf 取表达式在检查器里的类型文本（未定型字面量给空串，由调用点兜底）。
func (l *lowerer) tyOf(e parse.Expr) string {
	if t := l.typeOf(e); t != nil {
		return l.ty(t)
	}
	return "i32"
}

// typeArgsOf 取泛型调用的实参表（键与名同源，§10.2）。
func (l *lowerer) typeArgsOf(v *parse.Call) []string {
	// **首选检查器登记的实例实参**（推断调用在 AIR 里看不出实参：`id(x)` 的调用节点
	// 类型是结果类型，不是实例）。没有登记时回退到"结果类型是实例"的老路。
	if l.info != nil {
		if args, ok := l.info.CallTypeArgs[v]; ok && len(args) > 0 {
			out := make([]string, 0, len(args))
			for _, a := range args {
				out = append(out, l.ty(a))
			}
			return out
		}
	}
	t := l.typeOf(v)
	if inst, ok := t.(*types.Instance); ok {
		return l.typeArgsOfInstance(inst)
	}
	return nil
}

func (l *lowerer) typeArgsOfInstance(inst *types.Instance) []string {
	out := make([]string, 0, len(inst.Args))
	for _, a := range inst.Args {
		out = append(out, l.ty(a))
	}
	return out
}

// --- 类型与符号文本（§八 mangling 单表的 AIR 侧投影） -------------------------

// tyText 是类型在 `.air` 里的规范拼写（键与名同源：闭集基本名 + <pkg>_<Name>）。
func tyText(t types.Type) string {
	if t == nil {
		return "void"
	}
	switch v := t.(type) {
	case *types.Basic:
		return v.Name
	case *types.Class:
		return mangleType(v.Pkg, v.Name) + refSuffix(v)
	case *types.Enum:
		return mangleType(v.Pkg, v.Name)
	case *types.Interface:
		return mangleType(v.Pkg, v.Name)
	case *types.Slice:
		return tyText(v.Elem) + "[]"
	case *types.SetT:
		return "set[" + tyText(v.Elem) + "]"
	case *types.MapT:
		return "map[" + tyText(v.Key) + "]" + tyText(v.Value)
	case *types.ArrayT:
		return fmt.Sprintf("[%s;%d]", tyText(v.Elem), v.N)
	case *types.ChanT:
		return "chan[" + tyText(v.Elem) + "]"
	case *types.MutexT:
		return "sync_Mutex"
	case *types.Instance:
		// 基类的引用标记 `*` 属于**最外层**：`ok_Box[i32]*`。写成 `ok_Box*[i32]`
		// （星号嵌在实例实参中间）会让 emit 侧所有「尾部判 `*`」的规则失效：
		// `b.v` 被发成 `.` 而不是 `->`、链式字段类型解析直接落空（740 实测）。
		base := tyText(v.Base)
		ref := strings.HasSuffix(base, "*")
		out := strings.TrimSuffix(base, "*")
		for i, a := range v.Args {
			if i == 0 {
				out += "["
			} else {
				out += ","
			}
			out += tyText(a)
		}
		out += "]"
		if ref {
			out += "*"
		}
		return out
	case *types.FuncT:
		out := "fn("
		for i, p := range v.Params {
			if i > 0 {
				out += ","
			}
			out += tyText(p)
		}
		return out + ")->" + tyText(v.Result)
	case *types.MultiType:
		// 多返回值的类型文本必须是 **AIR 文本的元组**（`(trap_Buf*, trap_Buf*)`）：
		// 走 t.String() 会把点号形态（`trap.Buf`）带进类型表键，emit 的
		// irRetNameOf 查不到映射（780 实测）。
		out := "("
		for i, e := range v.Elems {
			if i > 0 {
				out += ", "
			}
			out += tyText(e)
		}
		return out + ")"
	case *types.TypeParam:
		return "T" + v.Name
	}
	return t.String()
}

// refSuffix：引用类在 AIR 里带 `*`（值类无）。
func refSuffix(cl *types.Class) string {
	if cl.Packed {
		return ""
	}
	return "*"
}

func mangleType(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "_" + name
}

func mangleFunc(pkg, recv, name string) string { return FuncSym(pkg, recv, name) }

func sigText(sig *types.FuncSig) string {
	if sig == nil {
		return "()"
	}
	out := "("
	for i, p := range sig.ParamTypes {
		if i > 0 {
			out += ", "
		}
		out += tyText(p)
	}
	out += ") -> ("
	for i, r := range sig.Results {
		if i > 0 {
			out += ", "
		}
		out += tyText(r)
	}
	return out + ")"
}

// sortedKeys 是 map 键的确定序遍历（AIR 文本禁 map 序，H8/V7.1）。
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// typeExprOf 把 parse 类型表达式解析成语义类型（复用检查器的解析结果：
// 这里只处理声明处能直接查到的具名/基本/容器形态）。
func typeExprOf(te parse.TypeExpr, info *types.Info) types.Type {
	// **首选检查器的解析结果**（唯一来源）：这份表的解析是完整的（实例实参、泛型嵌套、
	// 跨包具名类型都在），本函数下面的分支只是给"检查器没登记过的合成节点"兜底。
	if info != nil && te != nil {
		if t, ok := info.TypeExprTypes[te]; ok && t != nil {
			return t
		}
	}
	switch v := te.(type) {
	case *parse.BasicType:
		if b, ok := basicByName(v.Name); ok {
			return b
		}
	case *parse.NamedType:
		if v.Pkg == "" {
			if b, ok := basicByName(v.Name); ok {
				return b
			}
			if info != nil {
				if cl, ok := info.Classes[v.Name]; ok {
					return cl
				}
				if en, ok := info.Enums[v.Name]; ok {
					return en
				}
				if ifc, ok := info.Interfaces[v.Name]; ok {
					return ifc
				}
			}
		}
	case *parse.SliceType:
		if e := typeExprOf(v.Elem, info); e != nil {
			return &types.Slice{Elem: e}
		}
	case *parse.ArrayType:
		if e := typeExprOf(v.Elem, info); e != nil {
			return &types.ArrayT{Elem: e}
		}
	case *parse.MapType:
		k, val := typeExprOf(v.Key, info), typeExprOf(v.Value, info)
		if k != nil && val != nil {
			return &types.MapT{Key: k, Value: val}
		}
	case *parse.SetType:
		if e := typeExprOf(v.Elem, info); e != nil {
			return &types.SetT{Elem: e}
		}
	}
	return nil
}

func basicByName(name string) (types.Type, bool) {
	switch name {
	case "i8":
		return types.TI8, true
	case "i16":
		return types.TI16, true
	case "i32":
		return types.TI32, true
	case "i64":
		return types.TI64, true
	case "u8":
		return types.TU8, true
	case "u16":
		return types.TU16, true
	case "u32":
		return types.TU32, true
	case "u64":
		return types.TU64, true
	case "usize":
		return types.TUsize, true
	case "f32":
		return types.TF32, true
	case "f64":
		return types.TF64, true
	case "bool":
		return types.TBool, true
	case "str":
		return types.TStr, true
	case "Err":
		return types.TErr, true
	}
	return nil, false
}

// errSelectArm 是 select 分支形态不合规格的内部错误（检查器已挡住，这里是防御）。
var errSelectArm = fmt.Errorf("air: malformed select arm")
