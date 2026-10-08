package emit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aic/internal/air"
	"aic/internal/cir"
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// IR → C 后端（L5d/L5f）：直译路径（AST → C）退役后的唯一后端。
//
// 为什么放在 emit 包里：类型→C 名（ctype.go）、按需合成的打印器（printgen.go）、
// 见证表与 trampoline（iface.go）、@derive 家族（derive.go）、返回结构体、入口
// wrapper —— 这些**都是类型驱动的**，与"体从哪来"无关。体改从 IR 取，就只换掉
// 走树那一段（stmt/expr/call/defer/lambda），其余一律复用：同一条规则只有一处实现
// （红线 10），否则两条路径会在某个语料上悄悄分叉。
//
// IR 侧已经把所有难事做完了：求值顺序、临时量、去糖、单态化、守卫位置。后端只剩
// 「IR 指令 → C 语句」的直译，每个未覆盖的形态**返回错误**，绝不发射半截（红线 23）。
//
// 与直译路径的关系（L5e 的双跑等价）：
//   - 只要求行为等价（stdout 与退出码逐位一致），不要求 C 文本一致；
//   - 但两侧共用运行期 ABI、守卫宏、打印原语 —— 那些是 §runtime 已定案的东西。
// ---------------------------------------------------------------------------

// IRProg 是 IR 路径交给后端的材料（每个包一份 cir 程序，按拓扑序）。
type IRProg struct {
	// Bodies 是 air 符号（air.FuncSym / air.FuncInstSym）→ 结构化函数体。
	Bodies map[string]*cir.Func
	// used 记录已被发射消费的符号（收口时校验：不许有没人要的函数体 ——
	// 那说明签名侧与 IR 侧的符号对不上，是本类改动最容易犯的错）。
	used map[string]bool
}

// NewIRProg 建一个空的 IR 材料表。
func NewIRProg() *IRProg {
	return &IRProg{Bodies: map[string]*cir.Func{}, used: map[string]bool{}}
}

// Add 登记一个函数体（跨包同符号覆盖 = 编译器的错，直接报）。
func (p *IRProg) Add(sym string, f *cir.Func) error {
	if _, dup := p.Bodies[sym]; dup {
		return fmt.Errorf("emit(IR): duplicate IR body for %s", sym)
	}
	p.Bodies[sym] = f
	return nil
}

// resetUsed 清空消费记录（两遍发射的各一遍开头调用：used 是"本遍"的记账，
// 跨遍残留会把第二遍要发的函数体误判成已消费）。
func (p *IRProg) resetUsed() { p.used = map[string]bool{} }

