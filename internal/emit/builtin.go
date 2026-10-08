package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 容器/str 内建方法 (核心设计 §二.2) 与 std 包函数 (§十四) 的发射。
//
// 运行时符号表: 容器方法 → aic_list_<op>_<SUF> / aic_map_<op>_<KS>_<VS> /
// aic_set_<op>_<SUF>; str 包 → aic_str_* / aic_std_str_*。
// ---------------------------------------------------------------------------

// tryMethodCall: 接收者方法调用 (容器 / str.len / 类方法 / 接口方法)。
func (c *Ctx) tryMethodCall(v *parse.Call, want types.Type) (string, bool, error) {
	field, ok := v.Fn.(*parse.Field)
	if !ok {
		return "", false, nil
	}
	// 容器构造 set.new() / map.new(): 类型来自声明处 (§二.2)
	if ctor, isCtor := c.containerCtor(v, want); isCtor {
		e, err := c.emitContainerNew(ctor, v.Pos.Line)
		return e, true, err
	}
	// N7 `bytes` 的类型级构造：`bytes` 是**类型名**不是值，接收者没有类型条目，
	// 故必须在取接收者类型之前处理（bytes.new / withCap / fromStr）。
	if id, isID := field.X.(*parse.Ident); isID && id.Name == "bytes" {
		return c.emitBytesCtor(v, field.Name)
	}
	// N4 类型级方法：`Class.default()`（NoRecv）—— 接收者是类型名，没有值可求。
	if id, isID := field.X.(*parse.Ident); isID {
		if cl := c.recvClass(id.Name); cl != nil {
			if sig, has := cl.Method(field.Name); has && sig.NoRecv {
				args, err := c.argsFor(v, sig)
				if err != nil {
					return "", true, err
				}
				return fmt.Sprintf("%s(%s)", c.cMethodName(cl, sig.Name), args), true, nil
			}
		}
	}
	rt := c.ti(field.X)
	if rt == nil {
		// 接收者是包名 (str./math./os./option./testing.) 的形态已在 tryStdCall 处理;
		// 走到这里说明是别的东西, 报出接收者名字便于定位。
		if id, isId := field.X.(*parse.Ident); isId {
			return "", true, fmt.Errorf("emit: the receiver type of %s is unknown (%s.%s, line %d)",
				id.Name, id.Name, field.Name, v.Pos.Line)
		}
		return "", false, nil
	}
	// str.len() = 字节数 O(1) (§二.3)
	if types.IsStrType(rt) && field.Name == "len" {
		recv, err := c.expr(field.X, rt)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("((%s).len)", recv), true, nil
	}
	// str.isEmpty() = 字节数 == 0 (§十四)
	if types.IsStrType(rt) && field.Name == "isEmpty" {
		recv, err := c.expr(field.X, rt)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("((%s).len == 0)", recv), true, nil
	}
	// N7 `bytes` 实例方法（运行时 = aic_bytes_* + aic_list_u8 实例）。
	if types.IsBytes(rt) {
		return c.emitBytesMethod(v, field, rt)
	}
	// N3：标量上的 `compare`（Ord 约束授权的方法）。数值走内联三路比较，
	// str 走既有的字节序比较 —— 不引入新的运行时符号。
	if b, isBasic := rt.(*types.Basic); isBasic && field.Name == "compare" {
		switch b.Name {
		case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize", "f32", "f64", "bool":
			recv, err := c.expr(field.X, rt)
			if err != nil {
				return "", true, err
			}
			arg, err := c.arg(v, 0, rt)
			if err != nil {
				return "", true, err
			}
			return fmt.Sprintf("((%s) < (%s) ? -1 : ((%s) > (%s) ? 1 : 0))", recv, arg, recv, arg), true, nil
		case "str":
			recv, err := c.expr(field.X, rt)
			if err != nil {
				return "", true, err
			}
			arg, err := c.arg(v, 0, rt)
			if err != nil {
				return "", true, err
			}
			return fmt.Sprintf("aic_str_cmp(%s, %s)", recv, arg), true, nil
		}
	}
	if types.IsSlice(rt) || types.IsMap(rt) || types.IsSet(rt) || types.IsArray(rt) {
		e, err := c.containerMethod(v, field, rt)
		return e, true, err
	}
	if cl, isCl := types.IsClass(rt); isCl {
		sig, has := cl.Method(field.Name)
		if !has {
			return "", true, fmt.Errorf("emit: class %s has no method %s (line %d)", cl.Name, field.Name, v.Pos.Line)
		}
		recv, err := c.expr(field.X, rt)
		if err != nil {
			return "", true, err
		}
		args, err := c.argsFor(v, sig)
		if err != nil {
			return "", true, err
		}
		name := c.cMethodName(cl, sig.Name)
		if args == "" {
			return fmt.Sprintf("%s(%s)", name, recv), true, nil
		}
		return fmt.Sprintf("%s(%s, %s)", name, recv, args), true, nil
	}
	if ifc, isIfc := types.IsInterface(rt); isIfc {
		_ = ifc
		// 接口方法调用：见证表槽位分发（§十），nil interface 调用 = trap。
		return c.interfaceCall(v, field, rt, want)
	}
	// 泛型类实例的方法调用：符号名 = aic_<pkg>_<Cls>_<method>__<实参…>。
	if inst, isInst := rt.(*types.Instance); isInst {
		if cl, isCl := types.IsClass(inst.Base); isCl {
			sig, has := instMethodSig(cl, inst, field.Name)
			if !has {
				return "", true, fmt.Errorf("emit: class %s has no method %s (line %d)", cl.Name, field.Name, v.Pos.Line)
			}
			recv, err := c.expr(field.X, rt)
			if err != nil {
				return "", true, err
			}
			args, err := c.argsFor(v, sig)
			if err != nil {
				return "", true, err
			}
			name := c.instMethodCallName(inst, cl, sig.Name)
			if args == "" {
				return fmt.Sprintf("%s(%s)", name, recv), true, nil
			}
			return fmt.Sprintf("%s(%s, %s)", name, recv, args), true, nil
		}
	}
	return "", false, nil
}

