package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 名字 → 语义类型 (pass 1)。
//
// 为什么需要: 检查器的类型表覆盖「有类型的表达式」，但变量读（裸名）在
// checkIdent 里直接返回符号类型、不入表 —— 于是 `s.len()` 这种「接收者是裸名」
// 的调用在 emit 侧拿不到接收者类型。修法不是改检查器（那是 R2 的产物），
// 而是 emit 自己在开始发射前把每个函数体的声明扫一遍登记名字类型：
// 局部/参数/字段/const 各只有一处来源，扫一遍即可，不重做作用域分析
// （遮蔽由检查器保证，同名取「最后声明」即可）。
// ---------------------------------------------------------------------------

// nameTypes 是 (函数标识 → 名字 → 类型) 的表。
type nameTypes map[string]map[string]types.Type

func (c *Ctx) buildNameTypes() nameTypes {
	out := nameTypes{}
	for _, d := range c.File.Decls {
		switch v := d.(type) {
		case *parse.FuncDecl:
			key := types.FuncKey("", v.Name)
			m := out[key]
			if m == nil {
				m = map[string]types.Type{}
				out[key] = m
			}
			c.collectParams(m, v)
			c.collectBlockTypes(m, v.Body)
		case *parse.ClassDecl:
			for _, mth := range v.Methods {
				key := types.FuncKey(v.Name, mth.Name)
				m := out[key]
				if m == nil {
					m = map[string]types.Type{}
					out[key] = m
				}
				c.collectParams(m, mth)
				c.collectBlockTypes(m, mth.Body)
			}
		}
	}
	return out
}

// collectParams 登记形参类型 (this 由调用点按接收者给出)。
func (c *Ctx) collectParams(m map[string]types.Type, v *parse.FuncDecl) {
	if v.Recv != "" {
		// 方法的 this: 类型 = 接收者类 (由类名查具名类型表)
		if c.Info != nil {
			if cl, ok := c.Info.Classes[v.Recv]; ok {
				m["this"] = cl
			}
		}
	}
	for _, p := range v.Params {
		if t := c.typeOfTypeExpr(p.Type); t != nil {
			m[p.Name] = t
		}
	}
}

// collectBlockTypes 递归登记块内的 var 声明类型。
func (c *Ctx) collectBlockTypes(m map[string]types.Type, b *parse.Block) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		switch v := s.(type) {
		case *parse.VarDecl:
			// 多目标 = 多返回绑定（var a, err = f()）：逐位取被调返回类型
			if len(v.Targets) > 1 {
				elems := types.MultiElems(c.ti(v.Init))
				for i, tgt := range v.Targets {
					if tgt.Blank || i >= len(elems) || elems[i] == nil {
						continue
					}
					m[tgt.Name] = elems[i]
				}
				continue
			}
			t := c.declType(v)
			for _, tgt := range v.Targets {
				if !tgt.Blank && t != nil {
					m[tgt.Name] = t
				}
			}
		case *parse.ExprStmt:
			// a, err = f()：名字已存在，这里只是补充 emit 侧的类型知识
			ma, ok := v.X.(*parse.MultiAssign)
			if !ok {
				continue
			}
			elems := types.MultiElems(c.ti(ma.Right))
			for i, lhs := range ma.LHS {
				id, isId := lhs.(*parse.Ident)
				if !isId || id.Name == "_" || i >= len(elems) || elems[i] == nil {
					continue
				}
				m[id.Name] = elems[i]
			}
		case *parse.IfStmt:
			c.collectBlockTypes(m, v.Then)
			c.collectBlockTypes(m, v.Else)
		case *parse.ForStmt:
			if v.Init != nil {
				if vd, ok := v.Init.(*parse.VarDecl); ok {
					t := c.declType(vd)
					for _, tgt := range vd.Targets {
						if !tgt.Blank && t != nil {
							m[tgt.Name] = t
						}
					}
				}
			}
			// for-in 绑定名类型：走 types.RangeBindTypes（唯一实现，与检查器同源）
			iterTy := c.ti(v.RangeX)
			isInterval := v.RangeEnd != nil
			first, second := types.RangeBindTypes(iterTy, len(v.Names), isInterval)
			if len(v.Names) >= 1 && !v.Names[0].Blank && first != nil {
				m[v.Names[0].Name] = first
			}
			if len(v.Names) >= 2 && !v.Names[1].Blank && second != nil {
				m[v.Names[1].Name] = second
			}
			c.collectBlockTypes(m, v.Body)
		case *parse.MatchStmt:
			c.collectArms(m, v.Arms)
		case *parse.RegionStmt:
			c.collectBlockTypes(m, v.Body)
		case *parse.ScopeStmt:
			c.collectBlockTypes(m, v.Body)
		case *parse.Block:
			c.collectBlockTypes(m, v)
		case *parse.DeferStmt:
			c.collectBlockTypes(m, v.Block)
		}
	}
}

