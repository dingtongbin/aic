package emit

import (
	"fmt"
	"sort"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// @derive(Compare) / @derive(Hash) 的方法体合成（核心设计 §四 三件套）。
//
// 派生方法就是普通方法：签名由检查器装配（types.deriveMethods），名字走同一张
// mangling 表（aic_<pkg>_<C>_compare / _hash），调用点与显式方法无差别。
// 方法体在这里按**字段声明序**合成 —— 规则只有一处（检查器管签名、emit 管体）。
//
// 确定性：字段序 = 声明序，比较结果与哈希值跨配置一致（H3）；类的遍历按名字排序
// （Go 的 map 序随机，直接遍历会破坏 H2 逐字节一致）。
// ---------------------------------------------------------------------------

// emitDerived 发射全部派生方法（原型 + 定义），在原型段之前。
func (c *Ctx) emitDerived() error {
	if c.Info == nil {
		return nil
	}
	names := make([]string, 0, len(c.Info.Classes))
	for name, cl := range c.Info.Classes {
		if cl.HasDerive("Compare") || cl.HasDerive("Hash") || cl.HasDerive("Default") || cl.HasDerive("Clone") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	emitted := false
	for _, name := range names {
		cl := c.Info.Classes[name]
		if cl.HasDerive("Compare") {
			if _, ok := cl.Method("compare"); ok {
				emitted = true
				c.line("static aic_i32 %s(%s, %s);", c.cMethodName(cl, "compare"),
					c.cTypeDecl(cl, "this__"), c.cTypeDecl(cl, "other"))
			}
		}
		if cl.HasDerive("Hash") {
			if _, ok := cl.Method("hash"); ok {
				emitted = true
				c.line("static aic_u64 %s(%s);", c.cMethodName(cl, "hash"), c.cTypeDecl(cl, "this__"))
			}
		}
		// N4：@derive(Default) / @derive(Clone) —— default() 返回全零实例，
		// clone() 返回按值拷贝（引用字段在检查器已被拒，故这里是纯值拷贝）。
		if cl.HasDerive("Default") {
			if _, ok := cl.Method("default"); ok {
				emitted = true
				c.line("static %s %s(void);", c.cTypeName(cl), c.cMethodName(cl, "default"))
			}
		}
		if cl.HasDerive("Clone") {
			if _, ok := cl.Method("clone"); ok {
				emitted = true
				c.line("static %s %s(%s);", c.cTypeName(cl), c.cMethodName(cl, "clone"), c.cTypeDecl(cl, "this__"))
			}
		}
	}
	if !emitted {
		return nil
	}
	c.line("")
	for _, name := range names {
		cl := c.Info.Classes[name]
		if _, ok := cl.Method("compare"); ok && cl.HasDerive("Compare") {
			if err := c.emitCompareBody(cl); err != nil {
				return err
			}
		}
		if _, ok := cl.Method("hash"); ok && cl.HasDerive("Hash") {
			if err := c.emitHashBody(cl); err != nil {
				return err
			}
		}
		if _, ok := cl.Method("default"); ok && cl.HasDerive("Default") {
			if err := c.emitDefaultBody(cl); err != nil {
				return err
			}
		}
		if _, ok := cl.Method("clone"); ok && cl.HasDerive("Clone") {
			if err := c.emitCloneBody(cl); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitDefaultBody：`static T aic_pkg_C_default(void) { return (T){0}; }`
// （值类型）或分配一个全零对象（引用类型）。
func (c *Ctx) emitDefaultBody(cl *types.Class) error {
	ct := c.cTypeName(cl)
	if cl.Packed {
		c.line("static %s %s(void) { return (%s){0}; }", ct, c.cMethodName(cl, "default"), ct)
		return nil
	}
	// 引用类：区域分配 + 逐字段零值（**不能 memset**：对象头刚由 alloc 写好）
	c.line("static %s %s(void) {", ct, c.cMethodName(cl, "default"))
	c.line("    %s obj = (%s)aic_alloc_hdr(sizeof(%s), 0);", ct, ct, ct)
	for i := range cl.Fields {
		c.line("    obj->%s = %s;", cl.Fields[i].Name, c.zeroValue(cl.Fields[i].Type))
	}
	c.line("    return obj;")
	c.line("}")
	return nil
}

// emitCloneBody：按值拷贝（@packed 直接整结构赋值；引用类分配 + 逐字段拷贝）。
func (c *Ctx) emitCloneBody(cl *types.Class) error {
	ct := c.cTypeName(cl)
	if cl.Packed {
		c.line("static %s %s(%s) { return this__; }", ct, c.cMethodName(cl, "clone"), c.cTypeDecl(cl, "this__"))
		return nil
	}
	c.line("static %s %s(%s) {", ct, c.cMethodName(cl, "clone"), c.cTypeDecl(cl, "this__"))
	c.line("    %s obj = (%s)aic_alloc_hdr(sizeof(%s), 0);", ct, ct, ct)
	for i := range cl.Fields {
		c.line("    obj->%s = this__->%s;", cl.Fields[i].Name, cl.Fields[i].Name)
	}
	c.line("    return obj;")
	c.line("}")
	return nil
}

// fieldAccess 是派生方法体内的字段访问（@packed = 值，用 `.`；引用对象用 `->`）。
func (c *Ctx) fieldAccess(cl *types.Class, base, field string) string {
	if cl.Packed {
		return base + "." + field
	}
	return base + "->" + field
}

// emitCompareBody 发射 compare(other) -> i32（-1 / 0 / 1，按字段声明序短路）。
func (c *Ctx) emitCompareBody(cl *types.Class) error {
	c.line("static aic_i32 %s(%s, %s) {", c.cMethodName(cl, "compare"),
		c.cTypeDecl(cl, "this__"), c.cTypeDecl(cl, "other"))
	for i := range cl.Fields {
		fd := cl.Fields[i]
		lhs := c.fieldAccess(cl, "this__", fd.Name)
		rhs := c.fieldAccess(cl, "other", fd.Name)
		cmp, err := c.compareExpr(fd.Type, lhs, rhs)
		if err != nil {
			return err
		}
		c.line("    if (%s < 0) return -1;", cmp)
		c.line("    if (%s > 0) return 1;", cmp)
	}
	c.line("    return 0;")
	c.line("}")
	c.line("")
	return nil
}

// compareExpr 生成「比较两字段」的 C 表达式（结果 <0 / 0 / >0）。
func (c *Ctx) compareExpr(t types.Type, lhs, rhs string) (string, error) {
	if b, ok := t.(*types.Basic); ok {
		switch {
		case types.IsStrType(b):
			return fmt.Sprintf("aic_str_cmp(%s, %s)", lhs, rhs), nil
		case types.IsBoolType(b):
			// bool 无序：false < true（与 tag 比较同构）
			return fmt.Sprintf("(aic_i32)%s - (aic_i32)%s", lhs, rhs), nil
		case types.IsNumeric(b):
			// 浮点 NaN 会让两个 if 都不成立 → 落到 return 0，与 == 语义一致（§三）
			return fmt.Sprintf("((%s > %s) ? 1 : ((%s < %s) ? -1 : 0))", lhs, rhs, lhs, rhs), nil
		}
		return "", fmt.Errorf("emit: field type %s is not comparable (the checker should have rejected it)", b.Name)
	}
	if cl, ok := types.IsClass(t); ok {
		return fmt.Sprintf("%s(%s, %s)", c.cMethodName(cl, "compare"), lhs, rhs), nil
	}
	if en, ok := types.IsEnum(t); ok {
		if en.HasData {
			return "", fmt.Errorf("emit: enum %s with payloads is not comparable (the checker should have rejected it)", en.Name)
		}
		return fmt.Sprintf("(aic_i32)%s - (aic_i32)%s", lhs, rhs), nil
	}
	if elem, n, ok := types.ArrayElem(t); ok {
		// 定长数组按元素序比较（逐位短路，与 str 的字典序同构）
		c.line("    {")
		c.line("        aic_i32 _c = 0;")
		c.line("        int _i;")
		c.line("        for (_i = 0; _i < %d; _i++) {", n)
		inner, err := c.compareExpr(elem, fmt.Sprintf("%s[_i]", lhs), fmt.Sprintf("%s[_i]", rhs))
		if err != nil {
			return "", err
		}
		c.line("            _c = %s;", inner)
		c.line("            if (_c != 0) break;")
		c.line("        }")
		c.line("        if (_c < 0) return -1;")
		c.line("        if (_c > 0) return 1;")
		c.line("    }")
		return "0", nil // 数组比较已就地发射并短路，外层再比 0 即无操作
	}
	return "", fmt.Errorf("emit: field type %s is not comparable", t.String())
}

// emitHashBody 发射 hash() -> u64（FNV-1a 按字段序折叠，跨配置确定）。
func (c *Ctx) emitHashBody(cl *types.Class) error {
	c.line("static aic_u64 %s(%s) {", c.cMethodName(cl, "hash"), c.cTypeDecl(cl, "this__"))
	c.line("    aic_u64 _h = AIC_HASH_SEED;")
	for i := range cl.Fields {
		fd := cl.Fields[i]
		v := c.fieldAccess(cl, "this__", fd.Name)
		expr, err := c.hashExpr(fd.Type, "_h", v)
		if err != nil {
			return err
		}
		c.line("    _h = %s;", expr)
	}
	c.line("    return _h;")
	c.line("}")
	c.line("")
	return nil
}

// hashExpr 生成「把字段折叠进哈希」的 C 表达式。
func (c *Ctx) hashExpr(t types.Type, h, v string) (string, error) {
	if b, ok := t.(*types.Basic); ok {
		switch {
		case types.IsStrType(b):
			return fmt.Sprintf("aic_hash_str(%s, %s)", h, v), nil
		case types.IsBoolType(b):
			return fmt.Sprintf("aic_hash_i64(%s, (aic_i64)(%s ? 1 : 0))", h, v), nil
		case b.Name == "f32" || b.Name == "f64":
			return fmt.Sprintf("aic_hash_f64(%s, (aic_f64)%s)", h, v), nil
		case b.Name == "u64" || b.Name == "usize":
			return fmt.Sprintf("aic_hash_u64(%s, (aic_u64)%s)", h, v), nil
		case types.IsNumeric(b):
			return fmt.Sprintf("aic_hash_i64(%s, (aic_i64)%s)", h, v), nil
		}
		return "", fmt.Errorf("emit: field type %s is not hashable (the checker should have rejected it)", b.Name)
	}
	if cl, ok := types.IsClass(t); ok {
		return fmt.Sprintf("aic_hash_mix(%s, %s(%s))", h, c.cMethodName(cl, "hash"), v), nil
	}
	if en, ok := types.IsEnum(t); ok {
		if en.HasData {
			return "", fmt.Errorf("emit: enum %s with payloads is not hashable (the checker should have rejected it)", en.Name)
		}
		return fmt.Sprintf("aic_hash_i64(%s, (aic_i64)%s)", h, v), nil
	}
	if elem, n, ok := types.ArrayElem(t); ok {
		// 定长数组：按元素序折叠（元素表达式复用同一实现）
		c.line("    {")
		c.line("        int _i;")
		c.line("        for (_i = 0; _i < %d; _i++) {", n)
		inner, err := c.hashExpr(elem, "_h", fmt.Sprintf("%s[_i]", v))
		if err != nil {
			return "", err
		}
		c.line("            _h = %s;", inner)
		c.line("        }")
		c.line("    }")
		return "_h", nil
	}
	return "", fmt.Errorf("emit: field type %s is not hashable", t.String())
}