// emitContainerNew 发射容器构造: 区域分配 + 对象头 (新容器是创建表达式, §五 R1)。
func (c *Ctx) emitContainerNew(t types.Type, line int) (string, error) {
	switch {
	case types.IsSet(t):
		suf, ok := c.containerSuffix(types.SetElem(t))
		if !ok {
			return "", fmt.Errorf("emit: set element type %s is not instantiated (line %d)", types.SetElem(t).String(), line)
		}
		return fmt.Sprintf("aic_set_new_%s(%du)", suf, uint32(line)), nil
	case types.IsMap(t):
		k, v, _ := types.MapParts(t)
		ks, kok := c.mapKeySuffix(k)
		vs, vok := c.mapValueSuffix(v)
		if !kok || !vok {
			return "", fmt.Errorf("emit: map[%s]%s is not instantiated (line %d)", k.String(), v.String(), line)
		}
		return fmt.Sprintf("aic_map_new_%s_%s(%du)", ks, vs, uint32(line)), nil
	case types.IsSlice(t):
		elem := types.SliceElem(t)
		suf, ok := c.containerSuffix(elem)
		if !ok {
			return "", fmt.Errorf("emit: list element type %s is not instantiated (line %d)", elem.String(), line)
		}
		return fmt.Sprintf("aic_list_new_%s(%du)", suf, uint32(line)), nil
	}
	return "", fmt.Errorf("emit: this type is not a container (line %d)", line)
}