// collectArms 登记 match 分支的模式绑定类型。
func (c *Ctx) collectArms(m map[string]types.Type, arms []parse.MatchArm) {
	for _, arm := range arms {
		for _, p := range arm.Patterns {
			if p.Kind != "payload" || p.Binding == "" || p.Binding == "_" {
				continue
			}
			if en, ok := c.enumByName(p.Name); ok {
				if pt, has := en.VariantPayload(p.Name); has && pt != nil {
					m[p.Binding] = pt
				}
			}
		}
		c.collectBlockTypes(m, arm.Block)
	}
}

// enumByName 按变体名找所属 enum。
func (c *Ctx) enumByName(variant string) (*types.Enum, bool) {
	if c.Info == nil {
		return nil, false
	}
	for _, en := range c.Info.Enums {
		if _, ok := en.VariantPayload(variant); ok {
			return en, true
		}
	}
	return nil, false
}

// ti 取表达式节点的类型: 先查检查器的类型表, 再查 pass 1 的名字表, 最后按
// 调用形状推返回类型 (「调用 = 调用点偏移 + 被调返回摘要」的另一半: 形状)。
func (c *Ctx) ti(e parse.Expr) types.Type {
	if e == nil {
		return nil
	}
	if c.Info != nil {
		if t := c.Info.LookupType(e); t != nil {
			return c.substT(t)
		}
		if id, ok := e.(*parse.Ident); ok {
			return c.substT(c.nameType(id.Name))
		}
		if call, ok := e.(*parse.Call); ok {
			return c.substT(c.callResultType(call))
		}
		// s[i..j] = str 视图（§二.3）：视图永远是 str，与元素类型无关
		if ix, ok := e.(*parse.Index); ok {
			if ix.End != nil {
				return types.TStr
			}
			// 泛型实例化 Box[i32]：类型表里有类型，这里只在表缺项时兜底（返回 nil
			// 会让调用点当成下标读，故显式判掉）。
			if _, isGeneric := c.genericHead(ix); isGeneric {
				return nil
			}
			// 读下标：元素类型（str → u8）——println(s[i]) 这类位置要用
			xt := c.ti(ix.X)
			switch {
			case types.IsStrType(xt):
				return types.TU8
			case types.IsSlice(xt):
				return types.SliceElem(xt)
			case types.IsArray(xt):
				elem, _, _ := types.ArrayElem(xt)
				return elem
			case types.IsMap(xt):
				_, v, _ := types.MapParts(xt)
				return v
			}
		}
		// 限定枚举值 Color.Red：类型 = 该枚举（match 主体与实参位都要用）
		if f, ok := e.(*parse.Field); ok {
			if id, isID := f.X.(*parse.Ident); isID {
				if en, has := c.Info.Enums[id.Name]; has {
					if _, isVariant := en.VariantIndex(f.Name); isVariant {
						return en
					}
				}
			}
		}
	}
	// 未定型字面量（含一元负号）在类型表里本就是 nil：按 §一 最窄可容纳定宽。
	// 少了这一路，`println("")` / `"a" == "b"` 这类全字面量表达式就发不出去。
	return c.literalType(e)
}

// callResultType 按被调签名推调用结果类型 (emit 侧需要它来定宽与选打印原语)。
func (c *Ctx) callResultType(call *parse.Call) types.Type {
	if c.Info == nil {
		return nil
	}
	// std option 的 Some(x)：结果类型 = Option[T]，T 由实参决定（检查器同规则）。
	// 少了这一路，`match Some(v) { … }` 的主体在 emit 侧无类型可用。
	if id, ok := call.Fn.(*parse.Ident); ok && id.Name == "Some" && c.Info.Imported["option"] {
		if inst, isOpt := c.someInstanceShallow(call, nil); isOpt {
			return inst
		}
	}
	// std 包函数 str.f(…) / os.f(…) / math.f(…) / testing.f(…)：
	// 少了这一路，`var parts = str.split(…)`、`var v, e = str.toI64(…)`、
	// `println("abc".len())` 在 emit 侧都拿不到结果类型。
	if field, ok := call.Fn.(*parse.Field); ok {
		if id, isID := field.X.(*parse.Ident); isID && c.Info.Imported[id.Name] {
			switch id.Name {
			case "print":
				return nil // println 无返回
			case "option":
				// ok → Option[T]（T 由实参定）；none/isSome/unwrap 的结果类型
				// 由检查器记在表达式类型表里，这里不重复推断。
				if field.Name == "ok" {
					if inst, isOpt := c.someInstanceShallow(call, nil); isOpt {
						return inst
					}
				}
				return nil
			default:
				if _, results, ok := types.StdSig(id.Name, field.Name); ok {
					return resultsType(results)
				}
				// 用户包函数：结果类型取被调包的签名（跨包调用定宽要用）。
				if results, ok := c.depFuncResults(id.Name, field.Name); ok {
					return resultsType(results)
				}
			}
		}
	}
	// 自由函数 f(...)
	if id, ok := call.Fn.(*parse.Ident); ok {
		if sig, has := c.Info.Funcs[id.Name]; has {
			// 泛型函数：结果类型按**实例化后**的签名（T → 实参类型）。
			if len(sig.TypeParams) > 0 {
				argTypes := make([]types.Type, 0, len(call.Args))
				for _, a := range call.Args {
					argTypes = append(argTypes, c.ti(a))
				}
				if _, inst, ok := c.genericCallName(c.pkg(), sig, argTypes); ok {
					return resultsType(inst.Results)
				}
				return nil
			}
			return resultsType(sig.Results)
		}
		// 变体构造 Some(x) / Variant(x)
		if en, payload, isVar := c.lookupVariant(id.Name); isVar {
			if payload == nil {
				return en
			}
			return en
		}
		// 具名类型构造 C(args)（有 init 的类）
		if cl := c.recvClass(id.Name); cl != nil {
			if init, has := c.initMethod(cl); has {
				if len(init.Results) > 0 {
					return resultsType(append([]types.Type{cl}, init.Results...))
				}
			}
			return cl
		}
		if id.Name == "Err" {
			return types.TErr
		}
	}
	// 方法调用 recv.m(...)：类方法 / str / 容器 / 接口
	if field, ok := call.Fn.(*parse.Field); ok {
		if t := c.methodResultType(c.ti(field.X), field.Name); t != nil {
			return t
		}
	}
	return nil
}