// verifyAllUsed 报告有没有没被任何签名消费的函数体（符号对不上 = 编译器 bug）。
func (p *IRProg) verifyAllUsed() error {	var orphans []string
	for sym := range p.Bodies {
		if !p.used[sym] {
			orphans = append(orphans, sym)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)
	return fmt.Errorf("emit(IR): %d IR bodies were never consumed (symbol mismatch): %s",
		len(orphans), strings.Join(orphans, ", "))
}

// EmitIRUnit 是 IR 路径的整程序入口（与 EmitUnit 同纪律：两遍、纯函数）。
func EmitIRUnit(u *Unit, ir *IRProg) (*Result, error) {
	if u == nil || len(u.Files) == 0 {
		return nil, fmt.Errorf("emit: empty emission unit")
	}
	if ir == nil {
		return nil, fmt.Errorf("emit: the IR path needs IR bodies")
	}
	_, pre, err := runEmit(u, nil, ir)
	if err != nil {
		return nil, err
	}
	res, _, err := runEmit(u, pre, ir)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 类型文本 → C 类型
// ---------------------------------------------------------------------------

// buildIRTypeTab 从检查产物重建「AIR 类型文本 → 语义类型」表。
//
// AIR 里类型是**文本**（`.air` 是投影），而 C 类型名只由 types 侧那张表给出。
// 重建的主键用 air.TyText（文本形态的唯一定义在 air 侧），值取检查器里的语义类型
// 对象 —— 于是后端既不解析类型文本，也不再写一份 mangling。
func (c *Ctx) buildIRTypeTab() {
	tab := map[string]types.Type{}
	// 标量闭集先全部登记：字面量（`const 3` / `const "s"`）的类型由 §一 的规则给出，
	// 它们不一定出现在任何签名里，但打印分派要用它们。
	for _, b := range []types.Type{
		types.TI8, types.TI16, types.TI32, types.TI64,
		types.TU8, types.TU16, types.TU32, types.TU64, types.TUsize,
		types.TF32, types.TF64, types.TBool, types.TStr, types.TErr,
		types.TBytes, types.TMutex,
	} {
		tab[air.TyText(b)] = b
	}
	var note func(t types.Type)
	note = func(t types.Type) {
		if t == nil {
			return
		}
		text := air.TyText(t)
		if _, seen := tab[text]; seen {
			return
		}
		tab[text] = t
		// 与 noteNamedPkg 同一条约定（那里有注释）：**不带 `*` 的裸文本**也要登记 ——
		// 分配点（`alloc task(ok_Box[i32])`）与字段宿主解析用的是对象类型本身。
		// 泛型实例此前只有带 `*` 的键，宿主查表落空 ⇒ 字段名掉成 `_0`（740 实测）。
		if bare := strings.TrimSuffix(text, "*"); bare != "" && bare != text {
			if _, seen := tab[bare]; !seen {
				tab[bare] = t
			}
		}
		switch v := t.(type) {
		case *types.Slice:
			note(v.Elem)
		case *types.SetT:
			note(v.Elem)
		case *types.MapT:
			note(v.Key)
			note(v.Value)
		case *types.ArrayT:
			note(v.Elem)
		case *types.ChanT:
			note(v.Elem)
		case *types.Instance:
			note(v.Base)
			for _, a := range v.Args {
				note(a)
			}
		case *types.FuncT:
			for _, p := range v.Params {
				note(p)
			}
			note(v.Result)
		}
	}
	pkgs := make([]string, 0, len(c.deps))
	for p := range c.deps {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		info := c.deps[p]
		// 具名类型：Pkg 字段并非处处填（跨包引用时可能是空串），故把两种文本都登记：
		// 空包名的键留给"本包"解释，带包名的键留给跨包使用点。
		names := make([]string, 0, len(info.Classes))
		for n := range info.Classes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			cl := info.Classes[n]
			note(cl)
			noteNamedPkg(tab, cl, p)
			for i := range cl.Fields {
				note(cl.Fields[i].Type)
			}
			mns := make([]string, 0, len(cl.Methods))
			for mn := range cl.Methods {
				mns = append(mns, mn)
			}
			sort.Strings(mns)
			for _, mn := range mns {
				noteSig(note, cl.Methods[mn])
			}
		}
		ens := make([]string, 0, len(info.Enums))
		for n := range info.Enums {
			ens = append(ens, n)
		}
		sort.Strings(ens)
		for _, n := range ens {
			note(info.Enums[n])
			noteNamedPkg(tab, info.Enums[n], p)
		}
		ifs := make([]string, 0, len(info.Interfaces))
		for n := range info.Interfaces {
			ifs = append(ifs, n)
		}
		sort.Strings(ifs)
		for _, n := range ifs {
			note(info.Interfaces[n])
			noteNamedPkg(tab, info.Interfaces[n], p)
		}
		fns := make([]string, 0, len(info.Funcs))
		for n := range info.Funcs {
			fns = append(fns, n)
		}
		sort.Strings(fns)
		for _, n := range fns {
			noteSig(note, info.Funcs[n])
		}
		for _, fi := range info.FuncInsts {
			if fi.Fn != nil {
				noteSig(note, fi.Fn)
			}
			for _, a := range fi.Args {
				note(a)
			}
		}
		// 表达式类型表：**最全的一份**（含 Option 这类合成实例、容器元素、元组），
		// 单靠签名与具名类型会漏掉只在表达式里出现过的形状。
		for _, t := range info.Types {
			note(t)
		}
		for _, t := range info.TypeExprTypes {
			note(t)
		}
		for _, ts := range info.CallTypeArgs {
			for _, t := range ts {
				note(t)
			}
		}
	}
	c.irTab = tab
}

// noteNamedPkg 给具名类型补一份「带包名」的文本键（Pkg 为空时同一对象有另一种文本）。
// 同时登记**不带 `*` 的裸名**：`alloc task(ok_Buf)` 这类位置用的是对象类型本身
// （类在 AIR 文本里带 `*` 表示引用，但分配点要的是被分配的那个结构体）。
func noteNamedPkg(tab map[string]types.Type, t types.Type, pkg string) {
	if pkg == "" {
		return
	}
	name, star := "", ""
	switch v := t.(type) {
	case *types.Class:
		name, star = v.Name, ""
		if !v.Packed {
			star = "*"
		}
	case *types.Enum:
		name = v.Name
	case *types.Interface:
		name = v.Name
	default:
		return
	}
	for _, text := range []string{air.TypeSym(pkg, name) + star, air.TypeSym(pkg, name)} {
		if _, seen := tab[text]; !seen {
			tab[text] = t
		}
	}
}

func noteSig(note func(types.Type), sig *types.FuncSig) {
	if sig == nil {
		return
	}
	for _, p := range sig.ParamTypes {
		note(p)
	}
	for _, r := range sig.Results {
		note(r)
	}
}

// irTypeOfText 取 AIR 类型文本对应的语义类型（拿不到 = 第二种返回值 false）。
func (c *Ctx) irTypeOfText(text string) (types.Type, bool) {
	if text == "" || text == "void" {
		return nil, true
	}
	t, ok := c.irTab[text]
	return t, ok
}

// irNeedRetStruct 登记一个"合成返回结构体"的需求（元组/多返回值类型）。
// 这些 typedef 必须出现在任何使用点之前，而使用点散在函数体里 ⇒ 与打印器/见证表
// 同一套两遍机制：第一遍发现，第二遍在类型定义段统一发。
func (c *Ctx) irNeedRetStruct(name string, elems []types.Type) {
	if c.irRetTypes == nil {
		c.irRetTypes = map[string][]types.Type{}
	}
	if _, seen := c.irRetTypes[name]; seen {
		return
	}
	c.irRetTypes[name] = elems
	c.irRetOrder = append(c.irRetOrder, name)
}

// irEmitRetStructs 发射 IR 路径发现的多返回结构体（已在 retSeen 里的跳过：那些
// 由签名驱动的 emitRetStructs 发过，重复 typedef 在 C99 里是错）。
func (c *Ctx) irEmitRetStructs() {
	emitted := false
	for _, name := range c.irRetOrder {
		if c.retSeen[name] {
			continue
		}
		if _, isStd := c.irRetStd(name); isStd {
			continue // 运行时已声明（aic_std.h）
		}
		c.retSeen[name] = true
		c.line("typedef struct %s {", name)
		for i, t := range c.irRetTypes[name] {
			c.line("    %s _%d;", c.cTypeName(t), i)
		}
		c.line("} %s;", name)
		emitted = true
	}
	if emitted {
		c.line("")
	}
}

// irRetStd 报告该名字是否是运行时（aic_std.h）已声明的返回结构。
func (c *Ctx) irRetStd(name string) (string, bool) {
	for _, n := range stdRetStructs {
		if n == name {
			return n, true
		}
	}
	return "", false
}

// irRetNameOf 取（并登记）多值类型的 C 结构体名。
func (c *Ctx) irRetNameOf(elems []string) (string, error) {
	ts := make([]types.Type, 0, len(elems))
	for _, e := range elems {
		t, ok := c.irTab[e]
		if !ok {
			return "", fmt.Errorf("emit(IR): no C mapping for %q in the multi-value type", e)
		}
		ts = append(ts, t)
	}
	name := c.retStructNameFor(ts)
	c.irNeedRetStruct(name, ts)
	return name, nil
}

// irCText 把 AIR 类型文本翻成 C 类型名。未映射的文本**报错**（不猜、不发 void）。
func (c *Ctx) irCText(text string) (string, error) {
	if text == "" || text == "void" {
		return "void", nil
	}
	// 多值/元组先判：`(A, B)` 不是 C 类型名，而是"合成返回结构"（表里可能也有一条
	// 语义类型记录，但那不是 C 类型）。
	if elems, ok := splitMultiText(text); ok {
		return c.irRetNameOf(elems)
	}
	if t, ok := c.irTab[text]; ok {
		return c.cTypeName(t), nil
	}
	// **指针类型文本** `<base>*`（AddrRHS 的 let / 按引用形参）：C 类型 = 基类型
	// 的 C 名 + ` *`。类引用文本自带 `*`（已在 irTab 里先命中），这里是标量槽
	// 地址那段（`bool*` → `bool *`）。
	if base, isPtr := splitPtrText(text); isPtr {
		ct, err := c.irCText(base)
		if err != nil {
			return "", err
		}
		return ct + " *", nil
	}
	return "", fmt.Errorf("emit(IR): no C mapping for the type text %q", text)
}

// splitPtrText 拆「指针类型文本」`<base>*`（仅对**标量/容器**基座有意义：
// 类引用文本本身就以 `*` 结尾且已在 irTab 里，走到这里是 `bool*` 这种）。
func splitPtrText(text string) (string, bool) {
	if !strings.HasSuffix(text, "*") {
		return "", false
	}
	base := strings.TrimSuffix(text, "*")
	if base == "" {
		return "", false
	}
	return base, true
}

// irDeclText 是「带声明子」的 C 声明（[T;N] 与函数指针的 C 语法要求名字写在中间）。
func (c *Ctx) irDeclText(text, name string) (string, error) {
	if strings.HasPrefix(text, envPrefix) {
		// 闭包环境：统一的 `void *env__`（闭包值 = {fn(void*, …), env}），
		// 体内按需强转成该闭包自己的环境结构体。
		return "void *" + name, nil
	}
	if elems, ok := splitMultiText(text); ok {
		// 多返回/元组：合成返回结构体（运行时已声明的形状走 aic_std.h）。
		stName, err := c.irRetNameOf(elems)
		if err != nil {
			return "", err
		}
		return stName + " " + name, nil
	}
	if t, ok := c.irTab[text]; ok {
		return c.cTypeDecl(t, name), nil
	}
	// 指针类型文本（`bool*`）：`<基类型> *<name>`（AddrRHS 的 let 与 defer 上下文字段）。
	if base, isPtr := splitPtrText(text); isPtr {
		ct, err := c.irCText(base)
		if err != nil {
			return "", err
		}
		return ct + " *" + name, nil
	}
	return "", fmt.Errorf("emit(IR): no C declaration for the type text %q", text)
}

// splitMultiText 拆 `(A, B)` 形态的多值类型文本（顶层逗号）。
func splitMultiText(text string) ([]string, bool) {
	if len(text) < 2 || text[0] != '(' || text[len(text)-1] != ')' {
		return nil, false
	}
	inner := text[1 : len(text)-1]
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '(', '[', '<':
			depth++
		case ')', ']', '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(inner[start:]))
	return out, len(out) > 0
}

// envPrefix 是 AIR 里闭包环境伪类型的前缀（`env(<符号>)`）。
const envPrefix = "env("

// envStructName 是某个闭包的环境结构体名（确定性：由闭包符号拼出）。
func (c *Ctx) envStructName(sym string) string {
	return "aic_env_" + sanitize(sym)
}

// ---------------------------------------------------------------------------
// 值名与临时量
// ---------------------------------------------------------------------------

// irVal 把一个 AIR 值名翻成 C 表达式：`const <字面量>` 是内联字面量，其余是变量名。
func (c *Ctx) irVal(v string) string {
	if strings.HasPrefix(v, "const ") {
		return c.irConst(strings.TrimPrefix(v, "const "))
	}
	if v == "nil" {
		return "NULL"
	}
	return c.irName(v)
}

// irName 把一个 AIR 变量名翻成它在**当前 C 作用域**里的名字。
// 源语言允许遮蔽（先 `for i, x in xs` 再 `var i i32 = 0`），而 IR 里两者同名；
// 平铺到 C 就是"重复定义"（717 语料实测）。故按 CIR 的块结构维护作用域别名表：
// 同名再次声明时给新名字，出作用域恢复外层绑定。
func (c *Ctx) irName(name string) string {
	for i := len(c.irScopes) - 1; i >= 0; i-- {
		if b, ok := c.irScopes[i][name]; ok {
			return b.cname
		}
	}
	return name
}

// irScopedTy 取一个名字在**当前作用域**里的 AIR 类型文本（遮蔽时外层类型不算数）。
func (c *Ctx) irScopedTy(name string) (string, bool) {
	for i := len(c.irScopes) - 1; i >= 0; i-- {
		if b, ok := c.irScopes[i][name]; ok {
			if b.ty == "" {
				return "", false
			}
			return b.ty, true
		}
	}
	return "", false
}

// irBind 在当前作用域绑定一个 C 名与类型（本作用域重复声明 = 遮蔽，给确定性后缀）。
func (c *Ctx) irBind(name, ty string) string {
	if len(c.irScopes) == 0 {
		c.irScopes = append(c.irScopes, map[string]irBinding{})
	}
	cur := c.irScopes[len(c.irScopes)-1]
	if _, exists := cur[name]; !exists && c.irName(name) == name {
		cur[name] = irBinding{cname: name, ty: ty}
		return name
	}
	c.irShadow++
	fresh := fmt.Sprintf("%s__%d", name, c.irShadow)
	cur[name] = irBinding{cname: fresh, ty: ty}
	return fresh
}

// irBinding 是一个名字在当前 C 作用域里的落点。
type irBinding struct {
	cname string
	ty    string
}

func (c *Ctx) irPushScope() { c.irScopes = append(c.irScopes, map[string]irBinding{}) }

func (c *Ctx) irPopScope() {
	if len(c.irScopes) > 1 {
		c.irScopes = c.irScopes[:len(c.irScopes)-1]
	}
}

// irSlotVal 把一个值名翻成"写进某个类型槽位"的 C 表达式。
// `nil` 在 AIR 里是**裸值名**（不带类型），只有槽位类型能决定它的零值形态
// （NULL / ((aic_str){0,0}) / ((aic_iface){0}) / AIC_ERR_NONE）。
func (c *Ctx) irSlotVal(val, slotTy string) (string, error) {
	if val != "nil" {
		return c.irVal(val), nil
	}
	return c.irZero(slotTy)
}

// irConst 把 AIR 的字面量文本翻成 C 字面量。
func (c *Ctx) irConst(lit string) string {
	// **函数符号常量**（`const ok_lambda1`：lambda 提升体的引用、函数名作值）：
	// C 名必须是 aic_<pkg>_<name>，裸符号在 C 侧没有定义（718 实测）。
	// 两条值路径（irVal 的 const 分支与 Let 的 Const RHS）都汇到这里，故只此一处。
	if name, ok, _ := c.irFuncRefCName(lit); ok {
		return name
	}
	switch lit {
	case "true", "false":
		return lit
	case "nil":
		return "NULL"
	}
	if strings.HasPrefix(lit, "\"") {
		return c.strLit(unquoteAIC(lit))
	}
	// 数值：AIC 允许 `0b1010` 与 `1_000`，C11 两者都没有。
	s := strings.ReplaceAll(lit, "_", "")
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	switch {
	case strings.HasPrefix(s, "0b"), strings.HasPrefix(s, "0B"):
		var n uint64
		for _, ch := range s[2:] {
			if ch != '0' && ch != '1' {
				return lit
			}
			n = n<<1 | uint64(ch-'0')
		}
		s = fmt.Sprintf("%dULL", n)
	case strings.HasPrefix(s, "0o"), strings.HasPrefix(s, "0O"):
		var n uint64
		for _, ch := range s[2:] {
			if ch < '0' || ch > '7' {
				return lit
			}
			n = n*8 + uint64(ch-'0')
		}
		s = fmt.Sprintf("%dULL", n)
	}
	if neg {
		return "-" + s
	}
	return s
}

// irValTy 取一个 AIR 值名的类型文本（临时量/变量/形参）。
// 字面量按 §一 的最窄规则定（同一条规则由 types.LiteralTy 持有）。
func (c *Ctx) irValTy(v string) string {
	if strings.HasPrefix(v, "const ") {
		return types.LiteralTy(strings.TrimPrefix(v, "const "))
	}
	// 遮蔽的名字以**当前作用域**的类型为准：外层同名变量的类型不算数。
	// 693 语料里同一个 `x` 在一处是 str、另一处是 usize，用错类型会把 `i + 1`
	// 发成字符串拼接（C 层直接报错，或者更糟：编过了但语义错）。
	if ty, ok := c.irScopedTy(v); ok {
		return ty
	}
	return c.irTemps[v]
}

// irValC 是「值名 → C 表达式」并顺带把字面量的类型定下来（打印分派要用）。
func (c *Ctx) irValC(v string) (string, error) {
	if strings.HasPrefix(v, "const ") {
		if strings.Contains(v, "\x00") {
			return "", fmt.Errorf("emit(IR): NUL in a literal is not supported")
		}
		return c.irVal(v), nil
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 调用符号 → C 符号
// ---------------------------------------------------------------------------

// irCNameOf 是签名在 C 侧的符号名（与 emitProtos 的 cFuncName 同一规则，
// 但包名由调用方给出：跨包调用点上 c.pkg() 不是签名所属的包）。
func (c *Ctx) irCNameOf(pkg string, sig *types.FuncSig) string {
	if sig == nil {
		return ""
	}
	if sig.Extern || sig.Export {
		return ExternName(sig.Name)
	}
	return MangleFunc(c.ownerPkg(pkg), sig.Recv, sig.Name)
}

// irFuncRefCName 把一个**函数符号常量**翻成 C 函数名。
// 判据 = 该符号在调用表或 IR 体表里登记过（lambda 提升体 / 函数名作值都用这个
// 形态）；两表都没有就不是函数引用（普通字面量照原样走 irConst）。
func (c *Ctx) irFuncRefCName(lit string) (string, bool, error) {
	if lit == "" || strings.ContainsAny(lit, " \t\"") {
		return "", false, nil
	}
	_, inCall := c.irCall[lit]
	_, inBodies := c.ir.Bodies[lit]
	if !inCall && !inBodies {
		return "", false, nil
	}
	pkg, name := c.splitSymPkg(lit)
	if name == "" {
		return "", true, fmt.Errorf("emit(IR): cannot resolve the function symbol %q", lit)
	}
	return MangleFunc(c.ownerPkg(pkg), "", name), true, nil
}

// irFuncCName 定函数体的 C 符号名（三源同表）：
//  ① 实例符号 `<base>[args]` ⇒ 实例名（调用点 irCallee 同一规则）；
//  ② extern/export ⇒ 零 mangle 原名；
//  ③ 其余 ⇒ 签名表（包名 + 接收者 + 名）。
// lambda 提升体走 ③：合成签名时 Name 取符号尾段、无接收者。
func (c *Ctx) irFuncCName(sym string, sig *types.FuncSig) string {
	if strings.Contains(sym, "[") {
		if inst, ok := c.irInstCName(sym); ok {
			return inst
		}
	}
	return c.irCNameOf(c.pkg(), sig)
}

// buildIRCallMap 建「IR 符号 → C 符号」表：用户函数、方法、泛型函数实例。
func (c *Ctx) buildIRCallMap() {
	m := map[string]string{}
	pkgs := make([]string, 0, len(c.deps))
	for p := range c.deps {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		info := c.deps[p]
		for _, sig := range info.Funcs {
			if sig == nil {
				continue
			}
			m[air.FuncSym(p, sig.Recv, sig.Name)] = c.irCNameOf(p, sig)
		}
		for _, cl := range info.Classes {
			for _, sig := range cl.Methods {
				if sig == nil {
					continue
				}
				cp := c.ownerPkg(types.ClassPkg(cl))
				m[air.FuncSym(cp, cl.Name, sig.Name)] = MangleFunc(cp, cl.Name, sig.Name)
			}
		}
		for _, fi := range info.FuncInsts {
			if fi.Fn == nil || len(fi.Args) == 0 {
				continue
			}
			m[air.FuncInstSym(p, fi.Fn.Recv, fi.Fn.Name, fi.Args)] = funcInstName(p, fi.Fn, fi.Args)
		}
	}
	c.irCall = m
}

// irCallee 给一个 IR 调用符号定 C 符号：先查签名表，再按泛型实例的机械规则拼
// （实例方法调用点没有单独的签名登记）。运行时/内建不走这里（见 irBuiltinCall）。
//
// **带 TypeArgs 的调用是实例调用**：键 = `<sym>[<T1>, …]`（air.FuncInstSym 的拼法）。
// 直接拿裸 sym 查表会命中泛型**模板**自己的那一项（`info.Funcs` 里模板也在册），
// 于是所有实例都链到 `aic_ok_id` 这一个符号 —— 原型发的是实例名、定义也缺实例后缀，
// 最后 C 侧"隐式声明 + 重复定义"（740 实测）。
func (c *Ctx) irCallee(sym string, typeArgs []string) (string, error) {
	if len(typeArgs) > 0 {
		key := sym + "[" + strings.Join(typeArgs, ", ") + "]"
		if name, ok := c.irCall[key]; ok {
			return name, nil
		}
		if name, ok := c.irInstCName(key); ok {
			return name, nil
		}
		return "", fmt.Errorf("emit(IR): no monomorphized instance for %s (the checker and the IR disagree — compiler bug, not a source error)", key)
	}
	if name, ok := c.irCall[sym]; ok {
		return name, nil
	}
	if name, ok := c.irInstCName(sym); ok {
		return name, nil
	}
	// **IR 提升体**（defer 块形 / lambda）：签名表里没有，但 IR 体表里有 ——
	// 符号 = `<pkg>_<name>`，按同一张 mangling 表拼（035/718 实测）。
	if _, ok := c.ir.Bodies[sym]; ok {
		if pkg, name := c.splitSymPkg(sym); name != "" {
			return MangleFunc(c.ownerPkg(pkg), "", name), nil
		}
	}
	return "", fmt.Errorf("emit(IR): cannot resolve the call symbol %q to a C symbol", sym)
}

// irInstCName 按泛型实例的机械规则拼 C 符号：
// IR 符号 `<pkg>_<T>[_<m>][<a>, <b>]` → `aic_<pkg>_<T>[_<m>]__<seg(a)>_<seg(b)>`
// （后缀段规则 = typeSeg，实例发射用的也是它）。
func (c *Ctx) irInstCName(sym string) (string, bool) {
	open := strings.IndexByte(sym, '[')
	if open < 0 || !strings.HasSuffix(sym, "]") {
		return "", false
	}
	base := sym[:open]
	args := splitArgTexts(sym[open+1 : len(sym)-1])
	pkg, name := c.splitSymPkg(base)
	if name == "" {
		return "", false
	}
	segs := make([]string, 0, len(args))
	for _, a := range args {
		t, ok := c.irTab[a]
		if !ok {
			return "", false
		}
		segs = append(segs, typeSeg(t))
	}
	return MangleGeneric(pkg, name, segs), true
}

// splitSymPkg 把 IR 符号拆成 (包名, 名字)：包名 = 已知依赖包里最长的那个前缀。
func (c *Ctx) splitSymPkg(base string) (string, string) {
	best := ""
	for p := range c.deps {
		if p == "" || !strings.HasPrefix(base, p+"_") {
			continue
		}
		if len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		if strings.HasPrefix(base, c.pkg()+"_") {
			return c.pkg(), base[len(c.pkg())+1:]
		}
		return "", base
	}
	return best, base[len(best)+1:]
}

// splitArgTexts 拆 `<a>, <b>`（类型实参文本表，嵌套括号内的逗号不算）。
func splitArgTexts(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(', '<':
			depth++
		case ']', ')', '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// stdRuntimeCap 把运行时符号名归到 L2 按需链接的能力位（红线 16：不用不发）。
func stdRuntimeCap(name string) string {
	switch {
	case strings.HasPrefix(name, "aic_list_"), strings.HasPrefix(name, "aic_map_"),
		strings.HasPrefix(name, "aic_set_"), strings.HasPrefix(name, "aic_str_"),
		strings.HasPrefix(name, "aic_bytes_"), strings.HasPrefix(name, "aic_option_"),
		strings.HasPrefix(name, "aic_array_"):
		return "l1"
	case strings.HasPrefix(name, "aic_chan_"), strings.HasPrefix(name, "aic_spawn"),
		strings.HasPrefix(name, "aic_scope_"), strings.HasPrefix(name, "aic_select_"),
		strings.HasPrefix(name, "aic_sched"), strings.HasPrefix(name, "aic_mutex"),
		strings.HasPrefix(name, "aic_task"):
		return "l2"
	}
	return "l0"
}

// ---------------------------------------------------------------------------
// 函数体
// ---------------------------------------------------------------------------

// emitIRFuncs 发射本文件/本包的全部函数体（IR 路径）。
func (c *Ctx) emitIRFuncs(u *Unit) error {
	for _, fu := range u.Files {
		c.use(fu)
		if fu.File == nil || fu.Info == nil {
			continue
		}
		for _, d := range fu.File.Decls {
			switch v := d.(type) {
			case *parse.FuncDecl:
				if len(v.TypeParams) > 0 {
					continue // 模板体不发；实例各占一个符号（emitIRInstFuncs）
				}
				if v.Extern && v.Body == nil {
					continue
				}
				sig, _ := c.Info.Funcs[v.Name]
				if sig == nil {
					return fmt.Errorf("emit(IR): unknown function %s", v.Name)
				}
				if err := c.irOneFunc(air.FuncSym(c.pkg(), "", v.Name), sig, v.Pos); err != nil {
					return err
				}
			case *parse.ClassDecl:
				if len(v.TypeParams) > 0 {
					continue
				}
				cl := c.Info.Classes[v.Name]
				for _, m := range v.Methods {
					if m.Body == nil || m.Extern {
						continue
					}
					var sig *types.FuncSig
					if cl != nil {
						sig, _ = cl.Method(m.Name)
					}
					if sig == nil {
						return fmt.Errorf("emit(IR): unknown method %s.%s", v.Name, m.Name)
					}
					if err := c.irOneFunc(air.FuncSym(c.pkg(), v.Name, m.Name), sig, m.Pos); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// emitIRLambdaFuncs 发射 lambda 提升出来的静态函数体。
//
// 为什么单独一遍：lambda 在 AST 里不是 Decl（是表达式），emitIRFuncs 走
// File.Decls 找不到它们；而 IR 体表里有（air.lambdaExpr 提升成模块 Func，
// Flags 带 "static"）。漏了这条，`const ok_lambda1` 引用一个没有定义的函数
// （718 实测：C 层隐式声明）。
func (c *Ctx) emitIRLambdaFuncs() error {
	for _, sym := range c.irLambdaSyms() {
		name := sym
		if i := strings.LastIndexByte(sym, '_'); i > 0 && i+1 < len(sym) {
			// 形参/返回类型在 IR 体里（实例上下文已代换），签名表里没有这些函数 ——
			// 合成一份只带名字的签名（存储类 static 由 Flags 表达）。
			name = sym[i+1:]
		}
		if err := c.irOneFunc(sym, &types.FuncSig{Name: name}, parse.Pos{}); err != nil {
			return err
		}
	}
	return nil
}

// emitIRLambdaProtos 发 lambda 提升体的原型：C 要求先声明后使用，而原型段
// （emitProtos）只走签名表 —— 签名表里没有这些函数，故单独发（718 实测：
// `aic_ok_lambda1` 在 main 里先被引用，定义在文件末尾）。
func (c *Ctx) emitIRLambdaProtos() error {
	for _, sym := range c.irLambdaSyms() {
		f := c.ir.Bodies[sym]
		retCT, err := c.irRetCT(f)
		if err != nil {
			return err
		}
		params, err := c.irParamList(f)
		if err != nil {
			return err
		}
		name := sym
		if i := strings.LastIndexByte(sym, '_'); i > 0 && i+1 < len(sym) {
			name = sym[i+1:]
		}
		c.line("static %s %s(%s);", retCT, c.irFuncCName(sym, &types.FuncSig{Name: name}), params)
	}
	return nil
}

// irLambdaSyms 取全部待发射的 lambda 提升体符号（IR 体表 + static 标记 + 本遍
// 未消费；排序保 H2 确定性）。
func (c *Ctx) irLambdaSyms() []string {
	var syms []string
	for sym, f := range c.ir.Bodies {
		if c.ir.used[sym] {
			continue
		}
		for _, fl := range f.Flags {
			if fl == "static" {
				syms = append(syms, sym)
				break
			}
		}
	}
	sort.Strings(syms)
	return syms
}

// emitIRInstFuncs 发射泛型实例的函数体（函数实例 + 泛型类的方法实例）。
func (c *Ctx) emitIRInstFuncs(u *Unit) error {
	for _, e := range c.funcInsts {
		if e.sig == nil {
			continue
		}
		sym := air.FuncInstSym(e.pkg, e.sig.Recv, e.fn, e.args)
		if err := c.irOneFunc(sym, e.sig, c.irInstPos(u, e)); err != nil {
			return err
		}
	}
	for _, m := range c.collectInstMethods(u) {
		if m.sig == nil || m.decl == nil {
			continue
		}
		pkg := c.ownerPkg(types.ClassPkg(m.cl))
		sym := air.FuncInstSym(pkg, m.cl.Name, m.decl.Name, m.inst.Args)
		if err := c.irOneFunc(sym, m.sig, m.decl.Pos); err != nil {
			return err
		}
	}
	return nil
}

// irInstPos 找实例模板的源位置（#line 回指用；找不到给零值 = 不回指）。
func (c *Ctx) irInstPos(u *Unit, e funcInstEntry) parse.Pos {
	for _, fu := range u.Files {
		if fu.File == nil {
			continue
		}
		for _, d := range fu.File.Decls {
			if fd, ok := d.(*parse.FuncDecl); ok && fd.Name == e.fn && e.sig.Recv == "" {
				return fd.Pos
			}
			if cd, ok := d.(*parse.ClassDecl); ok && cd.Name == e.sig.Recv {
				for _, m := range cd.Methods {
					if m.Name == e.fn {
						return m.Pos
					}
				}
			}
		}
	}
	return parse.Pos{}
}

// irOneFunc 发射一个函数体：签名取签名表（符号/存储类），形参与返回位取 IR 体
// （实例体在 IR 里已经是代换过的具体类型，比签名表更准）。
func (c *Ctx) irOneFunc(sym string, sig *types.FuncSig, pos parse.Pos) error {
	f := c.ir.Bodies[sym]
	if f == nil {
		return fmt.Errorf("emit(IR): no IR body for %s (the checker and the IR disagree — compiler bug, not a source error)", sym)
	}
	c.ir.used[sym] = true
	// 事实按函数重置（与直译路径同纪律）：长度/守卫/归纳事实只在本函数体内有效。
	c.lenSym, c.lenFacts, c.appendFacts, c.loopCondBound, c.loopLenBound = nil, nil, nil, nil, nil
	c.declaredVars = map[string]bool{}
	for _, p := range f.Params {
		c.declaredVars[p.Name] = true
	}
	savedName, savedFn, savedLoop := c.curFuncName, c.curFunc, c.loopDepth
	savedTemps, savedIRFn, savedSym := c.irTemps, c.irCurFunc, c.irCurSym
	savedScopes, savedShadow := c.irScopes, c.irShadow
	c.curFuncName, c.curFunc, c.loopDepth = types.FuncKey(sig.Recv, sig.Name), sig, 0
	c.irTemps, c.irCurFunc, c.irCurSym = map[string]string{}, f, sym
	c.irScopes, c.irShadow = []map[string]irBinding{{}}, 0
	defer func() {
		c.curFuncName, c.curFunc, c.loopDepth = savedName, savedFn, savedLoop
		c.irTemps, c.irCurFunc, c.irCurSym = savedTemps, savedIRFn, savedSym
		c.irScopes, c.irShadow = savedScopes, savedShadow
	}()

	// defer 三件套：trampoline + 上下文结构体必须在函数之前（C 的定义顺序）。
	st, err := c.irDeferSites(f)
	if err != nil {
		return err
	}
	savedDefer := c.irCurDefer
	c.irCurDefer = st
	defer func() { c.irCurDefer = savedDefer }()
	// 形参类型必须先登记：trampoline 的上下文结构体要靠它给实参定型
	// （`defer note(tag)` 的 tag 就是形参）；体内后出现的 let 也要先扫一遍。
	params, err := c.irParamList(f)
	if err != nil {
		return err
	}
	c.irCollectTemps(f)
	c.irBindRefParams(f)
	if err := c.irEmitDeferTrampolines(f, st); err != nil {
		return err
	}
	retCT, err := c.irRetCT(f)
	if err != nil {
		return err
	}
	store := c.storageClass(sig)
	if c.isMain(sig) {
		store = "static " // 入口只被文件末尾的 C wrapper 调用（与直译路径一致）
	}
	// **实例体的 C 符号必须带实例后缀**：irCNameOf(sig) 只会给模板名（`aic_ok_id`），
	// 三个实例会互相覆盖、调用点链到错符号（740 实测：原型有后缀、定义没有）。
	// lambda 提升体同理：合成签名只带尾段名，包名从符号拆（irFuncCName 三源同表）。
	cName := c.irFuncCName(sym, sig)
	c.srcLine(pos)
	c.line("%s%s %s(%s) {", store, retCT, cName, params)
	if st != nil && st.stack != "" {
		c.line("    aic_defer_stack %s = AIC_DEFER_STACK_EMPTY;", st.stack)
	}
	if err := c.irStmts(f.Body, "    "); err != nil {
		return err
	}
	if len(c.irScopes) > 1 {
		return fmt.Errorf("emit(IR): unbalanced C scopes in %s", sym)
	}
	c.line("}")
	c.line("")
	return nil
}

// irCollectTemps 预登记函数体里全部临时量/局部量的类型（形参也在内：它们不是
// 体里的 let/var，但体与 trampoline 都要按形参类型定位实参）。
// 必须在发 trampoline 之前跑：defer 站点的上下文结构体要用实参类型，而那些实参
// 是函数体里后出现的 let（`defer note(tag)` 的 tag 由注册点绑定）。
func (c *Ctx) irCollectTemps(f *cir.Func) {
	for _, p := range f.Params {
		c.irTemps[p.Name] = p.Ty
	}
	var walk func([]cir.Stmt)
	walk = func(ls []cir.Stmt) {
		for _, s := range ls {
			switch v := s.(type) {
			case *cir.Leaf:
				switch in := v.In.(type) {
				case *air.Let:
					if in.Ty != "void" {
						c.irTemps[in.Tmp] = in.Ty
					}
				case *air.Var:
					c.irTemps[in.Name] = in.Ty
				case *air.SelWait:
					c.irTemps[in.Tmp] = "i32"
				}
			case *cir.If:
				walk(v.Then)
				walk(v.Else)
			case *cir.For:
				walk(v.Head)
				walk(v.Body)
				walk(v.Post)
			case *cir.Switch:
				for _, cs := range v.Cases {
					walk(cs.Body)
				}
			case *cir.Region:
				walk(v.Body)
			}
		}
	}
	walk(f.Body)
}

// irRetCT 是 IR 函数体的 C 返回类型（0 = void，1 = 该类型，多 = 合成返回结构）。
func (c *Ctx) irRetCT(f *cir.Func) (string, error) {
	if len(f.Rets) == 0 {
		return "void", nil
	}
	if len(f.Rets) == 1 {
		return c.irCText(f.Rets[0])
	}
	return c.irRetNameOf(f.Rets)
}

// irParamList 是 IR 函数体的 C 形参表（**纯函数**：不登记 irTemps —— 原型与体
// 两处都调它，副作用会让原型段就把名字写进当遍的类型表）。
//
// **按引用形参**（Param.Ref，defer 块形提升体的捕获槽）：C 侧声明为
// `Ty *name__p`，体内对 name 的每个引用都经别名表变成 `(*name__p)` ——
// 出口读到的是调用方变量槽的最终值（035/713 锚点）。
func (c *Ctx) irParamList(f *cir.Func) (string, error) {
	parts := make([]string, 0, len(f.Params))
	for _, p := range f.Params {
		ct, err := c.irCText(p.Ty)
		if err != nil {
			return "", err
		}
		if p.Ref {
			parts = append(parts, ct+" *"+p.Name+"__p")
			continue
		}
		decl, err := c.irDeclText(p.Ty, p.Name)
		if err != nil {
			return "", err
		}
		parts = append(parts, decl)
	}
	if len(parts) == 0 {
		return "void", nil
	}
	return strings.Join(parts, ", "), nil
}

// irBindRefParams 把按引用形参的名字绑成 `(*name__p)`（体内读写都经它）。
// 必须在体发射之前、作用域已重置之后调用。
func (c *Ctx) irBindRefParams(f *cir.Func) {
	for _, p := range f.Params {
		if !p.Ref {
			continue
		}
		c.irBindAlias(p.Name, "(*"+p.Name+"__p)", p.Ty)
	}
}

// irBindAlias 直接在当前作用域绑一个「AIR 名 → C 左值文本」的别名
// （irBind 会给冲突名换新标识符；这里要的就是任意的 C 左值形态）。
func (c *Ctx) irBindAlias(name, cname, ty string) {
	if len(c.irScopes) == 0 {
		c.irScopes = append(c.irScopes, map[string]irBinding{})
	}
	c.irScopes[len(c.irScopes)-1][name] = irBinding{cname: cname, ty: ty}
}

// ---------------------------------------------------------------------------
// 语句
// ---------------------------------------------------------------------------

func (c *Ctx) irStmts(list []cir.Stmt, ind string) error {
	for _, s := range list {
		if err := c.irStmt(s, ind); err != nil {
			return err
		}
	}
	return nil
}

func (c *Ctx) irStmt(s cir.Stmt, ind string) error {
	switch v := s.(type) {
	case *cir.Leaf:
		return c.irLeaf(v.In, ind)
	case *cir.Guard:
		return nil // 守卫已折进 Store 的 Limit；独立 Guard 节点不承载语义
	case *cir.If:
		c.srcLine(v.Loc.Pos(c.Path))
		cond, err := c.irCond(v.Cond)
		if err != nil {
			return err
		}
		c.line("%sif (%s) {", ind, cond)
		c.irPushScope()
		if err := c.irStmts(v.Then, ind+"    "); err != nil {
			return err
		}
		c.irPopScope()
		if len(v.Else) > 0 {
			c.line("%s} else {", ind)
			c.irPushScope()
			if err := c.irStmts(v.Else, ind+"    "); err != nil {
				return err
			}
			c.irPopScope()
		}
		c.line("%s}", ind)
		return nil
	case *cir.For:
		c.srcLine(v.Loc.Pos(c.Path))
		// C 的 `for(;;)` 里 `continue` 会跳过我们内联的 Post 段 —— 而 IR 的语义是
		// 「回到 forpost 重算」，跳过 Post 就是死循环（014 语料实测）。故循环体里
		// 只要出现 continue，就在 Post 前落一个标签，把 continue 发成 goto。
		needLabel := c.irHasContinue(v.Body) || c.irHasContinue(v.Head)
		label := c.tmp("post")
		c.line("%sfor (;;) {", ind)
		c.irPushScope()
		if err := c.irStmts(v.Head, ind+"    "); err != nil {
			return err
		}
		if v.Cond != "" {
			cond, err := c.irCond(v.Cond)
			if err != nil {
				return err
			}
			c.line("%s    if (!(%s)) break;", ind, cond)
		}
		// 体里的**遮蔽声明**必须关进迭代块：`for x in xs` 的元素绑定与索引变量同名
		//（同一个 AIR 名），C 里两条声明若在同一块，Post 的 `x = x + 1` 指到元素
		// 变量上（693 实测：str + 1 类型冲突；696 死循环同源）。Post 在迭代块**之后**
		// 发射，看到的仍是索引变量。
		iterScope := c.irBodyShadows(v.Body)
		if iterScope {
			c.line("%s    {", ind)
			c.irPushScope()
		}
		c.irLoopPost = append(c.irLoopPost, loopPost{hasCont: needLabel, label: label})
		err := c.irStmts(v.Body, ind+"    ")
		c.irLoopPost = c.irLoopPost[:len(c.irLoopPost)-1]
		if err != nil {
			return err
		}
		if iterScope {
			c.irPopScope()
			c.line("%s    }", ind)
		}
		if needLabel {
			c.line("%s%s: ;", ind, label)
		}
		if err := c.irStmts(v.Post, ind+"    "); err != nil {
			return err
		}
		c.irPopScope()
		c.line("%s}", ind)
		return nil
	case *cir.Switch:
		c.srcLine(v.Loc.Pos(c.Path))
		c.line("%sswitch (%s) {", ind, c.irVal(v.Val))
		for _, cs := range v.Cases {
			if cs.Label == "" {
				c.line("%sdefault:", ind)
			} else {
				c.line("%scase %s:", ind, cs.Label)
			}
			// **标签后面必须是语句**（C99/C11：声明不是语句）—— 直接跟一条声明是
			// 非法的，gcc 当扩展收下、tcc 按标准拒绝（027/708/783 实测：
			// `case 0: aic_i32 t3 = …;` ⇒ "'；' expected (got \"t3\")"）。
			// 包一层复合语句既合法又让每个分支各自成域。
			c.line("%s    {", ind)
			c.irPushScope()
			if err := c.irStmts(cs.Body, ind+"        "); err != nil {
				return err
			}
			c.irPopScope()
			c.line("%s    }", ind)
			c.line("%s    break;", ind)
		}
		c.line("%s}", ind)
		return nil
	case *cir.Region:
		if v.Scope {
			// `scope { }` = 任务域（不是内存区域）：调度器 scope + 区域层，
			// 退出 join 全部 spawn 的任务（L3 真并发；742/772/773 锚点）。
			// spawn 站点在体内，故 scope 变量名要进栈供它们取用。
			c.srcLine(v.Loc.Pos(c.Path))
			c.need("l2")
			name := c.tmp("scope")
			c.line("%saic_scope %s;", ind, name)
			c.line("%saic_scope_enter(&%s);", ind, name)
			c.line("%saic_region_push(%s, %d);", ind, cstr(c.locFile(v.Loc)), locLine(v.Loc))
			c.line("%s{", ind)
			c.irPushScope()
			savedScope := c.irScopeCur
			c.irScopeCur = name
			err := c.irStmts(v.Body, ind+"    ")
			c.irScopeCur = savedScope
			c.irPopScope()
			if err != nil {
				return err
			}
			c.line("%s}", ind)
			c.line("%saic_region_pop();", ind)
			c.line("%saic_scope_exit(&%s);", ind, name)
			return nil
		}
		c.srcLine(v.Loc.Pos(c.Path))
		c.line("%saic_region_push(%s, %d);", ind, cstr(c.locFile(v.Loc)), locLine(v.Loc))
		c.line("%s{", ind)
		c.irPushScope()
		if err := c.irStmts(v.Body, ind+"    "); err != nil {
			return err
		}
		c.irPopScope()
		c.line("%s}", ind)
		c.line("%saic_region_pop();", ind)
		return nil
	case *cir.Scope:
		// 词法块：**只发一对花括号 + 作用域进出**（没有 region 语义 —— 那是 Region）。
		// 同名遮蔽的落点由 C 的作用域决定，故这对花括号不能省（722 实测）。
		c.srcLine(v.Loc.Pos(c.Path))
		c.line("%s{", ind)
		c.irPushScope()
		if err := c.irStmts(v.Body, ind+"    "); err != nil {
			return err
		}
		c.irPopScope()
		c.line("%s}", ind)
		return nil
	case *cir.Return:
		c.srcLine(v.Loc.Pos(c.Path))
		vals := make([]string, 0, len(v.Vals))
		for _, x := range v.Vals {
			vals = append(vals, c.irVal(x))
		}
		switch len(vals) {
		case 0:
			c.line("%sreturn;", ind)
		case 1:
			c.line("%sreturn %s;", ind, vals[0])
		default:
			if c.irCurFunc == nil {
				return fmt.Errorf("emit(IR): return outside a function")
			}
			name, err := c.irRetCT(c.irCurFunc)
			if err != nil {
				return err
			}
			parts := make([]string, 0, len(vals))
			for i, x := range vals {
				parts = append(parts, fmt.Sprintf("._%d = %s", i, x))
			}
			c.line("%sreturn (%s){ %s };", ind, name, strings.Join(parts, ", "))
		}
		return nil
	case *cir.Break:
		c.line("%sbreak;", ind)
		return nil
	case *cir.Continue:
		// 循环体里内联了 Post 段：若有标签就 goto（否则 C 的 continue 会跳过 Post）。
		if n := len(c.irLoopPost); n > 0 && c.irLoopPost[n-1].hasCont {
			c.line("%sgoto %s;", ind, c.irLoopPost[n-1].label)
			return nil
		}
		c.line("%scontinue;", ind)
		return nil
	case *cir.CheckFailStmt:
		c.srcLine(v.Loc.Pos(c.Path))
		return c.irPropagateReturn(v.Err, ind)
	case *cir.Exit:
		return nil
	case *cir.Label:
		// 保真用的块标签（C 的标签有独立命名空间，但仍加前缀免得与 `post<n>`
		// 这类内联标签、以及用户标识符在阅读上打架）。标签后必须有语句。
		c.line("%s%s: ;", ind, c.irBlockLabel(v.Name))
		return nil
	case *cir.Goto:
		c.line("%sgoto %s;", ind, c.irBlockLabel(v.Name))
		return nil
	}
	return fmt.Errorf("emit(IR): unsupported statement %T", s)
}

// irBlockLabel 把 CIR 的块标签翻成 C 标签名（唯一实现：Label 与 Goto 同源）。
func (c *Ctx) irBlockLabel(name string) string { return "blk_" + name }

// loopPost 记录一层循环的内联 Post 段（continue 需要跳到它的标签）。
type loopPost struct {
	hasCont bool
	label   string
}

// irHasContinue 报告语句树里是否出现 continue（决定要不要落 Post 标签）。
func (c *Ctx) irHasContinue(list []cir.Stmt) bool {
	for _, s := range list {
		switch v := s.(type) {
		case *cir.Continue:
			return true
		case *cir.If:
			if c.irHasContinue(v.Then) || c.irHasContinue(v.Else) {
				return true
			}
		case *cir.For:
			// 内层循环的 continue 归内层（外层标签救不了它），故不下探。
			_ = v
		case *cir.Switch:
			for _, cs := range v.Cases {
				if c.irHasContinue(cs.Body) {
					return true
				}
			}
		case *cir.Region:
			if c.irHasContinue(v.Body) {
				return true
			}
		}
	}
	return false
}

// irBodyShadows 报告循环体顶层是否有**遮蔽声明**（var/let 的名字已在外层作用域
// 可见）。只查顶层：嵌套 if/for/switch 里的声明本来就落在自己的 C 块里，不影响
// Post 的名字解析。
func (c *Ctx) irBodyShadows(list []cir.Stmt) bool {
	for _, s := range list {
		leaf, ok := s.(*cir.Leaf)
		if !ok {
			continue
		}
		var name string
		switch in := leaf.In.(type) {
		case *air.Var:
			name = in.Name
		case *air.Let:
			name = in.Tmp
		default:
			continue
		}
		// 外层已有同名绑定（索引变量/外层局部）= 本循环体内再次声明会遮蔽它。
		if c.irName(name) != "" && c.irName(name) == name && c.irBound(name) {
			return true
		}
	}
	return false
}

// irBound 报告一个名字是否已在**任意**作用域绑定（含外层）。
func (c *Ctx) irBound(name string) bool {
	for i := len(c.irScopes) - 1; i >= 0; i-- {
		if _, ok := c.irScopes[i][name]; ok {
			return true
		}
	}
	return false
}

// irIsIntText 报告 AIR 类型文本是不是整型（除法查零只对整型：浮点除零是
// IEEE inf/nan，不是语言错误）。名字表与 BasicCName 同源。
func irIsIntText(text string) bool {
	switch text {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize":
		return true
	}
	return false
}

// irIntWidth 取整型的位宽（移位计数上界；usize 固定 64 位，§十 N13）。
func irIntWidth(text string) (int, bool) {
	switch text {
	case "i8", "u8":
		return 8, true
	case "i16", "u16":
		return 16, true
	case "i32", "u32":
		return 32, true
	case "i64", "u64", "usize":
		return 64, true
	}
	return 0, false
}

// irCond 取条件表达式的 C 文本（条件在 IR 里必然是单个值名/字面量）。
func (c *Ctx) irCond(cond string) (string, error) {
	if cond == "" {
		return "", fmt.Errorf("emit(IR): empty condition")
	}
	return c.irVal(cond), nil
}

// irPropagateReturn 发射 check 失败路径的返回（§六）：前置返回位取零值、末位置该 Err，
// 跑完 defer 再返回（defer 是函数级的 —— 这条出口同样要跑）。
func (c *Ctx) irPropagateReturn(errVal, ind string) error {
	if c.irCurFunc == nil {
		return fmt.Errorf("emit(IR): check propagation outside a function")
	}
	rets := c.irCurFunc.Rets
	switch len(rets) {
	case 0:
		return fmt.Errorf("emit(IR): check propagation in a function without an Err result")
	case 1:
		c.irDeferRun(ind)
		c.line("%sreturn %s;", ind, c.irVal(errVal))
		return nil
	}
	name, err := c.irRetCT(c.irCurFunc)
	if err != nil {
		return err
	}
	tmp := c.tmp("prop")
	c.line("%s%s %s;", ind, name, tmp)
	c.line("%smemset(&%s, 0, sizeof(%s));", ind, tmp, tmp)
	c.line("%s%s._%d = %s;", ind, tmp, len(rets)-1, c.irVal(errVal))
	c.irDeferRun(ind)
	c.line("%sreturn %s;", ind, tmp)
	return nil
}

// locFile 取指令/语句的文件（空 = 当前文件）。
func (c *Ctx) locFile(loc air.Loc) string {
	if loc.File == "" {
		return c.Path
	}
	return loc.File
}

func locLine(loc air.Loc) int {
	if loc.Line <= 0 {
		return 1
	}
	return loc.Line
}

// ---------------------------------------------------------------------------
// 叶子指令
// ---------------------------------------------------------------------------

func (c *Ctx) irLeaf(in air.Inst, ind string) error {
	switch v := in.(type) {
	case *air.Let:
		return c.irLet(v, ind)
	case *air.Var:
		return c.irVar(v, ind)
	case *air.Store:
		return c.irStore(v, ind)
	case *air.RegionEnter, *air.RegionExit:
		// region 的作用域在 CIR 层已折成 Region 语句（成对出现），叶子位的
		// enter/exit 只在跨块形态里出现 —— 那种形态现阶段明确拒绝（foldInsts 已先报）。
		return nil
	case *air.DeferReg:
		return nil // 注册点的实参在随后的 DeferInit 里；这里只是标记
	case *air.DeferInit:
		return c.irDeferInit(v, ind)
	case *air.DeferRun:
		c.irDeferRun(ind)
		return nil
	case *air.Trap:
		c.srcLine(v.Loc.Pos(c.Path))
		c.line("%saic_trap(%s, %s, %d);", ind, v.Code, cstr(c.locFile(v.Loc)), locLine(v.Loc))
		return nil
	case *air.TrapIfErr:
		return fmt.Errorf("emit(IR): trap.if.err is not emitted yet (line %d)", v.Line)
	case *air.Spawn:
		return c.irSpawn(v, ind)
	case *air.SelWait:
		return c.irSelWait(v, ind)
	}
	return fmt.Errorf("emit(IR): unsupported instruction %T", in)
}

// irSpawn 发一次 spawn：实参装进**类型化**上下文块 + 登记薄 thunk（定义在
// TU 末尾、原型在原型段）+ aic_scope_spawn。thunk 与被调符号的解析都按 IR 给
// 的东西来（不查 AST）；块字段带真实类型，比 void* 槽的往返更直白。
func (c *Ctx) irSpawn(v *air.Spawn, ind string) error {
	if c.irScopeCur == "" {
		return fmt.Errorf("emit(IR): spawn outside a scope (line %d)", v.Loc.Line)
	}
	callee, err := c.irCallee(v.Callee, v.TypeArgs)
	if err != nil {
		return err
	}
	ctxName := fmt.Sprintf("aic_spctx_%d", c.irSpawnSeq)
	trName := fmt.Sprintf("aic_spth_%d", c.irSpawnSeq)
	c.irSpawnSeq++
	// 实参类型先收集：上下文块的**定义**在文件作用域（原型段，第二遍按第一遍的
	// 登记发），这里只发使用点（块内声明变量 + 填字段 + spawn 调用）。
	// 曾经把结构体定义发在函数体内 —— thunk 在 TU 末尾，看不见它（742/773 实测
	// "storage size isn't known"）。
	argTys := make([]string, 0, len(v.Vals))
	for i, val := range v.Vals {
		ty := c.irValTy(val)
		if ty == "" || ty == "void" {
			return fmt.Errorf("emit(IR): spawn argument %d has no known type (line %d)", i, v.Loc.Line)
		}
		argTys = append(argTys, ty)
	}
	tmp := fmt.Sprintf("aic_sp_%d", c.irSpawnSeq)
	c.srcLine(v.Loc.Pos(c.Path))
	// 上下文块必须走 **aic_task_env（堆）**：运行时的任务出口会 free(env)
	// （aic_l2.c 的 aic_task_entry 尾部），栈变量 = 堆损坏（0xC0000374，742/773 实测）。
	c.line("%sstruct %s *%s = (struct %s *)aic_task_env(sizeof(struct %s));", ind, ctxName, tmp, ctxName, ctxName)
	for i, val := range v.Vals {
		init, err := c.irSlotVal(val, argTys[i])
		if err != nil {
			return err
		}
		c.line("%s%s->f%d = %s;", ind, tmp, i, init)
	}
	c.need("l2")
	// aic_scope_spawn 的 ABI = (scope*, thunk, env) 三参（runtime/aic_l2.h）——
	// 没有 file/line 形参（多传两个 = C 直接拒）。
	c.line("%saic_scope_spawn(&%s, %s, %s);", ind, c.irScopeCur, trName, tmp)
	c.irSpawns = append(c.irSpawns, irSpawnSite{trName: trName, ctxName: ctxName, callee: callee, nargs: len(v.Vals), argTys: argTys})
	return nil
}

// irSpawnSite 是一个已登记的 spawn 站点（上下文块 + thunk 的名字与被调符号）。
type irSpawnSite struct {
	trName  string
	ctxName string
	callee  string
	nargs   int
	argTys  []string
}

// emitIRSpawnCtxStructs 发 spawn 上下文块的**定义**（文件作用域、原型段）：
// 使用点（块内 var）与 thunk（TU 末尾）都要求在它们之前见到完整定义。
func (c *Ctx) emitIRSpawnCtxStructs() error {
	for _, s := range c.irSpawns {
		fields := make([]string, 0, len(s.argTys))
		for i, ty := range s.argTys {
			decl, err := c.irDeclText(ty, fmt.Sprintf("f%d", i))
			if err != nil {
				return err
			}
			fields = append(fields, decl+";")
		}
		c.line("struct %s { %s };", s.ctxName, strings.Join(fields, " "))
	}
	if len(c.irSpawns) > 0 {
		c.line("")
	}
	return nil
}

// emitIRSpawnProtos 发 spawn thunk 的原型（原型段：C 要求先声明后使用）。
// 站点表来自第一遍（seed）——本遍的原型段跑在函数体之前，站点还没被发现。
func (c *Ctx) emitIRSpawnProtos() {
	for _, s := range c.irSpawns {
		c.line("static void %s(void *);", s.trName)
	}
	if len(c.irSpawns) > 0 {
		c.line("")
	}
}

// emitIRSpawnBodies 发 spawn thunk 的定义（函数体段之后：取上下文块字段直调）。
func (c *Ctx) emitIRSpawnBodies() {
	for _, s := range c.irSpawns {
		args := make([]string, 0, s.nargs)
		for i := 0; i < s.nargs; i++ {
			args = append(args, fmt.Sprintf("c__->f%d", i))
		}
		c.line("static void %s(void *raw) {", s.trName)
		c.line("    struct %s *c__ = (struct %s *)raw;", s.ctxName, s.ctxName)
		c.line("    (void)(%s(%s));", s.callee, strings.Join(args, ", "))
		c.line("}")
		c.line("")
	}
}

func (c *Ctx) irLet(v *air.Let, ind string) error {
	c.irPendingTy, c.irPendingLoc = v.Ty, v.Loc
	// **chan recv 的原生结果结构 → AIR 多值结构**：两者布局逐位相同（_0/_1），
	// 但 C 不允许结构体间强转 —— tcc 当扩展收下、gcc/clang 直接拒
	// （742 实测：五配置的 H3 在 gcc 配置下是潜伏的坑）。故拆两条语句：
	// 先接原生值，再按字段逐一搬进目标结构（无 double-eval：原生值先落临时量）。
	if nativeTy, ok := c.irChanRecvNativeTy(v.Rhs); ok {
		if elems, isMulti := splitMultiText(v.Ty); isMulti {
			expr, err := c.irRHS(v.Rhs)
			c.irPendingTy, c.irPendingLoc = "", air.Loc{}
			if err != nil {
				return err
			}
			tmp := c.tmp("cr")
			c.srcLine(v.Loc.Pos(c.Path))
			c.line("%s%s %s = %s;", ind, nativeTy, tmp, expr)
			fields := make([]string, 0, len(elems))
			for i := range elems {
				fields = append(fields, fmt.Sprintf("._%d = %s._%d", i, tmp, i))
			}
			ct, err := c.irDeclText(v.Ty, v.Tmp)
			if err != nil {
				return err
			}
			// 目标结构的 C 名 = irRetNameOf（合成返回结构的那张表），**不是
			// AIR 类型文本**（`(i32, bool)` 不是 C 类型名，773 实测）。
			stName, err := c.irRetNameOf(elems)
			if err != nil {
				return err
			}
			c.line("%s%s = ((%s){ %s });", ind, ct, stName, strings.Join(fields, ", "))
			c.irTemps[v.Tmp] = v.Ty
			return nil
		}
	}
	rhs, err := c.irRHS(v.Rhs)
	c.irPendingTy, c.irPendingLoc = "", air.Loc{}
	if err != nil {
		return err
	}
	if v.Ty == "void" || v.Ty == "" {
		c.srcLine(v.Loc.Pos(c.Path))
		c.line("%s%s;", ind, rhs)
		c.irTemps[v.Tmp] = "void"
		return nil
	}
	ct, err := c.irDeclText(v.Ty, v.Tmp)
	if err != nil {
		return err
	}
	c.srcLine(v.Loc.Pos(c.Path))
	c.line("%s%s = %s;", ind, ct, rhs)
	c.irTemps[v.Tmp] = v.Ty
	return nil
}

func (c *Ctx) irVar(v *air.Var, ind string) error {
	cname := c.irBind(v.Name, v.Ty)
	ct, err := c.irDeclText(v.Ty, cname)
	if err != nil {
		return err
	}
	c.irTemps[v.Name] = v.Ty
	c.srcLine(v.Loc.Pos(c.Path))
	if v.Init != "" {
		// 定长数组是值类型但 C 里不可赋值：整体初始化走 memcpy。
		// **声明必须先发**（C 的数组声明把名字写在中间，只有 irDeclText 会写对）——
		// 曾经这里只发 memcpy，`a` 从未声明过，语料 730/715/784 全部编译失败。
		if t, ok := c.irTab[v.Ty]; ok && types.IsArray(t) {
			c.line("%s%s;", ind, ct)
			c.line("%smemcpy((void *)%s, (const void *)%s, sizeof(%s));", ind, cname, c.irVal(v.Init), cname)
			return nil
		}
		// 初值按本变量的类型求值（Option 零值/字面量定型都靠它）——与 irLet 同口径。
		savedTy, savedLoc := c.irPendingTy, c.irPendingLoc
		c.irPendingTy, c.irPendingLoc = v.Ty, v.Loc
		init, err := c.irSlotVal(v.Init, v.Ty)
		c.irPendingTy, c.irPendingLoc = savedTy, savedLoc
		if err != nil {
			return err
		}
		c.line("%s%s = %s;", ind, ct, init)
		return nil
	}
	zero, err := c.irZero(v.Ty)
	if err != nil {
		return err
	}
	c.line("%s%s = %s;", ind, ct, zero)
	return nil
}

// irChanRecvNativeTy 报告 let 的右值是不是 chan recv 调用，是则给出**运行时原生
// 结果结构**的 C 名（`aic_chan_<S>_recv_t`；布局与 AIR 的多值结构逐位相同，
// 但类型不同，强转非法）。判据只认符号形态（与 irChanCall 同一张解析）。
func (c *Ctx) irChanRecvNativeTy(r air.RHS) (string, bool) {
	call, ok := r.(*air.Call)
	if !ok {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(call.Sym, "chan."), "chan_")
	parts := strings.Split(rest, "_")
	// 与 irChanCall 同一张解析：op 可以在前（recv_i32）也可以在后（i32_recv）。
	suf := ""
	switch {
	case len(parts) == 2 && parts[0] == "recv":
		suf = parts[1]
	case len(parts) == 2 && parts[1] == "recv":
		suf = parts[0]
	default:
		return "", false
	}
	return "aic_chan_" + suf + "_recv_t", true
}

// irZero 是某类型的 C 零值（核心设计 §二.3）。
func (c *Ctx) irZero(text string) (string, error) {
	t, ok := c.irTab[text]
	if !ok {
		if text == "void" || text == "" {
			return "", fmt.Errorf("emit(IR): a void variable cannot be initialised")
		}
		return "", fmt.Errorf("emit(IR): no zero value for the type text %q", text)
	}
	if types.IsArray(t) {
		// 数组不能整体赋值/转换：零值是初始化器 `{0}`（只在定义处合法）。
		return "{0}", nil
	}
	// **Option 实例**是结构体（{tag, union}）：零值 = None 标签，不是标量 0
	// （`o = 0` 在 C 里直接非法；`func Pick(n) -> Option[i32]` 的返回槽
	// 走这里，generic 语料实测）。
	if _, isOpt := types.IsOptionInstance(t); isOpt {
		ct := c.cTypeName(t)
		_, none := optionTags(ct)
		return fmt.Sprintf("((%s){ .tag = %s })", ct, none), nil
	}
	return c.zeroValue(t), nil
}

func (c *Ctx) irStore(v *air.Store, ind string) error {
	// **list 的元素写有"增长"语义**（`xs[i] = v` 在 i == len 时追加，越界才是 trap）：
	// 必须走运行时的 set（热路径一次比较，冷路径按需增长），不能走 `ref` ——
	// `*(aic_list_ref_…(xs,i)) = v` 在 i == len 时直接 trap（018/023/695 实测：
	// 本该打印 "list ok" 的程序死在 "index out of bounds"）。
	// 定长数组/bytes 不是这条语义：数组走下标 + AIR 守卫，bytes 走 aic_list_set_u8。
	if _, handled, err := c.irListElemStore(v, ind); handled || err != nil {
		return err
	}
	place, err := c.irPlace(v.Place)
	if err != nil {
		return err
	}
	val, err := c.irSlotVal(v.Val, c.irPlaceTy(v.Place))
	if err != nil {
		return err
	}
	c.srcLine(v.Loc.Pos(c.Path))
	// 守卫是 store 的修饰符（AIR 里就没有"独立守卫指令"这种形态）：
	// 先判后存，违反 = trap（运行时给位置与修复建议）。
	if v.Limit != "" {
		if err := c.irGuard(v, val, place, ind); err != nil {
			return err
		}
	}
	c.line("%s%s = %s;", ind, place, val)
	return nil
}

// irListElemStore 把 `store elem <list>, i, v` 发成运行时的 set（带增长语义）。
// 非要写的宿主是 list（类型文本以 `[]` 结尾）才接管；其余返回 handled=false。
func (c *Ctx) irListElemStore(v *air.Store, ind string) (string, bool, error) {
	ep, ok := v.Place.(*air.ElemPlace)
	if !ok {
		return "", false, nil
	}
	host := c.irPlaceTy(ep.Base)
	if !strings.HasSuffix(host, "[]") {
		return "", false, nil
	}
	if v.Limit != "" {
		return "", false, nil // 带守卫的存储点：守卫语义优先（数组/区域路径）
	}
	elem := strings.TrimSuffix(host, "[]")
	suf, err := c.irSuffixOfText(elem)
	if err != nil {
		return "", true, err
	}
	base, err := c.irPlace(ep.Base)
	if err != nil {
		return "", true, err
	}
	idx := c.irVal(ep.Idx)
	val, err := c.irSlotVal(v.Val, elem)
	if err != nil {
		return "", true, err
	}
	c.need("l1")
	c.srcLine(v.Loc.Pos(c.Path))
	c.line("%saic_list_set_%s(%s, %s, %s, %s, %d);", ind, suf, base, idx, val,
		cstr(c.locFile(v.Loc)), locLine(v.Loc))
	return "", true, nil
}

// irGuard 发射一次存储点守卫（§五 R3）。limit 的受限文法见 air.Store。
func (c *Ctx) irGuard(v *air.Store, val, place, ind string) error {
	file := cstr(c.locFile(v.Loc))
	line := locLine(v.Loc)
	host, _ := c.irHostBase(v.Place)
	return c.irGuardLimit(v.Limit, val, place, ind, file, line, host)
}

// irGuardLimit 发一次存储点守卫（limit 文法 = fnentry｜depth｜depth-N｜host）。
// Store 的守卫与 DeferInit 实参的注册点守卫共用同一张翻译表（§五 R3/R5）——
// 高阶部分只换"被守的值"与"宿主是什么"。hostExpr = 宿主对象的 C 表达式
// （host 限制定理要读**宿主区域**的深度；空 = 拿不到宿主，按当前深度兜底）。
func (c *Ctx) irGuardLimit(limit, val, what, ind, file string, line int, hostExpr string) error {
	switch {
	case limit == "fnentry":
		c.guards.Kept++
		c.guards.Return++
		c.line("%sAIC_GUARD(%s, 0u, %s, %d); /* 返回点: 界 ≤ 入口 */", ind, val, file, line)
		return nil
	case limit == "depth":
		c.guards.Kept++
		c.guards.Store++
		c.line("%sAIC_GUARD(%s, aic_depth, %s, %d); /* 存储点 %s */", ind, val, file, line, what)
		return nil
	case strings.HasPrefix(limit, "depth-"):
		c.guards.Kept++
		c.guards.Store++
		c.line("%sAIC_GUARD(%s, (aic_depth - %su), %s, %d); /* 存储点 %s */",
			ind, val, strings.TrimPrefix(limit, "depth-"), file, line, what)
		return nil
	case limit == "host":
		c.guards.Kept++
		c.guards.Store++
		if hostExpr == "" {
			// 宿主是形参派生但存储点不在对象内部（形参槽本身）：界就是当前深度。
			c.line("%sAIC_GUARD(%s, aic_depth, %s, %d); /* 存储点 %s */", ind, val, file, line, what)
			return nil
		}
		c.line("%sAIC_GUARD(%s, aic_region_depth_of(%s->hdr.reg), %s, %d); /* 存储点 %s */",
			ind, val, hostExpr, file, line, what)
		return nil
	}
	return fmt.Errorf("emit(IR): unknown guard limit %q", limit)
}

// irHostBase 取存储点的宿主对象表达式（字段/下标存的宿主 = 该 place 的基座）。
func (c *Ctx) irHostBase(p air.Place) (string, bool) {
	switch v := p.(type) {
	case *air.FieldPlace:
		s, err := c.irPlace(v.Base)
		return s, err == nil
	case *air.ElemPlace:
		s, err := c.irPlace(v.Base)
		return s, err == nil
	}
	return "", false
}

func (c *Ctx) irSelWait(v *air.SelWait, ind string) error {
	arms := make([]string, 0, len(v.Chans))
	for i, ch := range v.Chans {
		kind := "AIC_SEL_RECV"
		if i < len(v.Sends) && v.Sends[i] {
			kind = "AIC_SEL_SEND"
		}
		arms = append(arms, fmt.Sprintf("{ %s, %s }", kind, c.irVal(ch)))
	}
	deadline := "0"
	if v.Deadline != "" {
		deadline = c.irVal(v.Deadline)
	}
	c.need("l2")
	c.srcLine(v.Loc.Pos(c.Path))
	c.line("%s%s = aic_select_wait((aic_sel_arm[]){ %s }, %du, %s, %s, %d);", ind, v.Tmp,
		strings.Join(arms, ", "), len(arms), deadline, cstr(c.locFile(v.Loc)), locLine(v.Loc))
	c.irTemps[v.Tmp] = "i32"
	return nil
}

// ---------------------------------------------------------------------------
// defer：IR 三件套 → 运行时 defer 栈 + 薄 trampoline
// ---------------------------------------------------------------------------

// irDeferState 是一个函数的 defer 发射状态。
type irDeferState struct {
	// sites 按注册点编号（IR 的 DeferInit.ID）索引。
	sites map[int]*air.DeferInit
	// stack 是 C 侧的 defer 栈变量名（无注册点 = 空）。
	stack string
	// hasErr 报告有没有 errdefer（收尾块据此决定要不要判 Err）。
	hasErr bool
}

// irDeferSites 收集一个函数体里的全部 defer 注册点（确定性：按 ID 排序）。
func (c *Ctx) irDeferSites(f *cir.Func) (*irDeferState, error) {
	st := &irDeferState{sites: map[int]*air.DeferInit{}}
	var walk func([]cir.Stmt)
	walk = func(list []cir.Stmt) {
		for _, s := range list {
			switch v := s.(type) {
			case *cir.Leaf:
				if di, ok := v.In.(*air.DeferInit); ok {
					st.sites[di.ID] = di
					if di.OnErr {
						st.hasErr = true
					}
				}
			case *cir.If:
				walk(v.Then)
				walk(v.Else)
			case *cir.For:
				walk(v.Head)
				walk(v.Body)
				walk(v.Post)
			case *cir.Switch:
				for _, cs := range v.Cases {
					walk(cs.Body)
				}
			case *cir.Region:
				walk(v.Body)
			}
		}
	}
	walk(f.Body)
	if len(st.sites) > 0 {
		st.stack = "aic_dfs_" + sanitize(c.irCurSym)
	}
	return st, nil
}

func (c *Ctx) irDeferState() *irDeferState {
	if c.irCurFunc == nil {
		return nil
	}
	return c.irCurDefer
}

// irEmitDeferTrampolines 为每个注册点发上下文结构体 + 薄 trampoline（在函数之前）。
func (c *Ctx) irEmitDeferTrampolines(f *cir.Func, st *irDeferState) error {
	if st == nil || len(st.sites) == 0 {
		return nil
	}
	ids := make([]int, 0, len(st.sites))
	for id := range st.sites {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	fn := c.irCurSym
	for _, id := range ids {
		di := st.sites[id]
		ctxName := fmt.Sprintf("aic_dfctx_%s_%d", sanitize(fn), id)
		trName := fmt.Sprintf("aic_df_%s_%d", sanitize(fn), id)
		// 上下文结构体：字段类型 = 实参类型（含接收者当首实参）。
		fields := make([]string, 0, len(di.Vals))
		for i, val := range di.Vals {
			ty := c.irValTy(val)
			if ty == "" || ty == "void" {
				return fmt.Errorf("emit(IR): defer site %d in %s: argument %d has no known type", id, fn, i)
			}
			decl, err := c.irDeclText(ty, fmt.Sprintf("f%d", i))
			if err != nil {
				return err
			}
			fields = append(fields, decl+";")
		}
		c.line("struct %s { %s };", ctxName, strings.Join(fields, " "))
		c.line("static void %s(void *ctx) {", trName)
		c.line("    struct %s *c__ = (struct %s *)ctx;", ctxName, ctxName)
		args := make([]string, 0, len(di.Vals))
		// 注意：这里**不能**用 defer 恢复（defer 在循环里要等函数返回才跑，
		// 第二次迭代拿到的就是上一轮清空后的空表 —— 多 defer 站点的函数会
		// 报"实参没有类型"）。逐轮显式恢复。
		savedTemps := c.irTemps
		c.irTemps = map[string]string{}
		for i, val := range di.Vals {
			ty := savedTemps[val]
			if strings.HasPrefix(val, "const ") {
				ty = types.LiteralTy(strings.TrimPrefix(val, "const "))
			}
			if ty == "" || ty == "void" {
				c.irTemps = savedTemps
				return fmt.Errorf("emit(IR): defer site %d in %s: argument %d has no known type", id, fn, i)
			}
			name := fmt.Sprintf("a%d", i)
			decl, err := c.irDeclText(ty, name)
			if err != nil {
				c.irTemps = savedTemps
				return err
			}
			c.line("    %s = c__->f%d;", decl, i)
			c.irTemps[name] = ty
			args = append(args, name)
		}
		// 被调者可能是运行时/内建（`defer print.println(x)`）：走同一条内建翻译，
		// 否则会去 mangling 表里找一个不存在的 print_println。
		call := ""
		if s, ok, err := c.irBuiltinCall(di.Callee, args, args, di.Loc); ok || err != nil {
			if err != nil {
				return err
			}
			call = s
		} else {
			callee, err := c.irCallee(di.Callee, di.TypeArgs)
			if err != nil {
				return err
			}
			call = fmt.Sprintf("%s(%s)", callee, strings.Join(args, ", "))
		}
		c.line("    (void)(%s);", call)
		c.line("}")
		c.line("")
		c.irTemps = savedTemps
	}
	return nil
}

func (c *Ctx) irDeferInit(v *air.DeferInit, ind string) error {
	st := c.irDeferState()
	if st == nil || st.stack == "" {
		return fmt.Errorf("emit(IR): defer.init outside a deferring function")
	}
	// 被调者的解析在 trampoline 里做（那里才能把内建/运行时调用一起处理）；
	// 注册点只负责把实参装进上下文。
	fn := c.irCurSym
	ctxName := fmt.Sprintf("aic_dfctx_%s_%d", sanitize(fn), v.ID)
	trName := fmt.Sprintf("aic_df_%s_%d", sanitize(fn), v.ID)
	tmp := fmt.Sprintf("aic_dc_%s_%d", sanitize(fn), v.ID)
	onerr := 0
	if v.OnErr {
		onerr = 1
	}
	c.srcLine(v.Loc.Pos(c.Path))
	c.line("%sstruct %s %s;", ind, ctxName, tmp)
	for i, val := range v.Vals {
		init := c.irVal(val)
		// 注册点守卫（§五 R5：defer 实参的界 ≤ 函数入口）：先守后存 ——
		// 块内对象作实参时必须在那一点就 trap，不能等退出时读已弹出的区域（781）。
		if i < len(v.Limits) && v.Limits[i] != "" {
			if err := c.irGuardLimit(v.Limits[i], init, "defer arg "+strconv.Itoa(i), ind,
				cstr(c.locFile(v.Loc)), locLine(v.Loc), ""); err != nil {
				return err
			}
		}
		c.line("%s%s.f%d = %s;", ind, tmp, i, init)
	}
	c.line("%saic_defer_push_copy_ex(&%s, %s, &%s, sizeof(%s), %d, %s, %d);",
		ind, st.stack, trName, tmp, tmp, onerr, cstr(c.locFile(v.Loc)), locLine(v.Loc))
	return nil
}

// irDeferRun 在出口跑 defer 栈（errdefer 项按返回的 Err 决定跑不跑）。
func (c *Ctx) irDeferRun(ind string) {
	st := c.irDeferState()
	if st == nil || st.stack == "" {
		return
	}
	if st.hasErr && c.irCurFunc != nil && len(c.irCurFunc.Rets) > 0 {
		last := c.irCurFunc.Rets[len(c.irCurFunc.Rets)-1]
		if last == "Err" {
			c.line("%sAIC_DEFER_RUN_ERR(&%s, %s.code);", ind, st.stack,
				fmt.Sprintf("ret%d", len(c.irCurFunc.Rets)-1))
			return
		}
	}
	c.line("%sAIC_DEFER_RUN(&%s);", ind, st.stack)
}

// ---------------------------------------------------------------------------
// 右值与位置
// ---------------------------------------------------------------------------

func (c *Ctx) irRHS(r air.RHS) (string, error) {
	switch v := r.(type) {
	case *air.Const:
		// 裸 `None` 在**有类型的槽位**上 = Option 零值构造（形态取决于实例，
		// 706 实测：实参位不给定目标类型就发不出 C）。irPendingTy 就是 let/var
		// 的槽位类型；不是 Option 实例时仍按普通常量走（由调用方兜疵）。
		if v.Lit == "None" && c.irPendingTy != "" {
			if t, ok := c.irTab[c.irPendingTy]; ok {
				if _, isOpt := types.IsOptionInstance(t); isOpt {
					return c.irOptionCtor("None", nil)
				}
			}
		}
		return c.irConst(v.Lit), nil
	case *air.TmpRef:
		// 临时量的值：可能是字面量（`tmp const "ab"`：基座物化的常见形态，
		// 113 实测直接返回原名会把 `const ` 前缀漏进 C），故按值路径转。
		return c.irVal(v.Name), nil
	case *air.VarRef:
		return c.irVal(v.Name), nil
	case *air.Nil:
		zero, err := c.irZero(v.Ty)
		if err != nil {
			return "", err
		}
		return zero, nil
	case *air.Binop:
		// str 没有 C 运算符（aic_str 是结构）：`str + str` = aic_str_concat。
		// 两侧都必须是 str —— 只看一侧会在同名遮蔽（`x` 在一处是 str、另一处是 usize）
		// 时把 `i + 1` 误发成 concat（693 语料实测）。
		if v.Op == "+" && c.irValTy(v.A) == "str" && c.irValTy(v.B) == "str" {
			return fmt.Sprintf("aic_str_concat(%s, %s, %d)", c.parenVal(v.A), c.parenVal(v.B), locLine(c.irPendingLoc)), nil
		}
		// 整数除法/取模的除零 = trap（§一：C 里是 UB；浮点除零 = IEEE inf，不查）。
		// 形态 = 内联比较 + cold 不返回报告（§10.4 第 1 条）；除数双侧求值安全
		// （AIR 的值名恒无副作用，V 系规则）。108/109 锚点。
		if (v.Op == "/" || v.Op == "%") && irIsIntText(c.irValTy(v.B)) {
			return fmt.Sprintf("(%s == 0 ? (aic_trap(AIC_TRAP_DIVIDE_BY_ZERO, %s, %d), 0) : %s %s %s)",
				c.parenVal(v.B), cstr(c.locFile(c.irPendingLoc)), locLine(c.irPendingLoc),
				c.parenVal(v.A), v.Op, c.parenVal(v.B)), nil
		}
		// 移位计数越界 = trap（§一：计数 ≥ 左操作数位宽或有符号负计数在 C 里都是 UB；
		// 此前静默算出 0 —— "静默算错"级缺陷，778 锚点）。形态同上：内联比较 + cold。
		if v.Op == "<<" || v.Op == ">>" {
			if w, ok := irIntWidth(c.irValTy(v.A)); ok {
				return fmt.Sprintf("((%s) < 0 || (%s) >= %d ? (aic_trap(AIC_TRAP_SHIFT_RANGE, %s, %d), 0) : %s %s %s)",
					c.parenVal(v.B), c.parenVal(v.B), w,
					cstr(c.locFile(c.irPendingLoc)), locLine(c.irPendingLoc),
					c.parenVal(v.A), v.Op, c.parenVal(v.B)), nil
			}
		}
		if v.Op == "==" || v.Op == "!=" {
			if c.irValTy(v.A) == "str" || c.irValTy(v.B) == "str" {
				eq := fmt.Sprintf("aic_str_eq(%s, %s)", c.parenVal(v.A), c.parenVal(v.B))
				if v.Op == "!=" {
					return "(!" + eq + ")", nil
				}
				return eq, nil
			}
		}
		return fmt.Sprintf("%s %s %s", c.parenVal(v.A), v.Op, c.parenVal(v.B)), nil
	case *air.Cmp:
		return c.irCmp(v)
	case *air.Unop:
		return fmt.Sprintf("%s%s", v.Op, c.parenVal(v.A)), nil
	case *air.Conv:
		return c.irConv(v)
	case *air.Call:
		return c.irCallExpr(v)
	case *air.CallInd:
		return c.irCallInd(v)
	case *air.Closure:
		// 纯 lambda（无捕获）= 静态函数指针：值就是被提升函数的 C 名。
		// 按值捕获的闭包值 = {fn, env} 两字结构（N1），随 756 一起做 —— 现在
		// 明确报错而不是发半截（红线 23）。
		if len(v.Env) > 0 {
			return "", fmt.Errorf("emit(IR): capturing closures are not emitted yet (N1 `closure %s` captures %d values)", v.Sym, len(v.Env))
		}
		name, ok, err := c.irFuncRefCName(v.Sym)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("emit(IR): the lifted lambda %q has no C function (the checker and the IR disagree)", v.Sym)
		}
		return name, nil
	case *air.CallClosure:
		// 闭包值调用：C 的函数指针调用就是 `f(args)`（纯形态）。捕获形态同理
		// 在 Closure 处已被拒。
		args := make([]string, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, c.irVal(a))
		}
		return fmt.Sprintf("%s(%s)", c.irVal(v.Val), strings.Join(args, ", ")), nil
	case *air.Alloc:
		return c.irAlloc(v)
	case *air.SizeOf:
		ct, err := c.irCText(v.Ty)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("sizeof(%s)", ct), nil
	case *air.FieldRHS:
		return c.irField(v)
	case *air.ElemRHS:
		return c.irElem(v)
	case *air.LenRHS:
		return c.irLen(v)
	case *air.StrViewRHS:
		return c.irStrView(v)
	case *air.AddrRHS:
		// 取地址（defer 块形按引用捕获的注册点）：C 的 `&x`。
		// 空值名 = 内部不一致，直接报错而不是发 `&`。
		if v.Val == "" {
			return "", fmt.Errorf("emit(IR): addr needs a value")
		}
		return "&" + c.irVal(v.Val), nil
	case *air.MultiExtract:
		return fmt.Sprintf("%s._%d", v.Val, v.Idx), nil
	case *air.EnumTag:
		return c.irEnumTag(v)
	case *air.EnumPayload:
		return c.irEnumPayload(v)
	case *air.Box:
		return c.irBox(v)
	case *air.Witness:
		return "", fmt.Errorf("emit(IR): a witness table reference is not a value")
	}
	return "", fmt.Errorf("emit(IR): unsupported rhs %T", r)
}

// parenVal 取一个 AIR 操作数的 C 表达式。
// **必须走 irVal/irName**：AIR 里的操作数是值名，而值名要按当前 C 作用域解析
// （同名遮蔽 → `i` / `i__1`）。曾经这里对非字面量直接原样返回，于是
// `cmp`/`binop`/`unop` 的操作数**绕过别名表**：第二个 `for i in 0..n` 的
// 条件与体里写的是外层那个 `i`（`i < n` 恒假 ⇒ 循环一次都不跑；751/749 实测）。
func (c *Ctx) parenVal(v string) string { return c.irVal(v) }

// irPackedClassOf 取一个值名的 @packed 类（值类型的比较要走每类一份的比较助手）。
func (c *Ctx) irPackedClassOf(val string) (*types.Class, bool) {
	t, ok := c.irTab[c.irValTy(val)]
	if !ok {
		return nil, false
	}
	if cl, isCl := types.IsClass(t); isCl && cl.Packed {
		return cl, true
	}
	return nil, false
}

func (c *Ctx) irCmp(v *air.Cmp) (string, error) {
	// AIR 的比较算符有两种拼写（`eq`/`==`，闭集见 §一）：先归一，再判 str。
	// （踩过的坑：只认 `eq` 拼写时 str 分支永远不命中，于是发出 `a == b` 给 C。）
	op := v.Op
	switch op {
	case "eq":
		op = "=="
	case "ne":
		op = "!="
	case "lt":
		op = "<"
	case "le":
		op = "<="
	case "gt":
		op = ">"
	case "ge":
		op = ">="
	}
	a, b := c.parenVal(v.A), c.parenVal(v.B)
	// @packed 类是**值类型**（C 结构），`==`/`<` 在 C 里没有运算符 ⇒ 走每类一份的
	// 比较助手（@derive(Compare) 的方法或按字段序生成的助手，见 packed.go）。
	// 曾经 IR 路径直接发 `p == q`，C 报 "invalid operands to binary == (have
	// 'aic_ok_Pair' and 'aic_ok_Pair')"（776 实测）。
	if cl, ok := c.irPackedClassOf(v.A); ok {
		call := c.packedCompareCall(cl, a, b)
		switch op {
		case "==":
			return fmt.Sprintf("(%s == 0)", call), nil
		case "!=":
			return fmt.Sprintf("(%s != 0)", call), nil
		case "<", "<=", ">", ">=":
			return fmt.Sprintf("(%s %s 0)", call, op), nil
		}
	}
	// str 的比较没有 C 运算符（aic_str 是结构）：走运行时的字节序比较。
	if c.irValTy(v.A) == "str" || c.irValTy(v.B) == "str" {
		switch op {
		case "==":
			return fmt.Sprintf("aic_str_eq(%s, %s)", a, b), nil
		case "!=":
			return fmt.Sprintf("(!aic_str_eq(%s, %s))", a, b), nil
		case "<", "<=", ">", ">=":
			return fmt.Sprintf("(aic_str_cmp(%s, %s) %s 0)", a, b, op), nil
		}
	}
	// 与 nil 的比较：nil 在 AIR 里是裸值名，形态由**对方**的类型决定。
	if v.A == "nil" || v.B == "nil" {
		other, neg := v.A, v.B
		if v.A == "nil" {
			other = v.B
		} else {
			neg = v.A
		}
		_ = neg
		ev := c.parenVal(other)
		ty := c.irValTy(other)
		isEq := op == "=="
		switch {
		case ty == "Err":
			if isEq {
				return fmt.Sprintf("(%s.code == 0)", ev), nil
			}
			return fmt.Sprintf("(%s.code != 0)", ev), nil
		case ty == "str":
			if isEq {
				return fmt.Sprintf("(%s.len == 0)", ev), nil
			}
			return fmt.Sprintf("(%s.len != 0)", ev), nil
		default:
			if t, ok := c.irTab[ty]; ok {
				if _, isIfc := types.IsInterface(t); isIfc {
					if isEq {
						return fmt.Sprintf("(%s.data == NULL)", ev), nil
					}
					return fmt.Sprintf("(%s.data != NULL)", ev), nil
				}
			}
			if isEq {
				return fmt.Sprintf("(%s == NULL)", ev), nil
			}
			return fmt.Sprintf("(%s != NULL)", ev), nil
		}
	}
	return fmt.Sprintf("%s %s %s", a, op, b), nil
}

// irConv 发射显式转换：窄化要 trap（§一：转换是显式且可检的），拓宽直接转。
func (c *Ctx) irConv(v *air.Conv) (string, error) {
	return "", fmt.Errorf("emit(IR): conversions are not emitted yet (%s)", v.Kind)
}

func (c *Ctx) irAlloc(v *air.Alloc) (string, error) {
	// AIR 的 Alloc.Ty 是**对象类型**（普通类按约定不带引用后缀 `*`），而 irTab 的键
	// 是语义类型文本（普通类带 `*`）⇒ 先按原样查，查不到按约定补 `*` 再查
	// （740 实测：`ok_Box[i32]` 直接查表必然落空）。
	t, ok := c.irTab[v.Ty]
	if !ok && !strings.HasSuffix(v.Ty, "*") {
		t, ok = c.irTab[v.Ty+"*"]
	}
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the allocated type %q", v.Ty)
	}
	base := c.cTypeName(t)
	// **@packed 类是值类型**：没有堆对象可分配，`alloc` 在这里的语义就是"造一个值"，
	// 发零值复合字面量（后续 `store field` 就地写字段）。曾经这里无条件发指针转换，
	// `aic_ok_Pair obj6 = (aic_ok_Pair *)aic_alloc_hdr(…)` 被 C 直接拒绝
	// （invalid initializer；731/733 实测）。判据 = 类的 Packed 位（与类型定义同源；
	// 泛型实例的 Packed 位在**基类**上 —— 实例本身不是 Class）。
	if cl, isCl := classOfType(t); isCl && cl.Packed {
		return fmt.Sprintf("((%s){0})", base), nil
	}
	elem, ptr := base, base
	if strings.HasSuffix(base, "*") {
		elem = strings.TrimSuffix(base, "*")
	} else {
		ptr = base + " *"
	}
	size := fmt.Sprintf("sizeof(%s)", elem)
	line := c.irPendingLoc.Line
	if line <= 0 {
		line = 1
	}
	switch v.Kind {
	case "live":
		c.need("l0")
		return fmt.Sprintf("(%s)aic_alloc_live(%s, %du)", ptr, size, uint32(line)), nil
	case "task", "region", "":
		return fmt.Sprintf("(%s)aic_alloc_hdr(%s, %du)", ptr, size, uint32(line)), nil
	}
	return "", fmt.Errorf("emit(IR): unknown allocation kind %q", v.Kind)
}

func (c *Ctx) irField(v *air.FieldRHS) (string, error) {
	// 变体引用 `Color.Red`：AIR 里是"对**类型名**取第 idx 个成员"（基座不是值名）。
	if vp, isVar := v.Place.(*air.VarPlace); isVar && c.irTemps[vp.Name] == "" {
		if s, ok, err := c.irVariantOfType(vp.Name, v.Idx); ok || err != nil {
			return s, err
		}
	}
	base, err := c.irPlace(v.Place)
	if err != nil {
		return "", err
	}
	name, err := c.irFieldName(v.Place, v.Idx)
	if err != nil {
		return "", err
	}
	// 字段访问的算符取决于宿主是**引用**（类指针 → `->`）还是**值**
	// （Err / @packed / 元组 → `.`）。读取与写入共用同一判据（irArrow）。
	return c.irHostCast(base, v.Place) + c.irArrow(v.Place) + name, nil
}

// irArrow 是字段访问算符：宿主是引用（类指针）→ `->`，是值（@packed 类 / Err /
// 元组 / 合成多返回结构）→ `.`。**读取（irField）与写入（irPlace）必须同源** ——
// 写侧曾经无条件发 `->`，@packed 类的字段写就直接非法
// （`obj6->a = 7`：invalid type argument of '->'；731/733 实测）。
func (c *Ctx) irArrow(host air.Place) string {
	if strings.HasSuffix(c.irPlaceTy(host), "*") {
		return "->"
	}
	return "."
}

// irHostCast 给"存储槽是 `void *`"的宿主补一次按语义类型的指针转换。
// 列表元素槽对引用类型就是 `void *`（runtime `AIC_LIST_SLOW_DECL(Box, void *)`），
// 于是 `*aic_list_ref_Box(…)` 的类型是 `void *`；字段访问/成员读写必须在具体
// 类型上做（否则 C 报 "request for member 'n' in something not a structure"，
// 789 实测）。判据 = 元素后缀是 Box（RuntimeSuffix 的唯一定义）。
func (c *Ctx) irHostCast(base string, p air.Place) string {
	ep, isElem := p.(*air.ElemPlace)
	if !isElem {
		return base
	}
	host := c.irPlaceTy(ep.Base)
	if !strings.HasSuffix(host, "[]") {
		return base
	}
	elem := strings.TrimSuffix(host, "[]")
	if !strings.HasSuffix(elem, "*") {
		return base
	}
	if suf, err := c.irSuffixOfText(elem); err != nil || suf != "Box" {
		return base
	}
	ct, err := c.irCText(c.irPlaceTy(p))
	if err != nil || ct == "" || strings.TrimSpace(ct) == "void *" {
		return base
	}
	return "((" + ct + ")" + base + ")"
}

// irVariantOfType 发一个变体引用（`Color.Red` → C 枚举常量）。
// 无负载枚举 = enum 常量；带负载的枚举不能只写变体名（语言层也不允许）。
func (c *Ctx) irVariantOfType(typeText string, idx int) (string, bool, error) {
	t, ok := c.irTab[typeText]
	if !ok {
		// 文本可能是"本包名省略"的形态：按各包补一次。
		for p := range c.deps {
			if tt, hit := c.irTab[air.TypeSym(p, typeText)]; hit {
				t, ok = tt, true
				break
			}
		}
	}
	if !ok {
		return "", false, nil
	}
	en, isEn := types.IsEnum(t)
	if !isEn {
		return "", false, nil
	}
	if idx < 0 || idx >= len(en.Variants) {
		return "", true, fmt.Errorf("emit(IR): enum %s has no variant with tag %d", en.Name, idx)
	}
	va := en.Variants[idx]
	if va.Payload != nil {
		return "", true, fmt.Errorf("emit(IR): the variant %s.%s carries a payload and needs a constructor call", en.Name, va.Name)
	}
	// 无数据变体的**值**：无数据枚举就是 C 枚举常量；带数据枚举的 C 值类型是
	// `{tag, union}` 结构，必须补成结构字面量（直接写 tag 常量会被 C 拒绝：
	// `aic_ok_Shape t = aic_ok_Shape_Dot;` = "invalid initializer"）。判据与
	// 类型定义同源（en.HasData / variantNoData，emit 内唯一实现）。
	return c.variantNoData(en, c.cTypeName(en), va.Name), true, nil
}

// irVariantCall 发一个变体构造调用（`Shape_Circle(3)` → 复合字面量）。
// 符号形态 = `<类型文本>_<变体名>`（AIR 侧由降级器拼出）。
func (c *Ctx) irVariantCall(sym string, args []string) (string, bool, error) {
	en, va, ok := c.findVariantBySym(sym)
	if !ok {
		return "", false, nil
	}
	name := c.cTypeName(en)
	if va.Payload == nil {
		if len(args) != 0 {
			return "", true, fmt.Errorf("emit(IR): the variant %s takes no payload", sym)
		}
		return c.variantNoData(en, name, va.Name), true, nil
	}
	if len(args) != 1 {
		return "", true, fmt.Errorf("emit(IR): the variant %s takes exactly one payload", sym)
	}
	return fmt.Sprintf("((%s){ .tag = %s_%s, .u.%s = %s })", name, name, va.Name, va.Name, args[0]), true, nil
}

// findVariantBySym 在全部包的枚举里按符号找变体（`<pkg>_<Enum>_<Variant>` 或省略包名）。
func (c *Ctx) findVariantBySym(sym string) (*types.Enum, types.Variant, bool) {
	pkgs := make([]string, 0, len(c.deps))
	for p := range c.deps {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		info := c.deps[p]
		names := make([]string, 0, len(info.Enums))
		for n := range info.Enums {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			en := info.Enums[n]
			for _, va := range en.Variants {
				if sym == air.TypeSym(p, en.Name)+"_"+va.Name || sym == en.Name+"_"+va.Name {
					return en, va, true
				}
			}
		}
	}
	return nil, types.Variant{}, false
}

// classOf 按类型文本取背后的类（泛型实例走基类；@packed 位在基类上）。
func (c *Ctx) classOf(text string) (*types.Class, bool) {
	t, ok := c.irTab[text]
	if !ok {
		return nil, false
	}
	return classOfType(t)
}

// classOfType 取语义类型背后的类（Instance → 基类）。
func classOfType(t types.Type) (*types.Class, bool) {
	if cl, isCl := types.IsClass(t); isCl {
		return cl, true
	}
	if inst, isInst := t.(*types.Instance); isInst {
		if cl, isCl := types.IsClass(inst.Base); isCl {
			return cl, true
		}
	}
	return nil, false
}

// fieldTypeOfText 取字段在语义层的类型（泛型实例按实参代换后的那种）。
func fieldTypeOfText(cl *types.Class, args []types.Type, idx int) (types.Type, bool) {
	if cl == nil || idx < 0 || idx >= len(cl.Fields) {
		return nil, false
	}
	ft := cl.Fields[idx].Type
	if len(args) > 0 && len(cl.TypeParams) == len(args) {
		ft = types.Subst(ft, cl.TypeParams, args)
	}
	return ft, true
}

// irFieldName 取字段的 C 名：具名类 = 字段名（ABI 冻结，声明序下标 → 名字），
// 元组/多返回结构 = `_<下标>`。
func (c *Ctx) irFieldName(p air.Place, idx int) (string, error) {
	host := c.irPlaceTy(p)
	if cl, ok := c.classOf(host); ok {
		if idx >= 0 && idx < len(cl.Fields) {
			return cl.Fields[idx].Name, nil
		}
		return "", fmt.Errorf("emit(IR): field %d is out of range for %s", idx, cl.Name)
	}
	if strings.HasPrefix(host, "Err") || host == "Err" {
		switch idx {
		case 0:
			return "code", nil
		case 1:
			return "msg", nil
		case 2:
			return "cause", nil
		}
	}
	// 元组/多返回结构：字段名就是 `_<下标>`（与 retStructNameFor 的发射同源）。
	return fmt.Sprintf("_%d", idx), nil
}

// irPlaceTy 取一个 place 的 AIR 类型文本。
func (c *Ctx) irPlaceTy(p air.Place) string {
	switch v := p.(type) {
	case *air.VarPlace:
		if strings.HasPrefix(v.Name, "const ") {
			// 字面量宿主（`len "hey"` / `"hey"[i]`）：类型按 §一 的最窄规则定。
			return types.LiteralTy(strings.TrimPrefix(v.Name, "const "))
		}
		return c.irTemps[v.Name]
	case *air.FieldPlace:
		base := strings.TrimSuffix(c.irPlaceTy(v.Base), "*")
		// Err 的字段是**冻结下标**（code/msg/cause，runtime aic_l0.h 的 aic_Err）：
		// cause 在 C 里是 `const aic_Err *` ⇒ 它的类型文本必须带回 `*`，否则链式
		// `e.cause.msg` 会被发成 `.` 而不是 `->`（791 实测）。
		if base == "Err" {
			switch v.Idx {
			case 0:
				return "i32"
			case 1:
				return "str"
			case 2:
				return "Err*"
			}
			return ""
		}
		if t, ok := c.irTab[base]; ok {
			if cl, isCl := types.IsClass(t); isCl {
				if ft, has := fieldTypeOfText(cl, nil, v.Idx); has {
					return air.TyText(ft)
				}
			}
			if inst, isInst := t.(*types.Instance); isInst {
				if cl, isCl := types.IsClass(inst.Base); isCl {
					if ft, has := fieldTypeOfText(cl, inst.Args, v.Idx); has {
						return air.TyText(ft)
					}
				}
			}
		}
		return ""
	case *air.ElemPlace:
		base := c.irPlaceTy(v.Base)
		if strings.HasSuffix(base, "[]") {
			return strings.TrimSuffix(base, "[]")
		}
		if strings.HasPrefix(base, "[") {
			if i := strings.IndexByte(base, ';'); i > 0 {
				return base[1:i]
			}
		}
		if base == "str" || base == "bytes" {
			return "u8"
		}
		return ""
	}
	return ""
}

func (c *Ctx) irElem(v *air.ElemRHS) (string, error) {
	// AIR 的**读**形态有两种写法：位置可以是"元素位置"（`elem elem xs, j, j`，
	// 位置里的下标与 RHS 的下标同值），也可以直接是宿主（`elem xs, j`）。
	inner, idx := v.Place, v.Idx
	if ep, ok := v.Place.(*air.ElemPlace); ok {
		inner = ep.Base
		if idx == "" {
			idx = ep.Idx
		}
	}
	base, err := c.irPlace(inner)
	if err != nil {
		return "", err
	}
	return c.irElemRead(inner, base, c.irVal(idx))
}

// irElemRead 按宿主类型发射一次元素读。
func (c *Ctx) irElemRead(hostPlace air.Place, base, idx string) (string, error) {
	host := c.irPlaceTy(hostPlace)
	switch {
	case strings.HasSuffix(host, "[]"):
		elem := strings.TrimSuffix(host, "[]")
		suf, err := c.irSuffixOfText(elem)
		if err != nil {
			return "", err
		}
		c.need("l1")
		return fmt.Sprintf("aic_list_get_%s(%s, %s, %s, 0)", suf, base, idx, cstr(c.Path)), nil
	case strings.HasPrefix(host, "["):
		// 定长数组：下标带越界 trap（AIC_ARRAY_AT：内联比较 + cold 报告；
		// 左值形态 ⇒ 读写同一表达式，004/112 锚点）。长度来自类型文本常量。
		if n, ok := arrayLenOfText(host); ok {
			return fmt.Sprintf("AIC_ARRAY_AT(%s, %s, %s, %s, 0)", base, strconv.Itoa(n), idx, cstr(c.Path)), nil
		}
		return fmt.Sprintf("%s[%s]", base, idx), nil
	case host == "str":
		// str 的字节 = aic_str_at（带越界 trap；bytes 才是 list_u8 槽位，见下一分支）。
		c.need("l1")
		return fmt.Sprintf("aic_str_at(%s, %s, %s, 0)", base, idx, cstr(c.Path)), nil
	case host == "bytes":
		// str/bytes 的下标 = 第 i 个字节（字节语义，§十四）。
		c.need("l1")
		return fmt.Sprintf("aic_list_get_u8((aic_list_u8 *)%s, %s, %s, 0)", base, idx, cstr(c.Path)), nil
	case strings.HasPrefix(host, "set["):
		// 集合按插入序取第 i 个（`for x in s` 的降级形态）。
		suf, err := c.irContainerSuffixOf(host)
		if err != nil {
			return "", err
		}
		c.need("l1")
		return fmt.Sprintf("aic_set_at_%s(%s, %s, %s, 0)", suf, base, idx, cstr(c.Path)), nil
	}
	return "", fmt.Errorf("emit(IR): element read on the host type %q is not covered", host)
}

// irContainerSuffixOf 取一个容器类型文本的运行时短后缀（list/set/chan/bytes）。
func (c *Ctx) irContainerSuffixOf(text string) (string, error) {
	t, ok := c.irTab[text]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the container type %q", text)
	}
	switch {
	case types.IsSet(t):
		return c.irSuffixOfType(types.SetElem(t), text)
	case types.IsSlice(t):
		return c.irSuffixOfType(types.SliceElem(t), text)
	}
	if ch, isCh := t.(*types.ChanT); isCh {
		return c.irSuffixOfType(ch.Elem, text)
	}
	return "", fmt.Errorf("emit(IR): %q is not a container type", text)
}

func (c *Ctx) irLen(v *air.LenRHS) (string, error) {
	base, err := c.irPlace(v.Place)
	if err != nil {
		return "", err
	}
	host := c.irPlaceTy(v.Place)
	switch {
	case strings.HasSuffix(host, "[]"):
		elem := strings.TrimSuffix(host, "[]")
		t, ok := c.irTab[elem]
		if !ok {
			return "", fmt.Errorf("emit(IR): no C mapping for the element type %q", elem)
		}
		suf, ok := types.RuntimeSuffix(t)
		if !ok {
			return "", fmt.Errorf("emit(IR): the runtime has no list instance for the element type %q", elem)
		}
		c.need("l1")
		return fmt.Sprintf("aic_list_len_%s(%s)", suf, base), nil
	case strings.HasPrefix(host, "["):
		// 定长数组：长度是编译期常量。
		if i := strings.IndexByte(host, ';'); i > 0 {
			return strings.TrimSuffix(host[i+1:], "]"), nil
		}
	case host == "str":
		// str 的长度是 aic_str 结构体的字段（运行时没有 aic_str_len 函数）：
		// 发调用会在 C 层"隐式声明"（025 实测）。
		return fmt.Sprintf("(%s).len", base), nil
	case host == "bytes":
		c.need("l1")
		return fmt.Sprintf("aic_bytes_len(%s)", base), nil
	case strings.HasPrefix(host, "map["):
		name, _, ok := c.irMapParts(host)
		if !ok {
			return "", fmt.Errorf("emit(IR): no runtime map instance for %q", host)
		}
		// 运行时的拼法是 `aic_map_len_<KS>_<VS>`（len 在前，与 put/get 的
		// `aic_map_put_<KS>_<VS>` 不同）—— 照 irMapCall 的同一张表拆，别自己拼。
		kvs := strings.TrimPrefix(name, "aic_map_")
		c.need("l1")
		return fmt.Sprintf("aic_map_len_%s(%s)", kvs, base), nil
	case strings.HasPrefix(host, "set["):
		t, ok := c.irTab[host]
		if !ok {
			return "", fmt.Errorf("emit(IR): no C mapping for %q", host)
		}
		suf, ok := types.RuntimeSuffix(types.SetElem(t))
		if !ok {
			return "", fmt.Errorf("emit(IR): the runtime has no set instance for %q", host)
		}
		c.need("l1")
		return fmt.Sprintf("aic_set_len_%s(%s)", suf, base), nil
	}
	return "", fmt.Errorf("emit(IR): length of the host type %q is not covered", host)
}

// irMapParts 取 `map[K]V` 的运行时函数名前缀与键值类型。
func (c *Ctx) irMapParts(host string) (string, types.Type, bool) {
	t, ok := c.irTab[host]
	if !ok {
		return "", nil, false
	}
	k, v, ok := types.MapParts(t)
	if !ok {
		return "", nil, false
	}
	ks, ok1 := types.RuntimeMapKeySuffix(k)
	vs, ok2 := types.RuntimeMapValueSuffix(v)
	if !ok1 || !ok2 {
		return "", nil, false
	}
	return "aic_map_" + ks + "_" + vs, t, true
}

// irEnumShape 取一个枚举值名的（枚举对象, 是否带数据）。
func (c *Ctx) irEnumShape(val string) (*types.Enum, bool, error) {
	ty := c.irValTy(val)
	en, err := c.irEnumOfText(ty)
	if err != nil {
		return nil, false, err
	}
	hasData := false
	for _, va := range en.Variants {
		if va.Payload != nil {
			hasData = true
			break
		}
	}
	return en, hasData, nil
}

// irEnumOfText 取一个类型文本对应的枚举对象（泛型实例/选项实例要拆到基座）。
func (c *Ctx) irEnumOfText(text string) (*types.Enum, error) {
	t, ok := c.irTab[text]
	if !ok {
		return nil, fmt.Errorf("emit(IR): no C mapping for the enum type %q", text)
	}
	if en, isEn := types.IsEnum(t); isEn {
		return en, nil
	}
	if inst, isInst := t.(*types.Instance); isInst {
		if en, isEn := types.IsEnum(inst.Base); isEn {
			return en, nil
		}
	}
	return nil, fmt.Errorf("emit(IR): %q is not an enum type", text)
}

// irEnumTag 发射枚举判别式：**带数据**的枚举是 {tag, union} 结构（读 `.tag`），
// 无数据的枚举在 C 层就是普通 enum（值本身就是 tag）。
func (c *Ctx) irEnumTag(v *air.EnumTag) (string, error) {
	_, hasData, err := c.irEnumShape(v.Val)
	if err != nil {
		return "", err
	}
	if !hasData {
		return v.Val, nil
	}
	return fmt.Sprintf("%s.tag", v.Val), nil
}

func (c *Ctx) irEnumPayload(v *air.EnumPayload) (string, error) {
	val := v.Val
	en, _, err := c.irEnumShape(val)
	if err != nil {
		return "", err
	}
	if v.Idx >= 0 && v.Idx < len(en.Variants) {
		return fmt.Sprintf("%s.u.%s", val, en.Variants[v.Idx].Name), nil
	}
	return "", fmt.Errorf("emit(IR): enum %s has no variant with tag %d", en.Name, v.Idx)
}

func (c *Ctx) irBox(v *air.Box) (string, error) {
	cl, err := c.irClassOfVal(v.Val)
	if err != nil {
		return "", err
	}
	ifc, ok := c.irTab[v.Iface]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the interface %q", v.Iface)
	}
	iface, ok := types.IsInterface(ifc)
	if !ok {
		return "", fmt.Errorf("emit(IR): %q is not an interface", v.Iface)
	}
	name, err := c.witnessFor(cl, iface)
	if err != nil {
		return "", err
	}
	raw := c.irVal(v.Val)
	if cl.Packed {
		ct := c.cTypeName(cl)
		tmp := c.tmp("box")
		c.line("    %s *%s = (%s *)aic_alloc_hdr(sizeof(%s), 0u);", ct, tmp, ct, ct)
		c.line("    *%s = %s;", tmp, raw)
		return fmt.Sprintf("((aic_iface){ .data = (void *)%s, .vt = (const void *const *)%s })", tmp, name), nil
	}
	return fmt.Sprintf("((aic_iface){ .data = (void *)(%s), .vt = (const void *const *)%s })", raw, name), nil
}

// irClassOfVal 取一个值名的具体类（装箱需要 concrete class）。
func (c *Ctx) irClassOfVal(val string) (*types.Class, error) {
	ty := c.irValTy(val)
	t, ok := c.irTab[ty]
	if !ok {
		return nil, fmt.Errorf("emit(IR): no C mapping for the boxed type %q", ty)
	}
	cl, isCl := types.IsClass(t)
	if !isCl {
		if inst, isInst := t.(*types.Instance); isInst {
			if base, isBase := types.IsClass(inst.Base); isBase {
				return base, nil
			}
		}
		return nil, fmt.Errorf("emit(IR): only a class value can be boxed into an interface (%s)", ty)
	}
	return cl, nil
}

// irCallInd 发射接口方法调用：`((R (*)(aic_iface, …))(r).vt[slot])((r), args…)`。
func (c *Ctx) irCallInd(v *air.CallInd) (string, error) {
	recv := c.irVal(v.Recv)
	tmp := c.tmp("ifc")
	c.line("    aic_iface %s = %s;", tmp, recv)
	c.line("    AIC_NONNULL(%s.data, %s, 0);", tmp, cstr(c.Path))
	params := []string{"aic_iface"}
	args := []string{tmp}
	for _, a := range v.Args {
		ty := c.irValTy(a)
		ct, err := c.irCText(ty)
		if err != nil {
			return "", err
		}
		params = append(params, ct)
		args = append(args, c.irVal(a))
	}
	// 返回类型由调用点的临时量给出（IR 里 let 的类型就是结果类型）。
	ret := "void"
	if c.irPendingTy != "" {
		ct, err := c.irCText(c.irPendingTy)
		if err != nil {
			return "", err
		}
		ret = ct
	}
	return fmt.Sprintf("((%s (*)(%s))(%s).vt[%d])(%s)",
		ret, strings.Join(params, ", "), tmp, v.Slot, strings.Join(args, ", ")), nil
}

// irCallExpr 发射一次直接调用：先看是不是运行时/内建，再按用户函数发。
func (c *Ctx) irCallExpr(v *air.Call) (string, error) {
	args := make([]string, 0, len(v.Args))
	for _, a := range v.Args {
		// 裸 `None` 作为**实参**时目标类型不可知（零值形态取决于它是哪个 Option 实例）：
		// 明确报错，不猜。
		if a == "const None" {
			return "", fmt.Errorf("emit(IR): a bare `None` argument needs a target type (call %s)", v.Sym)
		}
		args = append(args, c.irVal(a))
	}
	if s, ok, err := c.irBuiltinCall(v.Sym, args, v.Args, c.irPendingLoc); ok || err != nil {
		return s, err
	}
	if s, ok, err := c.irVariantCall(v.Sym, args); ok || err != nil {
		return s, err
	}
	name, err := c.irCallee(v.Sym, v.TypeArgs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(args, ", ")), nil
}

// irPrintln 发射 print.println(x)：按实参类型选打印原语（printgen.go 的唯一实现）。
func (c *Ctx) irPrintln(arg string) (string, error) {
	if strings.HasPrefix(arg, "const nil") || arg == "nil" {
		return "(aic_pr_nil(), aic_print_nl())", nil
	}
	tyText := c.irValTy(arg)
	if tyText == "" {
		return "", fmt.Errorf("emit(IR): the type of the println argument %q is unknown", arg)
	}
	t, ok := c.irTab[tyText]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the printed type %q", tyText)
	}
	pv, err := c.printValue(c.irVal(arg), t)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s, aic_print_nl())", pv), nil
}

// ---------------------------------------------------------------------------
// 内建与运行时调用表
//
// IR 里的内建调用符号继承的是**源码拼写**（`list.append[i32]` / `str_indexOf` /
// `math_abs`），而运行时的 C 名是 `<组>_<操作>_<短后缀>`。这张表就是两种拼写的
// 翻译，逐条对齐 runtime/*.h 的 ABI（参数里的 `file`/`line` 是运行期 trap 的定位，
// 缺了它们 trap 消息就没有位置 —— 语料锁着那些文本）。
//
// 机械家族（list/set/map/chan/array）由后缀+操作拼出；单例家族（str/math/bytes/…）
// 逐条列出。**未覆盖的一律报错**，不做"看着像就发"的猜测。
// ---------------------------------------------------------------------------

// irBuiltinCall 把内建/运行时调用翻成完整 C 表达式。
// ok=false = 不是内建（调用方走用户函数/实例符号）。
func (c *Ctx) irBuiltinCall(sym string, args []string, vals []string, loc air.Loc) (string, bool, error) {
	file := cstr(c.locFile(loc))
	ln := locLine(loc)

	switch sym {
	case "print_println":
		if len(args) != 1 {
			return "", true, fmt.Errorf("emit(IR): println takes exactly one value")
		}
		s, err := c.irPrintln(vals[0])
		return s, true, err
	case "Err":
		if len(args) != 2 {
			return "", true, fmt.Errorf("emit(IR): Err takes (code, msg)")
		}
		return fmt.Sprintf("((aic_Err){ %s, %s, NULL })", args[0], args[1]), true, nil
	case "Err_wrap":
		if len(args) != 3 {
			return "", true, fmt.Errorf("emit(IR): Err.wrap takes (code, msg, cause)")
		}
		return fmt.Sprintf("aic_err_wrap(%s, %s, %s)", args[0], args[1], args[2]), true, nil
	case "Some", "None":
		s, err := c.irOptionCtor(sym, args)
		return s, true, err
	case "sync_Mutex_new":
		c.need("l2")
		return "((aic_mutex){ 0 })", true, nil
	case "sync_Mutex_lock", "sync_Mutex_unlock":
		if len(args) != 1 {
			return "", true, fmt.Errorf("emit(IR): %s takes the mutex as its only argument", sym)
		}
		c.need("l2")
		op := strings.TrimPrefix(sym, "sync_Mutex_")
		return fmt.Sprintf("aic_mutex_%s(&%s)", op, args[0]), true, nil
	case "time_after":
		c.need("l2")
		return fmt.Sprintf("aic_time_deadline(%s)", args[0]), true, nil
	case "scope_timeout":
		c.need("l2")
		return fmt.Sprintf("aic_scope_timeout(%s)", args[0]), true, nil
	}
	// 基础类型的显式转换 `i32(x)`：C 的强制转换（窄化的 trap 由 irConv 承担）。
	if ct, ok := BasicCName[sym]; ok && len(args) == 1 {
		return fmt.Sprintf("((%s)%s)", ct, args[0]), true, nil
	}
	switch {
	case strings.HasPrefix(sym, "list."), strings.HasPrefix(sym, "list_"):
		s, err := c.irListCall(sym, args, vals, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "set."), strings.HasPrefix(sym, "set_"):
		s, err := c.irSetCall(sym, args, vals, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "map."), strings.HasPrefix(sym, "map_"):
		s, err := c.irMapCall(sym, args, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "chan."), strings.HasPrefix(sym, "chan_"):
		s, err := c.irChanCall(sym, args, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "array_"):
		s, err := c.irArrayCall(sym, args)
		return s, true, err
	case strings.HasPrefix(sym, "bytes_"):
		s, err := c.irBytesCall(sym, args, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "os_"):
		s, err := c.irOSCall(sym, args, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "str_"):
		s, err := c.irStrCall(sym, args, file, ln)
		return s, true, err
	case strings.HasPrefix(sym, "math_"):
		s, err := c.irMathCall(sym, args)
		return s, true, err
	case strings.HasPrefix(sym, "option_"):
		s, err := c.irOptionCall(sym, args)
		return s, true, err
	case sym == "ctx_new" || strings.HasPrefix(sym, "ctx_"):
		return "", true, fmt.Errorf("emit(IR): the `ctx` package has no runtime implementation yet (%s)", sym)
	case sym == "opaque_compare":
		// N3：Ord 能力约束在**标量/str** 上的 compare = 语言级三态比较
		//（`[T: Ord]` 的 a.compare(b)，T = i32 时无用户方法可调）。
		// str 走运行时的 aic_str_cmp（字节序 + 长度序）；数值发 C 三元式
		//（< 与 > 对同一对值各求值一次是安全的：两侧都是纯读）。
		if len(args) != 2 {
			return "", true, fmt.Errorf("emit(IR): opaque_compare takes the receiver and one argument")
		}
		if c.irValTy(vals[0]) == "str" {
			c.need("l0")
			return fmt.Sprintf("aic_str_cmp(%s, %s)", args[0], args[1]), true, nil
		}
		return fmt.Sprintf("((%s) < (%s) ? -1 : ((%s) > (%s) ? 1 : 0))",
			args[0], args[1], args[0], args[1]), true, nil
	}
	return "", false, nil
}

// irOptionCtor 发射 Some(x) / None（目标类型 = 当前 let 的类型）。
func (c *Ctx) irOptionCtor(sym string, args []string) (string, error) {
	ty, err := c.irOptionCT()
	if err != nil {
		return "", err
	}
	some, none := optionTags(ty)
	if sym == "None" {
		return fmt.Sprintf("((%s){ .tag = %s })", ty, none), nil
	}
	if len(args) != 1 {
		return "", fmt.Errorf("emit(IR): Some takes exactly one value")
	}
	return fmt.Sprintf("((%s){ .tag = %s, .u.Some = %s })", ty, some, args[0]), nil
}

// irOptionCT 取当前表达式的 Option 实例 C 类型名（选项值的构造需要它）。
func (c *Ctx) irOptionCT() (string, error) {
	if c.irPendingTy == "" {
		return "", fmt.Errorf("emit(IR): an option value needs its target type but the context gave none")
	}
	t, ok := c.irTab[c.irPendingTy]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the option type %q", c.irPendingTy)
	}
	if _, isOpt := types.IsOptionInstance(t); !isOpt {
		return "", fmt.Errorf("emit(IR): %q is not an Option type", c.irPendingTy)
	}
	return c.cTypeName(t), nil
}

// irOptionCall 发射 option.ok/none/isSome/isNone/unwrap。
func (c *Ctx) irOptionCall(sym string, args []string) (string, error) {
	fn := strings.TrimPrefix(sym, "option_")
	switch fn {
	case "ok":
		return c.irOptionCtor("Some", args)
	case "none":
		ty, err := c.irOptionCT()
		if err != nil {
			return "", err
		}
		_, none := optionTags(ty)
		return fmt.Sprintf("((%s){ .tag = %s })", ty, none), nil
	case "isSome", "isNone":
		if len(args) != 1 {
			return "", fmt.Errorf("emit(IR): option.%s takes one value", fn)
		}
		op := "== 0"
		if fn == "isNone" {
			op = "!= 0"
		}
		return fmt.Sprintf("((%s).tag %s)", args[0], op), nil
	case "unwrap":
		if len(args) != 1 {
			return "", fmt.Errorf("emit(IR): option.unwrap takes one value")
		}
		return fmt.Sprintf("((%s).u.Some)", args[0]), nil
	}
	return "", fmt.Errorf("emit(IR): unsupported option.%s", fn)
}

// irListCall 发射 list 家族（list.<op>[T] / list_<S>_<op>）。
func (c *Ctx) irListCall(sym string, args []string, vals []string, file string, ln int) (string, error) {
	recvTy := ""
	if len(vals) > 0 {
		recvTy = c.irValTy(vals[0])
	}
	op, suf, err := c.irContainerOp(sym, "list", recvTy)
	if err != nil {
		return "", err
	}
	c.need("l1")
	recv := argAt(args, 0)
	switch op {
	case "new":
		return fmt.Sprintf("aic_list_new_%s(%du)", suf, uint32(ln)), nil
	case "len":
		return fmt.Sprintf("aic_list_len_%s(%s)", suf, recv), nil
	case "isEmpty":
		return fmt.Sprintf("aic_list_isempty_%s(%s)", suf, recv), nil
	case "append":
		return fmt.Sprintf("aic_list_append_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "insert":
		return fmt.Sprintf("aic_list_insert_%s(%s, %s, %s, %s, %d)", suf, recv, argAt(args, 1), argAt(args, 2), file, ln), nil
	case "remove":
		return fmt.Sprintf("aic_list_remove_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "clear":
		return fmt.Sprintf("aic_list_clear_%s(%s, %s, %d)", suf, recv, file, ln), nil
	}
	return "", fmt.Errorf("emit(IR): list.%s is not covered by the IR backend", op)
}

// irSetCall 发射 set 家族（set.<op>_<S> / set_<S>_<op>）。
func (c *Ctx) irSetCall(sym string, args []string, vals []string, file string, ln int) (string, error) {
	recvTy := ""
	if len(vals) > 0 {
		recvTy = c.irValTy(vals[0])
	}
	op, suf, err := c.irContainerOp(sym, "set", recvTy)
	if err != nil {
		return "", err
	}
	c.need("l1")
	recv := argAt(args, 0)
	switch op {
	case "new":
		return fmt.Sprintf("aic_set_new_%s(%du)", suf, uint32(ln)), nil
	case "len":
		return fmt.Sprintf("aic_set_len_%s(%s)", suf, recv), nil
	case "isEmpty":
		return fmt.Sprintf("aic_set_isempty_%s(%s)", suf, recv), nil
	case "add":
		return fmt.Sprintf("aic_set_add_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "has":
		return fmt.Sprintf("aic_set_has_%s(%s, %s)", suf, recv, argAt(args, 1)), nil
	case "remove":
		return fmt.Sprintf("aic_set_remove_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "at":
		return fmt.Sprintf("aic_set_at_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "clear":
		return fmt.Sprintf("aic_set_clear_%s(%s, %s, %d)", suf, recv, file, ln), nil
	}
	return "", fmt.Errorf("emit(IR): set.%s is not covered by the IR backend", op)
}

// irMapCall 发射 map 家族（map_<K>_<V>_<op> / map_new_<K>_<V>）。
func (c *Ctx) irMapCall(sym string, args []string, file string, ln int) (string, error) {
	rest := strings.TrimPrefix(strings.TrimPrefix(sym, "map."), "map_")
	parts := strings.Split(rest, "_")
	if len(parts) < 2 {
		return "", fmt.Errorf("emit(IR): malformed map symbol %q", sym)
	}
	if parts[0] == "new" {
		kvs := strings.Join(parts[1:], "_")
		c.need("l1")
		return fmt.Sprintf("aic_map_new_%s(%du)", kvs, uint32(ln)), nil
	}
	op := parts[len(parts)-1]
	kvs := strings.Join(parts[:len(parts)-1], "_")
	c.need("l1")
	recv := argAt(args, 0)
	switch op {
	case "len":
		return fmt.Sprintf("aic_map_len_%s(%s)", kvs, recv), nil
	case "isEmpty":
		return fmt.Sprintf("aic_map_isempty_%s(%s)", kvs, recv), nil
	case "put":
		return fmt.Sprintf("aic_map_put_%s(%s, %s, %s, %s, %d)", kvs, recv, argAt(args, 1), argAt(args, 2), file, ln), nil
	case "get":
		return fmt.Sprintf("aic_map_get_%s(%s, %s)", kvs, recv, argAt(args, 1)), nil
	case "has":
		return fmt.Sprintf("aic_map_has_%s(%s, %s)", kvs, recv, argAt(args, 1)), nil
	case "remove":
		return fmt.Sprintf("aic_map_remove_%s(%s, %s, %s, %d)", kvs, recv, argAt(args, 1), file, ln), nil
	case "keyAt":
		return fmt.Sprintf("aic_map_key_at_%s(%s, %s, %s, %d)", kvs, recv, argAt(args, 1), file, ln), nil
	case "valAt":
		return fmt.Sprintf("aic_map_val_at_%s(%s, %s, %s, %d)", kvs, recv, argAt(args, 1), file, ln), nil
	case "clear":
		return fmt.Sprintf("aic_map_clear_%s(%s, %s, %d)", kvs, recv, file, ln), nil
	}
	return "", fmt.Errorf("emit(IR): map.%s is not covered by the IR backend", op)
}

// irChanCall 发射 chan 家族（chan_new_<S> / chan_<S>_<op> / chan_<op>_<S>）。
func (c *Ctx) irChanCall(sym string, args []string, file string, ln int) (string, error) {
	rest := strings.TrimPrefix(strings.TrimPrefix(sym, "chan."), "chan_")
	parts := strings.Split(rest, "_")
	if len(parts) < 1 {
		return "", fmt.Errorf("emit(IR): malformed chan symbol %q", sym)
	}
	c.need("l2")
	ops := map[string]bool{"send": true, "recv": true, "len": true, "cap": true, "ready": true, "canSend": true}
	op, suf := "", ""
	switch {
	case parts[0] == "new":
		suf = strings.Join(parts[1:], "_")
		cap := "0"
		if len(args) == 1 {
			cap = args[0]
		}
		return fmt.Sprintf("aic_chan_new_%s(%s, %du)", suf, cap, uint32(ln)), nil
	case len(parts) == 2 && ops[parts[0]]:
		op, suf = parts[0], parts[1]
	case len(parts) == 2 && ops[parts[1]]:
		suf, op = parts[0], parts[1]
	default:
		return "", fmt.Errorf("emit(IR): malformed chan symbol %q", sym)
	}
	recv := argAt(args, 0)
	switch op {
	case "send":
		return fmt.Sprintf("aic_chan_send_%s(%s, %s, %s, %d)", suf, recv, argAt(args, 1), file, ln), nil
	case "recv":
		// **直接发原生调用**：结果结构（aic_chan_<S>_recv_t）与 AIR 多值结构的
		// 转换在 irLet 处按字段做（两条语句）。此前这里发结构体强转，tcc 收、
		// gcc/clang 拒（742 的 H3 潜伏坑）。
		return fmt.Sprintf("aic_chan_recv_%s(%s, %s, %d)", suf, recv, file, ln), nil
	case "len":
		return fmt.Sprintf("aic_chan_len_%s(%s)", suf, recv), nil
	case "cap":
		return fmt.Sprintf("aic_chan_cap_%s(%s)", suf, recv), nil
	case "ready":
		return fmt.Sprintf("aic_chan_ready_%s(%s)", suf, recv), nil
	case "canSend":
		return fmt.Sprintf("aic_chan_can_send_%s(%s)", suf, recv), nil
	}
	return "", fmt.Errorf("emit(IR): chan.%s is not covered by the IR backend", op)
}

// irArrayCall 发射定长数组的内建（长度 = 编译期常量）。
//
// 符号里带的是**元素类型**后缀（`array_i32_len`），长度要从不变量本身取 ——
// 实参的类型文本就是 `[i32;3]`。曾经的实现把元素后缀当长度文本去解析，
// `array_i32_len` 于是恒发 `0u`：`a.len()` = 0、`for i < a.len()` 一轮都不跑
// （715/716/032 实测的"定长数组全错"就是这个）。
func (c *Ctx) irArrayCall(sym string, args []string) (string, error) {
	op := ""
	if i := strings.LastIndex(sym, "_"); i > 0 {
		op = sym[i+1:]
	}
	text := c.irValTy(argAt(args, 0))
	n, ok := arrayLenOfText(text)
	if !ok {
		return "", fmt.Errorf("emit(IR): %s needs a fixed-size array value (got %q)", sym, text)
	}
	switch op {
	case "len":
		return fmt.Sprintf("%du", n), nil
	case "isEmpty":
		return fmt.Sprintf("(%d == 0)", n), nil
	}
	return "", fmt.Errorf("emit(IR): array.%s is not covered by the IR backend", op)
}

// irBytesCall 发射 bytes 家族（构造 = aic_bytes_*，元素操作 = aic_list_u8_*）。
func (c *Ctx) irBytesCall(sym string, args []string, file string, ln int) (string, error) {
	c.need("l1")
	recv := argAt(args, 0)
	switch sym {
	case "bytes_new":
		return fmt.Sprintf("aic_bytes_new(%du)", uint32(ln)), nil
	case "bytes_withCap":
		return fmt.Sprintf("aic_bytes_with_cap(%s, %du)", argAt(args, 0), uint32(ln)), nil
	case "bytes_fromStr":
		return fmt.Sprintf("aic_bytes_from_str(%s, %du)", argAt(args, 0), uint32(ln)), nil
	case "bytes_len":
		return fmt.Sprintf("aic_list_len_u8(%s)", recv), nil
	case "bytes_cap":
		return fmt.Sprintf("aic_bytes_cap(%s)", recv), nil
	case "bytes_get", "bytes_readAt":
		return fmt.Sprintf("aic_list_get_u8(%s, %s, %s, %d)", recv, argAt(args, 1), file, ln), nil
	case "bytes_set", "bytes_writeAt":
		return fmt.Sprintf("aic_list_set_u8(%s, %s, %s, %s, %d)", recv, argAt(args, 1), argAt(args, 2), file, ln), nil
	case "bytes_append":
		return fmt.Sprintf("aic_list_append_u8(%s, %s, %s, %d)", recv, argAt(args, 1), file, ln), nil
	case "bytes_appendBytes":
		return fmt.Sprintf("aic_bytes_append_bytes(%s, %s, %s, %d)", recv, argAt(args, 1), file, ln), nil
	case "bytes_clear":
		return fmt.Sprintf("aic_list_clear_u8(%s, %s, %d)", recv, file, ln), nil
	case "bytes_slice":
		return fmt.Sprintf("aic_bytes_slice(%s, %s, %s, %s, %d)", recv, argAt(args, 1), argAt(args, 2), file, ln), nil
	case "bytes_toStr":
		return fmt.Sprintf("aic_bytes_to_str(%s, %du)", recv, uint32(ln)), nil
	}
	return "", fmt.Errorf("emit(IR): %s is not covered by the IR backend", sym)
}

// irStrCall 发射 str 家族（方法形态走字段/内联，包函数走运行时）。
func (c *Ctx) irStrCall(sym string, args []string, file string, ln int) (string, error) {
	recv := argAt(args, 0)
	switch sym {
	case "str_len": // s.len() —— aic_str 的长度字段，O(1)
		return fmt.Sprintf("((%s).len)", recv), nil
	case "str_isEmpty":
		return fmt.Sprintf("((%s).len == 0)", recv), nil
	case "str_concat":
		return fmt.Sprintf("aic_str_concat(%s, %s, %d)", recv, argAt(args, 1), ln), nil
	case "str_sub":
		return fmt.Sprintf("aic_str_sub(%s, %s, %s, %s, %d)", recv, argAt(args, 1), argAt(args, 2), file, ln), nil
	case "str_trim":
		return fmt.Sprintf("aic_str_trim(%s)", recv), nil
	case "str_split":
		return fmt.Sprintf("aic_std_str_split(%s, %s, %s, %d)", recv, argAt(args, 1), file, ln), nil
	case "str_startsWith":
		return fmt.Sprintf("aic_str_starts_with(%s, %s)", recv, argAt(args, 1)), nil
	case "str_endsWith":
		return fmt.Sprintf("aic_str_ends_with(%s, %s)", recv, argAt(args, 1)), nil
	case "str_indexOf":
		return fmt.Sprintf("aic_str_index_of_i64(%s, %s)", recv, argAt(args, 1)), nil
	case "str_fromI64":
		return fmt.Sprintf("aic_std_str_from_i64(%s, %du)", recv, uint32(ln)), nil
	case "str_fromU64":
		return fmt.Sprintf("aic_std_str_from_u64(%s, %du)", recv, uint32(ln)), nil
	case "str_fromBool":
		return fmt.Sprintf("aic_std_str_from_bool(%s)", recv), nil
	case "str_fromF64":
		return fmt.Sprintf("aic_std_str_from_f64(%s, %du)", recv, uint32(ln)), nil
	case "str_toI64":
		return fmt.Sprintf("aic_std_str_to_i64(%s, %s, %d)", recv, file, ln), nil
	case "str_toF64":
		return fmt.Sprintf("aic_std_str_to_f64(%s, %s, %d)", recv, file, ln), nil
	case "str_utf8At":
		return fmt.Sprintf("aic_std_str_utf8_at(%s, %s, %s, %d)", recv, argAt(args, 1), file, ln), nil
	case "str_codepoints":
		return fmt.Sprintf("aic_std_str_codepoints(%s, %s, %d)", recv, file, ln), nil
	}
	return "", fmt.Errorf("emit(IR): %s is not covered by the IR backend", sym)
}

// irOSCall 发射 os 家族（`os.args/readFile/readStdin/exit`；运行时在 aic_std.h
// 的薄包装 + aic_l0.h 的原语）。这些是 std 包函数，符号 = `<pkg>_<fn>`。
func (c *Ctx) irOSCall(sym string, args []string, file string, ln int) (string, error) {
	switch sym {
	case "os_args":
		c.need("l0")
		return fmt.Sprintf("aic_std_os_args(%s, %d)", file, ln), nil
	case "os_readFile":
		c.need("l0")
		return fmt.Sprintf("aic_std_os_read_file(%s, %s, %d)", argAt(args, 0), file, ln), nil
	case "os_readStdin":
		c.need("l0")
		return fmt.Sprintf("aic_std_os_read_stdin(%s, %d)", file, ln), nil
	case "os_exit":
		c.need("l0")
		return fmt.Sprintf("aic_os_exit(%s)", argAt(args, 0)), nil
	case "os_writeFile":
		c.need("l0")
		return fmt.Sprintf("aic_std_os_write_file(%s, %s, %s, %d)", argAt(args, 0), argAt(args, 1), file, ln), nil
	case "os_print":
		c.need("l1")
		return fmt.Sprintf("aic_print_str(%s)", argAt(args, 0)), nil
	case "os_println":
		c.need("l1")
		return fmt.Sprintf("(aic_print_str(%s), aic_print_nl())", argAt(args, 0)), nil
	}
	return "", fmt.Errorf("emit(IR): %s is not covered by the IR backend", sym)
}

// irMathCall 发射 math 家族（全部走 f64 原语）。
func (c *Ctx) irMathCall(sym string, args []string) (string, error) {
	fn := strings.TrimPrefix(sym, "math_")
	switch fn {
	case "abs", "floor", "ceil", "sqrt":
		return fmt.Sprintf("aic_math_%s_f64(%s)", fn, argAt(args, 0)), nil
	case "min", "max", "pow":
		return fmt.Sprintf("aic_math_%s_f64(%s, %s)", fn, argAt(args, 0), argAt(args, 1)), nil
	}
	return "", fmt.Errorf("emit(IR): math.%s is not covered by the IR backend", fn)
}

// irContainerOp 把容器符号拆成 (操作, 运行时短后缀)。
// 三种拼写都要认：`list.append[i32]`（带类型文本）、`list_i32_append`（后缀内嵌）、
// `list.append`（**类型实参省略**：构造函数从结果类型取，方法从接收者类型取）。
func (c *Ctx) irContainerOp(sym, group, recvTy string) (string, string, error) {
	rest := strings.TrimPrefix(strings.TrimPrefix(sym, group+"."), group+"_")
	if rest == "" || rest == sym {
		return "", "", fmt.Errorf("emit(IR): malformed %s symbol %q", group, sym)
	}
	if i := strings.IndexByte(rest, '['); i >= 0 && strings.HasSuffix(rest, "]") {
		op := rest[:i]
		suf, err := c.irSuffixOfText(rest[i+1 : len(rest)-1])
		if err != nil {
			return "", "", err
		}
		return op, suf, nil
	}
	parts := strings.Split(rest, "_")
	switch {
	case parts[0] == "new":
		if len(parts) >= 2 {
			return "new", strings.Join(parts[1:], "_"), nil
		}
		// `list.new()` / `set.new()` / `map.new()`：元素类型 = 结果类型（let 的类型）。
		suf, err := c.irCtorSuffix(group)
		if err != nil {
			return "", "", err
		}
		return "new", suf, nil
	case len(parts) == 1:
		// `list.append`（容器字面量的降级形态）：元素类型 = 接收者的类型。
		suf, err := c.irElemSuffixOf(group, recvTy)
		if err != nil {
			return "", "", err
		}
		return parts[0], suf, nil
	case len(parts) == 2:
		return parts[1], parts[0], nil // <S>_<op>
	case len(parts) == 3:
		// <S1>_<S2>_<op>（如 map 的键值后缀）：后缀拼回短后缀形式
		return parts[2], parts[0] + "_" + parts[1], nil
	}
	return "", "", fmt.Errorf("emit(IR): cannot split the %s symbol %q", group, sym)
}

// irElemSuffixOf 取容器类型文本的元素短后缀（list/set）。
func (c *Ctx) irElemSuffixOf(group, text string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("emit(IR): %s.<op> without a type argument needs its receiver type", group)
	}
	t, ok := c.irTab[text]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the container type %q", text)
	}
	switch group {
	case "list":
		if types.IsSlice(t) {
			return c.irSuffixOfType(types.SliceElem(t), text)
		}
	case "set":
		if types.IsSet(t) {
			return c.irSuffixOfType(types.SetElem(t), text)
		}
	}
	return "", fmt.Errorf("emit(IR): %s.<op> does not match its receiver type %q", group, text)
}

// irCtorSuffix 从结果类型里取容器的运行时短后缀（构造函数符号不带类型时用）。
func (c *Ctx) irCtorSuffix(group string) (string, error) {
	if c.irPendingTy == "" {
		return "", fmt.Errorf("emit(IR): %s.new() without a type argument needs its result type", group)
	}
	t, ok := c.irTab[c.irPendingTy]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the container type %q", c.irPendingTy)
	}
	switch group {
	case "list":
		if types.IsSlice(t) {
			return c.irSuffixOfType(types.SliceElem(t), c.irPendingTy)
		}
	case "set":
		if types.IsSet(t) {
			return c.irSuffixOfType(types.SetElem(t), c.irPendingTy)
		}
	case "map":
		if types.IsMap(t) {
			k, v, _ := types.MapParts(t)
			ks, ok1 := types.RuntimeMapKeySuffix(k)
			vs, ok2 := types.RuntimeMapValueSuffix(v)
			if ok1 && ok2 {
				return ks + "_" + vs, nil
			}
		}
	}
	return "", fmt.Errorf("emit(IR): %s.new() does not match its result type %q", group, c.irPendingTy)
}

// irSuffixOfType 把语义类型翻成运行时短后缀（Box / iface / i32 / str …）。
func (c *Ctx) irSuffixOfType(t types.Type, what string) (string, error) {
	if suf, ok := types.RuntimeSuffix(t); ok {
		return suf, nil
	}
	if _, isIfc := types.IsInterface(t); isIfc {
		return "iface", nil
	}
	return "", fmt.Errorf("emit(IR): the runtime has no instance for %q", what)
}

// irSuffixOfText 把 AIR 类型文本翻成运行时的短后缀（Box / iface / i32 / str …）。
func (c *Ctx) irSuffixOfText(text string) (string, error) {
	t, ok := c.irTab[text]
	if !ok {
		return "", fmt.Errorf("emit(IR): no C mapping for the type text %q", text)
	}
	return c.irSuffixOfType(t, text)
}

// arrayLenOfText 从 AIR 的定长数组类型文本 `[T;N]` 取长度 N。
// **唯一实现**（数组长度只许从类型文本这一处解析）。
func arrayLenOfText(text string) (int, bool) {
	if !strings.HasPrefix(text, "[") {
		return 0, false
	}
	i := strings.LastIndex(text, ";")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(text[i+1:], "]"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// argAt 取第 i 个实参（越界 = 空串；调用形态由检查器保证，这里不静默补位）。
func argAt(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return args[i]
}

func (c *Ctx) irPlace(p air.Place) (string, error) {
	switch v := p.(type) {
	case *air.VarPlace:
		return c.irVal(v.Name), nil
	case *air.FieldPlace:
		base, err := c.irPlace(v.Base)
		if err != nil {
			return "", err
		}
		name, err := c.irFieldName(v.Base, v.Idx)
		if err != nil {
			return "", err
		}
		return c.irHostCast(base, v.Base) + c.irArrow(v.Base) + name, nil
	case *air.ElemPlace:
		base, err := c.irPlace(v.Base)
		if err != nil {
			return "", err
		}
		host := c.irPlaceTy(v.Base)
		idx := c.irVal(v.Idx)
		switch {
		case strings.HasSuffix(host, "[]"), host == "str", host == "bytes":
			elem := strings.TrimSuffix(host, "[]")
			if host == "str" || host == "bytes" {
				elem = "u8"
			}
			suf, err := c.irSuffixOfText(elem)
			if err != nil {
				return "", err
			}
			c.need("l1")
			// aic_list_ref_* 给的是**元素地址**（T*）：写要解引用。
			return fmt.Sprintf("(*aic_list_ref_%s(%s, %s, %s, 0))", suf, base, idx, cstr(c.Path)), nil
		case strings.HasPrefix(host, "["):
			// 定长数组：写也走 AIC_ARRAY_AT（左值形态，越界 trap —— 004/112）。
			if n, ok := arrayLenOfText(host); ok {
				return fmt.Sprintf("AIC_ARRAY_AT(%s, %s, %s, %s, 0)", base, strconv.Itoa(n), idx, cstr(c.Path)), nil
			}
			return fmt.Sprintf("%s[%s]", base, idx), nil
		case strings.HasPrefix(host, "map["):
			return "", fmt.Errorf("emit(IR): writing a map element needs the runtime put (lowered as a call)")
		}
		return "", fmt.Errorf("emit(IR): element write on the host type %q is not covered", host)
	case *air.StrViewPlace:
		// 视图只能当值用（str 不可变）：place 形态出现就是发它的读。
		base, err := c.irPlace(v.Base)
		if err != nil {
			return "", err
		}
		return c.irStrView(&air.StrViewRHS{Base: base, Lo: v.Lo, Hi: v.Hi})
	}
	return "", fmt.Errorf("emit(IR): unsupported place %T", p)
}

// irStrView 发一次 str 视图求值：零拷贝 + 边界检查（运行期 aic_str_view）。
// 基础是 str 值名（字面量/变量/字段读都行：C 的结构体字段访问对两者都合法）。
func (c *Ctx) irStrView(v *air.StrViewRHS) (string, error) {
	base := c.irVal(v.Base)
	lo, hi := c.irVal(v.Lo), c.irVal(v.Hi)
	if base == "" || lo == "" || hi == "" {
		return "", fmt.Errorf("emit(IR): a str view needs its base and both bounds")
	}
	c.need("l1")
	return fmt.Sprintf("aic_str_view(%s, %s, %s, %s, 0)", base, lo, hi, cstr(c.Path)), nil
}