// containerMethod 发射容器内建方法 (§二.2 的完整方法面)。
func (c *Ctx) containerMethod(v *parse.Call, field *parse.Field, rt types.Type) (string, error) {

	// 探测遍（F3 事实收集）：变更类方法可能改变长度/数据指针。
	if c.probe {
		switch field.Name {
		case "append", "set", "clear", "insert", "remove", "pop":
			if id, ok := field.X.(*parse.Ident); ok {
				c.markLenMutatedName(id.Name)
			}
		}
		// F4：append 计数（每轮一次 → 长度式 += 迭代次数）；其他变更破坏该推导
		if id, ok := field.X.(*parse.Ident); ok {
			if field.Name == "append" {
				c.noteAppend(id.Name)
			} else {
				c.noteOtherMutation(id.Name)
			}
		}
	}
	// 长度事实的作废：**收缩类**操作会让「len = 常量」失效（append/set/insert 只增长，
	// 事实仍然成立 → 不必作废）。
	switch field.Name {
	case "clear", "remove", "pop":
		if id, ok := field.X.(*parse.Ident); ok {
			c.invalidateLenFact(id.Name)
		}
	}
	recv, err := c.expr(field.X, rt)
	if err != nil {
		return "", err
	}
	line := cstr(c.Path)
	ln := v.Pos.Line

	switch t := rt.(type) {
	case *types.Slice:
		suf, ok := c.containerSuffix(types.SliceElem(t))
		if !ok {
			return "", fmt.Errorf("emit: list element type %s is not instantiated (line %d)", types.SliceElem(t).String(), ln)
		}
		elem := types.SliceElem(t)
		switch field.Name {
		case "len":
			return fmt.Sprintf("aic_list_len_%s(%s)", suf, recv), nil
		case "isEmpty":
			return fmt.Sprintf("aic_list_isempty_%s(%s)", suf, recv), nil
		case "append":
			a, err := c.arg(v, 0, elem)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_list_append_%s(%s, %s, %s, %d)", suf, recv, a, line, ln), nil
		case "insert":
			i, err := c.arg(v, 0, types.TUsize)
			if err != nil {
				return "", err
			}
			a, err := c.arg(v, 1, elem)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_list_insert_%s(%s, %s, %s, %s, %d)", suf, recv, i, a, line, ln), nil
		case "remove":
			i, err := c.arg(v, 0, types.TUsize)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_list_remove_%s(%s, %s, %s, %d)", suf, recv, i, line, ln), nil
		case "clear":
			return fmt.Sprintf("aic_list_clear_%s(%s, %s, %d)", suf, recv, line, ln), nil
		case "pop":
			// pop() -> (T, bool): 多返回 → 合成结构 (§二.2)
			return c.popCall(suf, recv, elem, ln)
		}
		return "", fmt.Errorf("emit: list has no method %s (line %d)", field.Name, ln)

	case *types.ArrayT:
		switch field.Name {
		case "len":
			return fmt.Sprintf("%du", t.N), nil
		case "isEmpty":
			return fmt.Sprintf("(%d == 0)", t.N), nil
		}
		return "", fmt.Errorf("emit: a fixed-size array has no method %s (line %d)", field.Name, ln)

	case *types.SetT:
		suf, ok := c.containerSuffix(types.SetElem(t))
		if !ok {
			return "", fmt.Errorf("emit: set element type %s is not instantiated (line %d)", types.SetElem(t).String(), ln)
		}
		elem := types.SetElem(t)
		switch field.Name {
		case "len":
			return fmt.Sprintf("aic_set_len_%s(%s)", suf, recv), nil
		case "isEmpty":
			return fmt.Sprintf("aic_set_isempty_%s(%s)", suf, recv), nil
		case "add":
			a, err := c.arg(v, 0, elem)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_set_add_%s(%s, %s, %s, %d)", suf, recv, a, line, ln), nil
		case "has":
			a, err := c.arg(v, 0, elem)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_set_has_%s(%s, %s)", suf, recv, a), nil
		case "remove":
			a, err := c.arg(v, 0, elem)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_set_remove_%s(%s, %s, %s, %d)", suf, recv, a, line, ln), nil
		case "at":
			i, err := c.arg(v, 0, types.TUsize)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_set_at_%s(%s, %s, %s, %d)", suf, recv, i, line, ln), nil
		case "clear":
			return fmt.Sprintf("aic_set_clear_%s(%s, %s, %d)", suf, recv, line, ln), nil
		}
		return "", fmt.Errorf("emit: set has no method %s (line %d)", field.Name, ln)

	case *types.MapT:
		ks, kok := c.mapKeySuffix(t.Key)
		vs, vok := c.mapValueSuffix(t.Value)
		if !kok || !vok {
			return "", fmt.Errorf("emit: map[%s]%s is not instantiated (line %d)", t.Key.String(), t.Value.String(), ln)
		}
		sym := ks + "_" + vs
		switch field.Name {
		case "len":
			return fmt.Sprintf("aic_map_len_%s(%s)", sym, recv), nil
		case "isEmpty":
			return fmt.Sprintf("aic_map_isempty_%s(%s)", sym, recv), nil
		case "put":
			k, err := c.arg(v, 0, t.Key)
			if err != nil {
				return "", err
			}
			val, err := c.arg(v, 1, t.Value)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_put_%s(%s, %s, %s, %s, %d)", sym, recv, k, val, line, ln), nil
		case "get":
			k, err := c.arg(v, 0, t.Key)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_get_%s(%s, %s)", sym, recv, k), nil
		case "has":
			k, err := c.arg(v, 0, t.Key)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_has_%s(%s, %s)", sym, recv, k), nil
		case "remove":
			k, err := c.arg(v, 0, t.Key)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_remove_%s(%s, %s, %s, %d)", sym, recv, k, line, ln), nil
		case "keyAt":
			i, err := c.arg(v, 0, types.TUsize)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_key_at_%s(%s, %s, %s, %d)", sym, recv, i, line, ln), nil
		case "valAt":
			i, err := c.arg(v, 0, types.TUsize)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("aic_map_val_at_%s(%s, %s, %s, %d)", sym, recv, i, line, ln), nil
		case "clear":
			return fmt.Sprintf("aic_map_clear_%s(%s, %s, %d)", sym, recv, line, ln), nil
		}
		return "", fmt.Errorf("emit: map has no method %s (line %d)", field.Name, ln)
	}
	return "", fmt.Errorf("emit: container method %s is not supported (line %d)", field.Name, ln)
}