// methodResultType 是「接收者类型 + 方法名 → 结果类型」的唯一实现
// （类方法 / str / list / set / map / [T;N] / interface）。
func (c *Ctx) methodResultType(rt types.Type, name string) types.Type {
	if rt == nil {
		return nil
	}
	if cl, isCl := types.IsClass(rt); isCl {
		if sig, has := cl.Method(name); has {
			return resultsType(sig.Results)
		}
		return nil
	}
	if ifc, isIfc := types.IsInterface(rt); isIfc {
		if sig, has := ifc.Method(name); has {
			return resultsType(sig.Results)
		}
		return nil
	}
	if types.IsStrType(rt) {
		switch name {
		case "len":
			return types.TUsize
		case "isEmpty":
			return types.TBool
		}
		return nil
	}
	if types.IsArray(rt) {
		switch name {
		case "len":
			return types.TUsize
		case "isEmpty":
			return types.TBool
		}
		return nil
	}
	if types.IsSlice(rt) {
		switch name {
		case "len":
			return types.TUsize
		case "isEmpty":
			return types.TBool
		case "pop":
			return &types.MultiType{Elems: []types.Type{types.SliceElem(rt), types.TBool}}
		}
		return nil
	}
	if types.IsSet(rt) {
		switch name {
		case "len":
			return types.TUsize
		case "isEmpty", "has":
			return types.TBool
		case "at":
			return types.SetElem(rt)
		}
		return nil
	}
	if types.IsMap(rt) {
		k, v, _ := types.MapParts(rt)
		switch name {
		case "len":
			return types.TUsize
		case "isEmpty", "has":
			return types.TBool
		case "get", "valAt":
			return v
		case "keyAt":
			return k
		}
		return nil
	}
	return nil
}

// resultsType 把返回位列表折成类型: 0 = nil, 1 = 该类型, ≥2 = 多返回形状。
func resultsType(results []types.Type) types.Type {
	switch len(results) {
	case 0:
		return nil
	case 1:
		return results[0]
	default:
		return &types.MultiType{Elems: results}
	}
}

// containerCtor 识别 set.new() / map.new() 这类容器构造（类型来自声明处, §二.2）。
func (c *Ctx) containerCtor(call *parse.Call, want types.Type) (types.Type, bool) {
	field, ok := call.Fn.(*parse.Field)
	if !ok || field.Name != "new" {
		return nil, false
	}
	id, ok := field.X.(*parse.Ident)
	if !ok {
		return nil, false
	}
	switch id.Name {
	case "set":
		if types.IsSet(want) {
			return want, true
		}
	case "map":
		if types.IsMap(want) {
			return want, true
		}
	}
	return nil, false
}

// nameType 查名字的语义类型 (局部/参数/const/包级类型)。
func (c *Ctx) nameType(name string) types.Type {
	if c.names != nil {
		if m, ok := c.names[c.curFuncName]; ok {
			if t, has := m[name]; has {
				return t
			}
		}
	}
	if c.Info != nil {
		if sym, ok := c.Info.Consts[name]; ok {
			return sym.Type
		}
		if cl, ok := c.Info.Classes[name]; ok {
			return cl
		}
		if en, ok := c.Info.Enums[name]; ok {
			return en
		}
		if ifc, ok := c.Info.Interfaces[name]; ok {
			return ifc
		}
	}
	return nil
}