// popCall 发射 pop() -> (T, bool): 运行时以出参给出 bool, 这里装进合成结构。
func (c *Ctx) popCall(suf, recv string, elem types.Type, ln int) (string, error) {
	tmp := c.tmp("pop")
	okv := c.tmp("ok")
	c.line("bool %s = false;", okv)
	c.line("%s %s = aic_list_pop_%s(%s, &%s);", c.cTypeName(elem), tmp, suf, recv, okv)
	res := c.tmp("popr")
	c.line("%s %s;", c.retStructNameFor([]types.Type{elem, types.TBool}), res)
	c.line("%s._0 = %s;", res, tmp)
	c.line("%s._1 = %s;", res, okv)
	return res, nil
}

// arg 取第 i 个实参的表达式 (按期望类型定宽)。
func (c *Ctx) arg(v *parse.Call, i int, want types.Type) (string, error) {
	if i >= len(v.Args) {
		return "", fmt.Errorf("emit: too few arguments (line %d)", v.Pos.Line)
	}
	return c.expr(v.Args[i], want)
}

// --- std 包 (§十四) -----------------------------------------------------------

// tryStdCall: 发射 import包.函数(...) 与 print.println。
func (c *Ctx) tryStdCall(v *parse.Call, want types.Type) (string, bool, error) {
	field, ok := v.Fn.(*parse.Field)
	if !ok {
		return "", false, nil
	}
	id, ok := field.X.(*parse.Ident)
	if !ok {
		return "", false, nil
	}
	if c.Info == nil || !c.Info.Imported[id.Name] {
		return "", false, nil
	}
	pkg, fn := id.Name, field.Name
	switch pkg {
	case "print":
		if fn != "println" {
			return "", true, fmt.Errorf("emit: the print surface has println only (line %d)", v.Pos.Line)
		}
		e, err := c.printCall(v)
		return e, true, err
	case "str":
		e, err := c.stdStrCall(v, fn)
		return e, true, err
	case "os":
		e, err := c.stdOsCall(v, fn)
		return e, true, err
	case "math":
		e, err := c.stdMathCall(v, fn)
		return e, true, err
	case "time":
		e, err := c.stdTimeCall(v, fn)
		return e, true, err
	case "option":
		e, err := c.stdOptionCall(v, fn)
		return e, true, err
	case "testing":
		e, err := c.stdTestingCall(v, fn)
		return e, true, err
	default:
		// 用户包（目录即包）：跨包调用 aic_<被调包>_<f>(…)
		return c.depCall(v, pkg, fn, want)
	}
}

// stdStrCall: str 包函数 (§十四)。
func (c *Ctx) stdStrCall(v *parse.Call, fn string) (string, error) {
	line := cstr(c.Path)
	ln := v.Pos.Line
	switch fn {
	case "concat":
		a, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		b, err := c.arg(v, 1, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_str_concat(%s, %s, %d)", a, b, ln), nil
	case "sub":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		i, err := c.arg(v, 1, types.TUsize)
		if err != nil {
			return "", err
		}
		j, err := c.arg(v, 2, types.TUsize)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_str_sub(%s, %s, %s, %s, %d)", s, i, j, line, ln), nil
	case "trim":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_str_trim(%s)", s), nil
	case "split":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		sep, err := c.arg(v, 1, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_split(%s, %s, %s, %d)", s, sep, line, ln), nil
	case "startsWith":
		return c.strPair(v, "aic_str_starts_with")
	case "endsWith":
		return c.strPair(v, "aic_str_ends_with")
	case "indexOf":
		// 语言级返回 i64，未命中 = -1（语料 682 冻结）
		return c.strPair(v, "aic_str_index_of_i64")
	case "fromI64":
		x, err := c.arg(v, 0, types.TI64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_from_i64(%s, %du)", x, uint32(ln)), nil
	case "fromU64":
		x, err := c.arg(v, 0, types.TU64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_from_u64(%s, %du)", x, uint32(ln)), nil
	case "fromBool":
		x, err := c.arg(v, 0, types.TBool)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_from_bool(%s)", x), nil
	case "fromF64":
		x, err := c.arg(v, 0, types.TF64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_from_f64(%s, %du)", x, uint32(ln)), nil
	case "toI64":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_to_i64(%s, %s, %d)", s, line, ln), nil
	case "toF64":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_to_f64(%s, %s, %d)", s, line, ln), nil
	case "utf8At":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		i, err := c.arg(v, 1, types.TUsize)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_utf8_at(%s, %s, %s, %d)", s, i, line, ln), nil
	case "codepoints":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_str_codepoints(%s, %s, %d)", s, line, ln), nil
	}
	return "", fmt.Errorf("emit: no such function in the str package: %s (line %d)", fn, ln)
}

// strPair 发射 (str, str) → 结果的 str 包函数 (内联助手)。
func (c *Ctx) strPair(v *parse.Call, helper string) (string, error) {
	a, err := c.arg(v, 0, types.TStr)
	if err != nil {
		return "", err
	}
	b, err := c.arg(v, 1, types.TStr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s(%s, %s)", helper, a, b), nil
}

// stdOsCall: os 包 (§十四: args/readFile/readStdin/exit)。
func (c *Ctx) stdOsCall(v *parse.Call, fn string) (string, error) {
	line := cstr(c.Path)
	ln := v.Pos.Line
	switch fn {
	case "args":
		return fmt.Sprintf("aic_std_os_args(%s, %d)", line, ln), nil
	case "readFile":
		p, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_std_os_read_file(%s, %s, %d)", p, line, ln), nil
	case "readStdin":
		return fmt.Sprintf("aic_std_os_read_stdin(%s, %d)", line, ln), nil
	case "exit":
		code, err := c.arg(v, 0, types.TI32)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_os_exit(%s)", code), nil
	}
	return "", fmt.Errorf("emit: no such function in the os package: %s (line %d)", fn, ln)
}

// stdTimeCall 发射 time 面（§15.3 / N6）。
//
//	time.now()       → aic_std_time_now()        （墙钟毫秒）
//	time.monotonic() → aic_std_time_monotonic()  （单调毫秒）
//	time.sleep(ms)   → aic_std_time_sleep(ms)
//	time.after(ms)   → aic_time_deadline(ms)     （L2：截止点 = 单调 + ms）
func (c *Ctx) stdTimeCall(v *parse.Call, fn string) (string, error) {
	switch fn {
	case "now":
		if len(v.Args) != 0 {
			return "", fmt.Errorf("emit: time.now takes no arguments (line %d)", v.Pos.Line)
		}
		c.need("std")
		return "aic_std_time_now()", nil
	case "monotonic":
		if len(v.Args) != 0 {
			return "", fmt.Errorf("emit: time.monotonic takes no arguments (line %d)", v.Pos.Line)
		}
		c.need("std")
		return "aic_std_time_monotonic()", nil
	case "sleep":
		ms, err := c.arg(v, 0, types.TI64)
		if err != nil {
			return "", err
		}
		c.need("std")
		return fmt.Sprintf("aic_std_time_sleep(%s)", ms), nil
	case "after":
		ms, err := c.arg(v, 0, types.TI64)
		if err != nil {
			return "", err
		}
		c.need("l2")
		return fmt.Sprintf("aic_time_deadline(%s)", ms), nil
	}
	return "", fmt.Errorf("emit: no such function in the time package: %s (line %d)", fn, v.Pos.Line)
}

// stdMathCall: math 包 (§十四: abs/min/max/floor/ceil/sqrt/pow)。
func (c *Ctx) stdMathCall(v *parse.Call, fn string) (string, error) {
	a, err := c.arg(v, 0, types.TF64)
	if err != nil {
		return "", err
	}
	switch fn {
	case "abs":
		return fmt.Sprintf("aic_math_abs_f64(%s)", a), nil
	case "floor":
		return fmt.Sprintf("aic_math_floor_f64(%s)", a), nil
	case "ceil":
		return fmt.Sprintf("aic_math_ceil_f64(%s)", a), nil
	case "sqrt":
		return fmt.Sprintf("aic_math_sqrt_f64(%s)", a), nil
	case "min", "max", "pow":
		b, err := c.arg(v, 1, types.TF64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("aic_math_%s_f64(%s, %s)", fn, a, b), nil
	}
	return "", fmt.Errorf("emit: no such function in the math package: %s (line %d)", fn, v.Pos.Line)
}

// stdOptionCall 见 instance.go（随泛型实例一起落地）。

// stdTestingCall: testing 包 (eq/gt/lt 失败即 trap, §十一)。
//
// 按**实参类型**选原语（与打印同一条定型分派纪律）：str 走字节比较、
// f32/f64 走浮点比较（IEEE，NaN 一律失败）、整型/bool 走 i64 比较。
// 少了这一路，testing.eq("a","b") 会发出 eq_i64(str, str) = C 层类型错。
func (c *Ctx) stdTestingCall(v *parse.Call, fn string) (string, error) {
	if fn != "eq" && fn != "gt" && fn != "lt" {
		return "", fmt.Errorf("emit: no such function in the testing package: %s (line %d)", fn, v.Pos.Line)
	}
	t := c.printExprOf(v.Args[0])
	kind := "i64"
	switch {
	case t != nil && types.IsStrType(t):
		kind = "str"
	case t != nil && (types.IsNumeric(t)):
		if b, ok := t.(*types.Basic); ok && (b.Name == "f32" || b.Name == "f64") {
			kind = "f64"
		}
	}
	var want types.Type = types.TI64
	if kind == "str" {
		want = types.TStr
	} else if kind == "f64" {
		want = types.TF64
	}
	a, err := c.arg(v, 0, want)
	if err != nil {
		return "", err
	}
	b, err := c.arg(v, 1, want)
	if err != nil {
		return "", err
	}
	name := "aic_std_test_" + fn + "_" + kind
	if fn == "eq" && kind == "i64" {
		name = "aic_std_test_eq_i64"
	}
	return fmt.Sprintf("%s(%s, %s, %s, %s, %d)",
		name, a, b, c.strLit("testing."+fn), cstr(c.Path), v.Pos.Line), nil
}
