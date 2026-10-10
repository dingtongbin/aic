package types

import (
	"sort"
	"strings"

	"aic/internal/parse"
	"aic/internal/pkg"
)

// ---------------------------------------------------------------------------
// Checker（R2）：类型检查 + 区域深度标注 + 存储点守卫判定（核心设计 §二–§九）。
// 输入 = parse.File（R1 产物）；输出 = 三段式诊断 + 守卫标记表（R3 消费）。
//
// 单遍检查 + 区域偏移随语句行走维护；返回摘要恒 = 0（返回点守卫/编译错保证，
// 见 §五 R3 传播规则——R0c ② 修订版），故无需跨函数不动点。
// ---------------------------------------------------------------------------

type GuardKind int

const (
	GuardNone      GuardKind = iota
	GuardStore               // 单引用存储：比较 obj.depth 与目标存储深度
	GuardComposite           // 含引用复合值逐内部引用守卫
	GuardReturn              // 返回点：深度 ≤ 入口
	GuardDefer               // defer 注册点：深度 ≤ 入口
)

// GuardSpec 标记一个需要运行时检查的点位。Delta 仅变量槽存储使用：
// 目标声明深度 = 当前深度 − Delta（R3 发射形态）。
type GuardSpec struct {
	Kind  GuardKind
	Delta int
	// Dynamic = limit 必须**运行期**取（宿主是形参派生时，其对象所在区域深度在
	// 被调方不可知：参数定理只给"绝对深度 ≤ 入口"，用 aic_depth 当 limit 会**放宽**
	// 检查 ⇒ 静默内存损坏）。发射侧按 `aic_region_depth_of(宿主->hdr.reg)` 取。
	Dynamic bool
}

type CheckResult struct {
	Errors []CheckerError
	Guards map[parse.Node]GuardSpec
	// Types: 每个表达式节点的检查类型（R3 emit 消费）。
	Types map[parse.Expr]Type
	// Info: emit 需要的只读语义事实（局部名表/签名/具名类型/泛型实参）。
	Info *Info
}

type Checker struct {
	file   string
	pkg    string
	files  []*parse.File
	errors []CheckerError
	// deps 是已检查完的 import 包（包名 → 该包的 Info）。
	// 跨包名解析的唯一入口：类型/函数/常量/变体都从这里查（核心设计 §四 可见性）。
	deps   map[string]*Info
	guards map[parse.Node]GuardSpec
	types  map[parse.Expr]Type
	// typeParams 是当前声明（泛型类/泛型函数）在作用域内的类型参数名。
	// 只影响类型位解析：`T` 解析成 TypeParam，而不是「未知类型名」。
	typeParams map[string]bool
	// failed 记下"声明失败过"的名字（const 折叠失败等）：后续引用不再刷
	// "未声明的名字" —— 一条错刷成多条会把首因埋在级联噪音里。
	failed map[string]bool
	// funcInsts/funcInstSeen 是泛型函数实例登记（实例缓存，红线 11）。
	funcInsts    []FuncInst
	funcInstSeen map[string]bool
	// locals: 函数标识 → 局部名/参数名集合（emit 区分裸名与包级名）
	locals map[string]map[string]bool
	// fnKey: 当前正在检查的函数标识（FuncKey 形式）
	fnKey string

	imports       map[string]bool
	classes       map[string]*Class
	fnReturnCount int
	ifaces        map[string]*Interface
	enums         map[string]*Enum
	funcs         map[string]*FuncSig
	consts        map[string]*Symbol
	typeOrder     []string // 声明序（布局确定性）

	// 当前函数检查状态
	fn        *FuncSig
	thisClass *Class
	scope     *Scope
	regionOff int
	nilState  map[string]*nilState // 局部变量非 nil 流证明（§九）

	// N1 闭包捕获收集：lambdaBase = 当前闭包**自己的**最外层作用域（走到它就停），
	// caps = 正在收集的捕获表（自由变量 = 在 lambdaBase 之外解析到的名字）。
	lambdaBase *Scope
	caps       *[]capture
	// closures 是已收集的闭包捕获表（LambdaExpr 节点 → 捕获项名，按首次出现序）。
	closures map[parse.Expr][]string
	// capRefs 记录闭包体内**每一次**捕获引用（标识符节点 → 环境槽号）。按节点记，
	// 因为同名局部量可以遮蔽捕获项 —— 光有名字表无法区分两者。
	capRefs map[parse.Expr]int
	// comptime 是编译期内省原语的折叠结果（N4）：emit 按它发常量/sizeof。
	comptime map[parse.Expr]ComptimeVal
	// arrayLens 是 [T;N] 的 N 折出来的长度（键 = N 表达式节点）：emit 按**同一份结果**
	// 发 C 数组维度（禁 emit 再折一遍 —— 那条路只认字面量与 const 名）。
	arrayLens map[parse.Expr]int64
	// pendingCons 是推迟到类型收齐后再校验的约束（接口可以声明在用到它的泛型之后）。
	pendingCons []pendingConstraint
	// pendingEnumPayloads 是推迟到类型收齐后再解析的枚举负载（前向引用合法）。
	pendingEnumPayloads []pendingEnumPayload
	// typeExprTypes 是「类型写法节点 → 语义类型」的登记表（AIR/emit 的唯一来源，
	// 禁各自再写一份 typeExpr→type 规则：那些副本各有各的漏项）。
	typeExprTypes map[parse.TypeExpr]Type
	// callTypeArgs 是泛型调用点的实例实参（键 = 调用节点）：推断与显式两条路都登记，
	// 单态化（mair）与 C 发端按它决定要给哪个实例发符号。
	callTypeArgs map[parse.Expr][]Type
}

// implementedStd 是**当前构建真的可用**的 std 包（其余注册名 = 阶段 S 的路线图条目）。
// 这张表是"状态"不是"规则"：每个包的成员面仍由 stdFuncSig/stdType 唯一决定。
var implementedStd = map[string]bool{
	"print": true, "str": true, "os": true, "math": true, "option": true,
	"testing": true, "sync": true, "time": true, "ctx": true,
}

// capture 是一个按值捕获的绑定（名字 + 类型 + 声明的区域深度）。
type capture struct {
	Name string
	Type Type
	Off  int // 声明时的 region 深度（闭包存进更浅区域 = 逃逸）
}

// CheckFile checks one translation unit. The File is always checked fully:
// 单次编译报全部错误（红线 14）。
func CheckFile(f *parse.File) *CheckResult {
	return CheckPackage([]*parse.File{f}, nil)
}

// CheckPackage 检查一个包（目录即包：同目录多个 .aic 合成一个包，共用一个 Info）。
//
// deps 是 import 闭包里已检查完的包（包名 → Info），按拓扑序由调用方提供
// （internal/pkg 收集，红线 23：整程序收集后方可发射）。deps 里的符号按 §四
// 可见性规则：顶层名首字母大写 = 跨包可见（小写与 `_` 前缀 = 包私有）。
func CheckPackage(files []*parse.File, deps map[string]*Info) *CheckResult {
	c := &Checker{
		files:   files,
		deps:    deps,
		guards:  map[parse.Node]GuardSpec{},
		types:   map[parse.Expr]Type{},
		locals:  map[string]map[string]bool{},
		imports: map[string]bool{},
		classes: map[string]*Class{},
		ifaces:  map[string]*Interface{},
		enums:   map[string]*Enum{},
		funcs:   map[string]*FuncSig{},
		consts:  map[string]*Symbol{},
		failed:  map[string]bool{},

		closures: map[parse.Expr][]string{},
		capRefs:  map[parse.Expr]int{},
		comptime: map[parse.Expr]ComptimeVal{},
		arrayLens: map[parse.Expr]int64{},
		typeExprTypes: map[parse.TypeExpr]Type{},
		callTypeArgs:  map[parse.Expr][]Type{},
	}
	if len(files) == 0 {
		return &CheckResult{Info: &Info{}}
	}
	c.file = files[0].Path
	c.pkg = pkgNameFromPath(files[0].Path) // 包名唯一所有者（目录即包，§四）
	c.collectDecls(files)
	// 约束名的校验**推迟到收集结束**：接口可以声明在用到它的泛型之后
	// （`func sum[T: Shape]` 写在 `interface Shape` 之前是合法的 —— 此前在收集期
	// 逐条校验，于是"先写函数后写接口"报"未知泛型约束"）。
	c.flushConstraints()
	c.checkFuncBodies(files)
	return &CheckResult{
		Errors: c.errors,
		Guards: c.guards,
		Types:  c.types,
		Info:   c.buildInfo(),
	}
}

// pendingEnumPayload 是一条推迟到类型收齐后再解析的枚举负载。
type pendingEnumPayload struct {
	en  *Enum
	idx int
	te  parse.TypeExpr
	at  parse.Pos
}

// pendingConstraint 是一条推迟到收集结束再校验的约束（接口可后声明）。
type pendingConstraint struct {
	owner string
	tpc   parse.TypeParamConstraint
}

// flushConstraints 在**类型收齐后**校验约束名（顺序无关）。
func (c *Checker) flushConstraints() {
	for _, pc := range c.pendingCons {
		c.checkConstraintDecl(pc.owner, pc.tpc)
	}
	c.pendingCons = nil
}

// deferConstraint 记录一条待校验的约束（见 flushConstraints）。
func (c *Checker) deferConstraint(owner string, tpc parse.TypeParamConstraint) {
	c.pendingCons = append(c.pendingCons, pendingConstraint{owner: owner, tpc: tpc})
}

func (c *Checker) errorAt(pos parse.Pos, desc, context, fix string) {
	c.errors = append(c.errors, CheckerError{
		Pos:     parsePos{File: pos.File, Line: pos.Line, Col: pos.Col},
		Desc:    desc,
		Context: context,
		Fix:     fix,
	})
}

// --- 第一阶段：包级声明收集 -------------------------------------------------

func (c *Checker) collectDecls(files []*parse.File) {
	// 声明序三轮：
	//   ⓪ import 先收全 —— 包内符号一次导入全局可用（§四），且签名解析要用到
	//      import 集合；少这一轮就会出现「同包另一文件的 import 尚未登记」的
	//      假错（跨文件包按文件名序处理，先后顺序不该改变可见性）；
	//   ① 类型（类/接口/枚举自引用合法，§二.6）与常量/函数签名；
	//   ② 字段类型与方法签名（类型彼此已可引用）。
	for _, f := range files {
		for _, d := range f.Decls {
			im, ok := d.(*parse.ImportDecl)
			if !ok {
				continue
			}
			if c.imports[im.Name] {
				c.errorAt(im.Pos, "duplicate import of the same package", "import "+im.Name,
					"delete the duplicate import line; one import makes the package symbols available throughout")
				continue
			}
			// 幽灵包（注册了名字但一个成员都没有）在 **import 位**就报"尚未实现"：
			// 此前 `import json` 过 check、直到调用成员才报"包内无此函数"，使用者会以为
			// 包存在只是自己拼错了。
			if pkg.IsStd(im.Name) && !implementedStd[im.Name] {
				c.errorAt(im.Pos, "this standard package is not implemented yet", "import "+im.Name,
					"the standard library lands in phase S (core design §15.3 lists the package roadmap); the packages available today are print/str/os/math/option/testing/sync/time/ctx")
				continue
			}
			c.imports[im.Name] = true
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *parse.ImportDecl:
				// ⓪ 已处理
			case *parse.ConstDecl:
				c.collectConst(v)
			case *parse.ClassDecl:
				c.collectClass(v)
			case *parse.InterfaceDecl:
				c.collectInterface(v)
			case *parse.EnumDecl:
				c.collectEnum(v)
			case *parse.FuncDecl:
				c.collectFunc(v)
			}
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			if cl, ok := d.(*parse.ClassDecl); ok {
				c.finishClass(cl)
			}
		}
	}
	// ②b 枚举负载：类型名此刻已全部登记 ⇒ 前向引用（`enum E { X(Late) }` 里 Late 后声明）
	//    现在可解析；递归负载检测也在这里做（要等负载类型齐了才准）。
	c.resolveEnumPayloads()
	// ③ comptime 块（N4）：必须排在 ② 之后 —— 内省原语要读**已解析的字段表**
	//    （`sizeOf(T)`/`fieldsOf(T)` 都在 finishClass 里才定型）；而它声明的常量
	//    又必须早于函数体检查登记，否则函数体里用 `N` 会报"未声明的名字"。
	for _, f := range files {
		for _, d := range f.Decls {
			if cb, ok := d.(*parse.ComptimeBlock); ok {
				c.checkComptimeBlock(cb)
			}
		}
	}
}

func (c *Checker) collectConst(v *parse.ConstDecl) {
	if _, exists := c.consts[v.Name]; exists {
		c.errorAt(v.Pos, "duplicate constant declaration", "const "+v.Name,
			"only one constant may carry this name; rename or delete one")
		return
	}
	val, valErr := foldConst(v.Init, c)
	if valErr != "" {
		// 折叠失败的原因各不相同（溢出 / 除零 / 不可折叠）：desc 要能分辨，
		// 具体原因放 fix 行（三段式的 fix 是"唯一可执行的机械动作"）。
		desc := "const initialisation must be foldable literal arithmetic"
		switch {
		case strings.Contains(valErr, "overflow"), strings.Contains(valErr, "underflow"),
			strings.Contains(valErr, "by zero"):
			desc = "the constant expression has no valid compile-time value"
		}
		c.errorAt(v.Pos, desc, "const "+v.Name+" = …", valErr)
		// 记下"这个名字声明失败过"：后续引用不再刷"未声明的名字"（级联噪音会把首因埋掉）。
		c.failed[v.Name] = true
		return
	}
	var ty Type
	if v.Type != nil {
		ty = c.resolveType(v.Type)
		if ty == nil {
			return
		}
		if !constFitsTy(val, ty) {
			c.errorAt(v.Pos, "the constant value does not match the declared type", "const "+v.Name+"'s value does not fit "+ty.String(),
				"adjust the value or the declared type; there is no implicit conversion (red line 1)")
			return
		}
	} else {
		ty = val.inferredType()
	}
	c.consts[v.Name] = &Symbol{Kind: SymConst, Name: v.Name, Type: ty, Const: val, Private: !exported(v.Name)}
}

func (c *Checker) collectClass(v *parse.ClassDecl) {
	if _, exists := c.classes[v.Name]; exists {
		c.errorAt(v.Pos, "duplicate class declaration", "class "+v.Name,
			"only one type may carry this name; rename or delete one")
		return
	}
	cl := &Class{
		Name:        v.Name,
		Pkg:         c.pkg, // 包名唯一所有者：mangling 只读它（红线 10）
		Fields:      []Field{},
		fieldIdx:    map[string]int{},
		Methods:     map[string]*FuncSig{},
		Packed:      v.Packed,
		TypeParams:  v.TypeParams,
		Constraints: v.Constraints,
		Derived:     c.derivesOf(v),
	}
	c.classes[v.Name] = cl
	// N3：泛型类约束名在声明处校验。
	for _, tpc := range v.Constraints {
		c.deferConstraint("class "+v.Name, tpc)
	}
}

// derivesOf 解析 @derive 能力集（§四 三件套 + §16 N4 扩展）。
//
// **能力集是封闭枚举**（核心设计 §16 N4）：ToString / Compare / Hash /
// Default / Clone 已实现；Json / Row 是设计里承诺的（序列化代码生成），
// 尚未实现 —— 这里**明确报「未实现」而不是静默接受**（接受了却不生成方法 = 撒谎）。
// 非法能力名 = 编译错（不静默忽略：注解写错必须报）。
func (c *Checker) derivesOf(v *parse.ClassDecl) map[string]bool {
	out := map[string]bool{}
	for _, ann := range v.Annotations {
		// 词法把注解原文整段留下（含 @ 前缀）：@derive(ToString)。
		if !strings.HasPrefix(ann, "@derive(") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ann, "@derive("), ")")
		for _, part := range strings.Split(inner, ",") {
			name := strings.TrimSpace(part)
			switch name {
			case "ToString", "Compare", "Hash", "Default", "Clone":
				out[name] = true
			case "":
				c.errorAt(v.Pos, "@derive capability name is missing", "@derive("+inner+")",
					"write @derive(ToString) / @derive(Compare) / @derive(Hash) / @derive(Default) / @derive(Clone); they may be combined")
			case "Json", "Row":
				// 能力集是封闭枚举（§16 N4），Json/Row 在其中但**本构建不可用**：
				// 文案要说明"现在能写什么"，而不是给一句路线图。
				c.errorAt(v.Pos, "@derive("+name+") is not available in this build", name,
					"serialise explicitly (write a toJson/fromJson method on the class), or use @derive(ToString) for printing; "+name+" lands with the standard library (core design §16 N4 / §15.3)")
			default:
				c.errorAt(v.Pos, "unsupported @derive capability", name,
					"the closed set is ToString / Compare / Hash / Default / Clone (core design §16 N4)")
			}
		}
	}
	return out
}

// finishClass 第二轮：字段类型与方法签名（类型彼此已可引用）。
func (c *Checker) finishClass(v *parse.ClassDecl) {
	cl := c.classes[v.Name]
	// 泛型类的字段与方法签名在**类型参数在作用域内**时解析：`var v T` 里的 T
	// 必须解析成 TypeParam，否则泛型类根本声明不出来（T1 泛型单态化的前提）。
	saved := c.typeParams
	c.typeParams = typeParamSet(v.TypeParams)
	defer func() { c.typeParams = saved }()
	for _, fd := range v.Fields {
		ft := c.resolveType(fd.Type)
		if ft == nil {
			continue
		}
		if cl.Packed && !packedFieldOK(ft) && !containsTypeParam(ft) {
			// 含类型参数的字段推迟到**实例化时**校验（那时 A → i32 已知）；
			// 非泛型字段仍在此处立即拒绝。
			c.errorAt(fd.Pos, "@packed field type is not on the whitelist",
				"field "+targetList(fd.Targets)+" has type "+ft.String(),
				"@packed fields allow basic types / str / nested @packed / [T;N] / payload-free enums only; use a plain class for references and containers (core design §4)")
			continue
		}
		for _, tgt := range fd.Targets {
			if tgt.Live {
				// R20：live 只修饰**局部变量目标**（§五 R4 的分配位提升）。
				// 字段的生命周期归宿主对象，没有"提升一层"可言。
				c.errorAt(tgt.Pos, "live applies to local variables only",
					"field "+cl.Name+"."+tgt.Name+" is marked live",
					"drop live from the field; write var live x = C{...} on a local declaration inside a function body (core design §5 R4)")
				continue
			}
			if _, dup := cl.fieldIdx[tgt.Name]; dup {
				c.errorAt(tgt.Pos, "duplicate field declaration", cl.Name+"."+tgt.Name,
					"one field per name per class; rename or delete one")
				continue
			}
			cl.fieldIdx[tgt.Name] = len(cl.Fields)
			cl.Fields = append(cl.Fields, Field{Name: tgt.Name, Type: ft, Line: tgt.Pos.Line})
		}
	}
	for _, m := range v.Methods {
		sig := c.buildSig(m, v.Name)
		if sig == nil {
			continue
		}
		if sig.IsInit && !m.IsInit {
			c.errorAt(m.Pos, "init cannot be an ordinary method name", cl.Name+".init",
				"init is only a constructor (func init(...)); rename an ordinary method")
		}
		if _, dup := cl.Methods[sig.Name]; dup {
			desc := "methods cannot be overloaded (one per name per class)"
			if sig.IsInit {
				desc = "at most one init, and it cannot be overloaded"
			}
			c.errorAt(m.Pos, desc,
				cl.Name+"."+sig.Name,
				"delete or merge the overload; use different method names for different shapes (core design §4)")
			continue
		}
		cl.Methods[sig.Name] = sig
	}
	c.deriveMethods(v, cl)
	c.delegateMethods(v, cl)
	c.typeParams = saved
}

// delegateMethods 处理 `use 字段 类型`（N10，核心设计 §16）：
// 把被委托类型的方法**提升**到本类（调用点 a.m() → a.字段.m()）。
//
// 规则：
//   - 字段必须在本类声明，且类型是 class / @packed（值类型没有方法）；
//   - 同名方法在本类已有**显式实现** ⇒ 显式实现获胜，不报错（这正是"显式实现"）；
//   - 同名方法来自**另一条 use** ⇒ 歧义，必须在本类显式实现，报错；
//   - 泛型实例字段暂不支持（报错而不是默默发错代码）。
func (c *Checker) delegateMethods(v *parse.ClassDecl, cl *Class) {
	for _, u := range v.Uses {
		t := c.resolveType(u.Type)
		if t == nil {
			continue
		}
		var target *Class
		switch tv := t.(type) {
		case *Class:
			target = tv
		default:
			if _, isInst := t.(*Instance); isInst {
				c.errorAt(u.Pos, "use does not support a generic instance field yet", cl.Name+"."+u.Field+" "+t.String(),
					"embed the concrete instantiation as a class of its own, or call the field's methods explicitly")
				continue
			}
			c.errorAt(u.Pos, "use needs a class field", cl.Name+"."+u.Field+" "+t.String(),
				"`use field Type` promotes the methods of a class (or @packed) field (core design §16 N10)")
			continue
		}
		if _, ok := cl.fieldIdx[u.Field]; !ok {
			c.errorAt(u.Pos, "the delegated field is not declared in this class", cl.Name+"."+u.Field,
				"declare the field first: var "+u.Field+" "+target.Name+"; then use "+u.Field+" "+target.Name+" (fields stay local to the class)")
			continue
		}
		// 方法名按字典序处理：登记顺序必须确定（map 遍历序会破坏 H2）。
		names := make([]string, 0, len(target.Methods))
		for name := range target.Methods {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sig, ok := target.Method(name)
			if !ok || sig.IsInit {
				continue
			}
			if prev, dup := cl.Methods[name]; dup {
				if prev.DelegateField != "" && prev.DelegateField != u.Field {
					c.errorAt(u.Pos, "two use fields promote the same method name", cl.Name+"."+name,
						"implement "+name+" explicitly in "+cl.Name+", or drop one of the use declarations (core design §16 N10)")
				}
				// 本类显式实现（或有 @derive 的同名方法）获胜：不覆盖、不报错。
				continue
			}
			promoted := *sig
			promoted.Recv = cl.Name
			promoted.DelegateField = u.Field
			promoted.DelegateClass = target.Name
			cl.Methods[name] = &promoted
		}
	}
}

// typeParamSet 把声明的类型参数名收成集合（空 = 非泛型）。
func typeParamSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// deriveMethods 装配 @derive(Compare) / @derive(Hash) 的派生方法签名（§四 三件套）。
// 派生方法就是普通方法：可被 `p.compare(q)` / `p.hash()` 调用，走同一套 mangling
// 与调用检查；方法体由 emit 按字段序合成（同一条规则，禁两处各写一份）。
func (c *Checker) deriveMethods(v *parse.ClassDecl, cl *Class) {
	if cl.HasDerive("Compare") {
		if _, dup := cl.Methods["compare"]; dup {
			c.errorAt(v.Pos, "a derived method collides with an explicit one", cl.Name+".compare",
				"@derive(Compare) generates compare(other) -> i32; drop the explicit method or the capability")
		} else if bad, bline := firstNonComparable(cl); bad != "" {
			c.errorAt(deriveFieldPos(v.Pos, bline), "a field of @derive(Compare) is not comparable", bad,
				"Compare supports numeric / bool / str / payload-free enum / @packed / Compare-enabled class fields only (core design §4)")
		} else {
			cl.Methods["compare"] = &FuncSig{
				Name: "compare", Recv: cl.Name, Derived: true,
				Params: []string{"other"}, ParamTypes: []Type{cl}, Results: []Type{TI32},
			}
		}
	}
	if cl.HasDerive("Hash") {
		if _, dup := cl.Methods["hash"]; dup {
			c.errorAt(v.Pos, "a derived method collides with an explicit one", cl.Name+".hash",
				"@derive(Hash) generates hash() -> u64; drop the explicit method or the capability")
		} else if bad, bline := firstNonHashable(cl); bad != "" {
			c.errorAt(deriveFieldPos(v.Pos, bline), "a field of @derive(Hash) is not hashable", bad,
				"Hash supports numeric / bool / str / payload-free enum / @packed / Hash-enabled class fields only (core design §4)")
		} else {
			cl.Methods["hash"] = &FuncSig{
				Name: "hash", Recv: cl.Name, Derived: true,
				Results: []Type{TU64},
			}
		}
	}
	// N4：@derive(Default) / @derive(Clone) —— 与三件套同款「签名由检查器装配、
	// 体由 emit 按字段序合成」。泛型类暂不支持（实例化时的字段代换另算）。
	if cl.HasDerive("Default") {
		if len(cl.TypeParams) > 0 {
			c.errorAt(v.Pos, "@derive(Default) on a generic class is not supported yet", cl.Name,
				"derive on the concrete instantiation instead (core design §16 N4)")
		} else if _, dup := cl.Methods["default"]; dup {
			c.errorAt(v.Pos, "a derived method collides with an explicit one", cl.Name+".default",
				"@derive(Default) generates default() -> T; drop the explicit method or the capability")
		} else {
			cl.Methods["default"] = &FuncSig{
				Name: "default", Recv: cl.Name, Derived: true, NoRecv: true,
				Results: []Type{cl},
			}
		}
	}
	if cl.HasDerive("Clone") {
		if len(cl.TypeParams) > 0 {
			c.errorAt(v.Pos, "@derive(Clone) on a generic class is not supported yet", cl.Name,
				"derive on the concrete instantiation instead (core design §16 N4)")
		} else if _, dup := cl.Methods["clone"]; dup {
			c.errorAt(v.Pos, "a derived method collides with an explicit one", cl.Name+".clone",
				"@derive(Clone) generates clone() -> T; drop the explicit method or the capability")
		} else if bad := firstNonClonable(cl); bad != "" {
			c.errorAt(v.Pos, "a field of @derive(Clone) cannot be copied", bad,
				"Clone copies value fields; a reference field would alias the original — copy it explicitly in a method (core design §16 N4)")
		} else {
			cl.Methods["clone"] = &FuncSig{
				Name: "clone", Recv: cl.Name, Derived: true,
				Results: []Type{cl},
			}
		}
	}
}

// firstNonClonable 返回第一个不可按值拷贝字段的说明（全可拷贝返回空串）。
// Clone 的语义是**值拷贝**：引用字段（class/interface/容器）会变成别名，
// 那不是"克隆"——必须显式实现。
func firstNonClonable(cl *Class) string {
	for i := range cl.Fields {
		ft := cl.Fields[i].Type
		if isReference(ft) {
			return cl.Name + "." + cl.Fields[i].Name + " is " + ft.String()
		}
		if containsReference(ft) {
			return cl.Name + "." + cl.Fields[i].Name + " contains a reference"
		}
	}
	return ""
}

// deriveFieldPos 把"类声明位置 + 字段行号"合成为字段处的诊断位置
// （@derive 的字段级错误必须指到**那个字段**，而不是类声明行）。
func deriveFieldPos(clsPos parse.Pos, fieldLine int) parse.Pos {
	if fieldLine <= 0 {
		return clsPos
	}
	return parse.Pos{File: clsPos.File, Line: fieldLine, Col: 1}
}

// firstNonComparable 返回第一个不可比较字段的说明与**它的行号**（全可比较 = ("", 0)）。
// 行号是必须的：@derive 的字段级错误此前报在**类声明行**，用户要自己找是哪个字段。
func firstNonComparable(cl *Class) (string, int) {
	for i := range cl.Fields {
		fd := cl.Fields[i]
		if why := comparableField(fd.Type, 0); why != "" {
			return cl.Name + "." + fd.Name + ": " + why, fd.Line
		}
	}
	return "", 0
}

func comparableField(t Type, depth int) string {
	if t == nil || depth > 8 {
		return ""
	}
	if b, ok := t.(*Basic); ok {
		switch b.Name {
		case "str", "bool", "Err":
			return ""
		}
		if isNumeric(b) {
			return ""
		}
		return "type " + b.Name + " is not comparable"
	}
	if cl, ok := t.(*Class); ok {
		if !cl.HasDerive("Compare") {
			return "class " + cl.String() + " has no @derive(Compare)"
		}
		return ""
	}
	if en, ok := t.(*Enum); ok {
		if en.HasData {
			return "enum with payload " + en.String() + " is not comparable (a payload-free enum compares by tag)"
		}
		return ""
	}
	if arr, ok := t.(*ArrayT); ok {
		return comparableField(arr.Elem, depth+1)
	}
	return "type " + t.String() + " is not comparable (containers and function types have no comparison)"
}

// firstNonHashable 返回第一个不可哈希字段的说明与**它的行号**（全可哈希 = ("", 0)）。
func firstNonHashable(cl *Class) (string, int) {
	for i := range cl.Fields {
		fd := cl.Fields[i]
		if why := hashableField(fd.Type, 0); why != "" {
			return cl.Name + "." + fd.Name + ": " + why, fd.Line
		}
	}
	return "", 0
}

func hashableField(t Type, depth int) string {
	if t == nil || depth > 8 {
		return ""
	}
	if b, ok := t.(*Basic); ok {
		if isNumeric(b) || b.Name == "bool" || b.Name == "str" {
			return "" // str 按内容哈希（§四：@derive(Hash) 支持 str 字段）
		}
		return "type " + b.Name + " is not hashable"
	}
	if cl, ok := t.(*Class); ok {
		if !cl.HasDerive("Hash") {
			return "class " + cl.String() + " has no @derive(Hash)"
		}
		return ""
	}
	if en, ok := t.(*Enum); ok {
		if en.HasData {
			return "enum with payload " + en.String() + " is not hashable"
		}
		return ""
	}
	if arr, ok := t.(*ArrayT); ok {
		return hashableField(arr.Elem, depth+1)
	}
	return "type " + t.String() + " is not hashable"
}

func (c *Checker) collectInterface(v *parse.InterfaceDecl) {
	if _, exists := c.ifaces[v.Name]; exists {
		c.errorAt(v.Pos, "duplicate interface declaration", "interface "+v.Name,
			"only one type may carry this name")
		return
	}
	ifc := &Interface{Name: v.Name, Pkg: c.pkg, Methods: map[string]*FuncSig{}}
	c.ifaces[v.Name] = ifc
	for _, m := range v.Methods {
		sig := c.buildSig(m, v.Name)
		if sig == nil {
			continue
		}
		if m.Body != nil {
			c.errorAt(m.Pos, "an interface method cannot have a body",
				"interface "+v.Name+"'s "+sig.Name,
				"an interface holds signatures only: func name(...) -> T; implementations live in class methods (structural satisfaction, core design §4)")
			continue
		}
		if _, dup := ifc.Methods[sig.Name]; dup {
			c.errorAt(m.Pos, "duplicate interface method", v.Name+"."+sig.Name, "one method per name per interface")
			continue
		}
		ifc.Methods[sig.Name] = sig
		ifc.Slots = append(ifc.Slots, sig.Name) // 声明序 = 分发槽位号（§十）
	}
	c.embedInto(v, ifc)
}

// embedInto 把嵌入接口的槽位并进本接口（N9，核心设计 §16）。
//
// 语义：**并集**，不是继承 ——
//   - 槽位序 = 声明序（嵌入项插在它出现的位置，方法各自保持自己的位置）；
//   - 同名同签名 = 合并成**一个**槽位（不重复）；
//   - 同名不同签名 = 编译错（槽位冲突，必须显式裁决）；
//   - 无覆盖、无层次、无默认实现。
func (c *Checker) embedInto(v *parse.InterfaceDecl, ifc *Interface) {
	if len(v.Embeds) == 0 {
		return
	}
	ordered := make([]string, 0, len(ifc.Slots))
	seen := map[string]bool{}
	push := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		ordered = append(ordered, name)
	}
	addEmbed := func(e parse.IfaceEmbed) {
		emb := c.lookupInterfaceRef(e.Name)
		if emb == nil {
			c.errorAt(e.Pos, "unknown interface in the embed list", e.Name,
				"embed an interface declared in this package, or a package-qualified one (core design §16 N9)")
			return
		}
		if emb == ifc {
			c.errorAt(e.Pos, "an interface cannot embed itself", ifc.Name,
				"remove the self-reference; embedding is a union, not a hierarchy (core design §16 N9)")
			return
		}
		for _, slot := range InterfaceSlots(emb) {
			sig, _ := emb.Method(slot)
			if prev, dup := ifc.Methods[slot]; dup {
				if !sameSig(prev, sig) {
					c.errorAt(e.Pos, "embedded interfaces disagree on the signature of a method", slot,
						"two embedded interfaces declare "+slot+" with different signatures; rename one or implement the union explicitly (core design §16 N9)")
					continue
				}
				push(slot)
				continue
			}
			ifc.Methods[slot] = sig
			push(slot)
		}
	}
	for i := 0; i <= len(ifc.Slots); i++ {
		for _, e := range v.Embeds {
			if e.AfterMethod == i {
				addEmbed(e)
			}
		}
		if i < len(ifc.Slots) {
			push(ifc.Slots[i])
		}
	}
	ifc.Slots = ordered
}

// lookupInterfaceRef 解析嵌入项的接口名（本包裸名 / 跨包 pkg.Name）。
func (c *Checker) lookupInterfaceRef(name string) *Interface {
	if ifc, ok := c.ifaces[name]; ok {
		return ifc
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		pkg, base := name[:i], name[i+1:]
		if dep := c.deps[pkg]; dep != nil {
			if ifc, ok := dep.Interfaces[base]; ok {
				return ifc
			}
		}
	}
	return nil
}

// sameSig 见 assign.go（唯一定义）。


func (c *Checker) collectEnum(v *parse.EnumDecl) {
	if _, exists := c.enums[v.Name]; exists {
		c.errorAt(v.Pos, "duplicate enum declaration", "enum "+v.Name,
			"only one type may carry this name")
		return
	}
	en := &Enum{Name: v.Name, Pkg: c.pkg, variantI: map[string]int{}, DeclPos: v.Pos}
	c.enums[v.Name] = en
	for _, va := range v.Variants {
		if _, dup := en.variantI[va.Name]; dup {
			c.errorAt(va.Pos, "duplicate enum variant", en.Name+"."+va.Name, "one variant per name per enum")
			continue
		}
		en.variantI[va.Name] = len(en.Variants)
		// 变体名先登记、**负载类型推迟到类型收齐后**解析（`enum E { X(Late) }` 允许
		// `class Late` 声明在后面 —— 与类的字段解析同一口径；此前在收集期就地解析，
		// 于是枚举负载的前向引用报"未知类型名"）。
		pv := Variant{Name: va.Name}
		if va.Payload != nil {
			en.HasData = true
		}
		en.Variants = append(en.Variants, pv)
		if va.Payload != nil {
			c.pendingEnumPayloads = append(c.pendingEnumPayloads, pendingEnumPayload{en: en, idx: len(en.Variants) - 1, te: va.Payload, at: va.Pos})
		}
	}
}

// resolveEnumPayloads 在**所有具名类型都登记之后**解析负载类型并做递归检测。
func (c *Checker) resolveEnumPayloads() {
	for _, pe := range c.pendingEnumPayloads {
		t := c.resolveType(pe.te)
		if t == nil {
			continue
		}
		c.enums[pe.en.Name].Variants[pe.idx].Payload = t
	}
	c.pendingEnumPayloads = nil
	for _, en := range c.enums {
		c.detectEnumCycle(en, map[string]bool{}, en.DeclPos)
	}
}

// detectEnumCycle: enum 的 payload 不得（传递地）包含自身。
func (c *Checker) detectEnumCycle(en *Enum, seen map[string]bool, at parse.Pos) {
	if seen[en.Name] {
		c.errorAt(at, "the enum recursively contains itself", "enum "+en.Name,
			"a payload variant may not (transitively) carry itself; use a class for recursive structures (core design §2.6)")
		return
	}
	seen[en.Name] = true
	for _, va := range en.Variants {
		if va.Payload == nil {
			continue
		}
		if sub, ok := va.Payload.(*Enum); ok {
			c.detectEnumCycle(sub, cloneSet(seen), at)
		}
		if arr, ok := va.Payload.(*ArrayT); ok {
			if sub, ok := arr.Elem.(*Enum); ok {
				c.detectEnumCycle(sub, cloneSet(seen), at)
			}
		}
	}
}

func cloneSet(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func (c *Checker) collectFunc(v *parse.FuncDecl) {
	sig := c.buildSig(v, "")
	if sig == nil {
		return
	}
	if _, exists := c.funcs[sig.Name]; exists {
		c.errorAt(v.Pos, "duplicate function declaration", "func "+sig.Name,
			"functions cannot be overloaded (one per name per package); use different names for different shapes")
		return
	}
	c.funcs[sig.Name] = sig
}

// buildSig 解析函数/方法签名的类型。类型未解析时返回 nil（错误已报）。
func (c *Checker) buildSig(v *parse.FuncDecl, recv string) *FuncSig {
	sig := &FuncSig{
		Name:        v.Name,
		Recv:        recv,
		Export:      v.Export,
		Extern:      v.Extern,
		IsInit:      v.IsInit,
		TypeParams:  v.TypeParams,
		Constraints: v.Constraints,
	}
	// N3：约束名在**声明处**校验（不认识的名字绝不推迟到调用点）。
	for _, tpc := range v.Constraints {
		c.deferConstraint("func "+v.Name, tpc)
	}
	// 泛型函数的签名在**类型参数在作用域内**时解析：`func id[T](x T) -> T` 的
	// 形参与返回位都指向 TypeParam（T1 泛型单态化的前提）。
	// 方法不得新增类型参数（§四）：方法体里只继承所属类的类型参数。
	saved := c.typeParams
	if recv == "" {
		c.typeParams = typeParamSet(v.TypeParams)
	}
	defer func() { c.typeParams = saved }()
	ok := true
	for _, p := range v.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			ok = false
			continue
		}
		sig.Params = append(sig.Params, p.Name)
		sig.ParamTypes = append(sig.ParamTypes, pt)
	}
	for _, r := range v.Results {
		rt := c.resolveType(r.Type)
		if rt == nil {
			ok = false
			continue
		}
		sig.Results = append(sig.Results, rt)
	}
	if !ok {
		return nil
	}
	// 公开函数的签名是**跨包契约**：签名里出现本包私有类型 = 调用方拿不到那个类型
	// （此前放行 ⇒ `func Make() -> secret` 编译通过，跨包调用点却无法书写该类型）。
	c.checkExportedSig(v.Pos, sig)
	return sig
}

// checkExportedSig 检查公开（跨包可见）函数/方法的签名里是否出现本包私有类型。
func (c *Checker) checkExportedSig(at parse.Pos, sig *FuncSig) {
	if sig == nil {
		return
	}
	// 可见性：自由函数看名字；方法随所属类型（R7 决策一：成员不单独按大小写判定）。
	pub := exported(sig.Name)
	if sig.Recv != "" {
		pub = exported(sig.Recv)
	}
	if !pub {
		return
	}
	for i, t := range sig.ParamTypes {
		if name := c.privateTypeName(t, 0); name != "" {
			c.errorAt(at, "a public signature mentions a package-private type",
				"parameter "+itoa(i+1)+" has type "+name,
				"capitalise the type's first letter, or make the function package-private (lowercase name) (core design §4 visibility)")
			return
		}
	}
	for i, t := range sig.Results {
		if name := c.privateTypeName(t, 0); name != "" {
			c.errorAt(at, "a public signature mentions a package-private type",
				"result "+itoa(i+1)+" has type "+name,
				"capitalise the type's first letter, or make the function package-private (lowercase name) (core design §4 visibility)")
			return
		}
	}
}

// privateTypeName 报告类型里第一个**本包私有**具名类型的名字（无 = 空串）。
// 穿透容器/数组/泛型实例/函数类型 —— 它们都可能把私有类型带进公开签名。
func (c *Checker) privateTypeName(t Type, depth int) string {
	if t == nil || depth > 8 {
		return ""
	}
	switch v := t.(type) {
	case *Class:
		if c.isPrivateType(v.Pkg, v.Name) {
			return v.Name
		}
	case *Enum:
		if c.isPrivateType(v.Pkg, v.Name) {
			return v.Name
		}
	case *Interface:
		if c.isPrivateType(v.Pkg, v.Name) {
			return v.Name
		}
	case *Instance:
		if name := c.privateTypeName(v.Base, depth+1); name != "" {
			return name
		}
		for _, a := range v.Args {
			if name := c.privateTypeName(a, depth+1); name != "" {
				return name
			}
		}
	case *Slice:
		return c.privateTypeName(v.Elem, depth+1)
	case *SetT:
		return c.privateTypeName(v.Elem, depth+1)
	case *MapT:
		if name := c.privateTypeName(v.Key, depth+1); name != "" {
			return name
		}
		return c.privateTypeName(v.Value, depth+1)
	case *ArrayT:
		return c.privateTypeName(v.Elem, depth+1)
	case *FuncT:
		for _, p := range v.Params {
			if name := c.privateTypeName(p, depth+1); name != "" {
				return name
			}
		}
		return c.privateTypeName(v.Result, depth+1)
	}
	return ""
}

// isPrivateType 报告（包, 名）是否指向**本包**的私有具名类型。
func (c *Checker) isPrivateType(pkg, name string) bool {
	if pkg != "" && pkg != c.pkg {
		return false // 别的包的类型由那个包自己保证
	}
	return !exported(name)
}

// resolveType 把 parse 类型表达式解析为语义类型；失败时报错并返回 nil。
//
// **解析结果按节点登记**（`TypeExprTypes`）：AIR 与 emit 需要"这个类型写法对应哪个语义
// 类型"时必须读这份表，而不是各自再写一份 typeExpr→type 的规则 —— 那类副本各自漏东西
// （AIR 那份漏了 `NamedType.Args` ⇒ `Box[Box[i32]]` 静默退化成 `ok_Box`）。
func (c *Checker) resolveType(t parse.TypeExpr) Type {
	ty := c.resolveTypeInner(t)
	if t != nil {
		c.typeExprTypes[t] = ty
	}
	return ty
}

func (c *Checker) resolveTypeInner(t parse.TypeExpr) Type {
	switch v := t.(type) {
	case *parse.BasicType:
		if b, ok := basicType(v.Name); ok {
			return b
		}
		c.errorAt(v.Pos, "unknown basic type name", v.Name,
			"basic types = i8 i16 i32 i64 u8 u16 u32 u64 usize f32 f64 bool str (Err is built in); for a class write its name")
		return nil
	case *parse.NamedType:
		if v.Pkg == "" {
			if b, ok := basicType(v.Name); ok {
				return b // 基本类型名由 parse 产为 NamedType，此处归位
			}
		}
		if v.Pkg != "" {
			if !c.imports[v.Pkg] {
				c.errorAt(v.Pos, "package not imported", v.Pkg+"."+v.Name,
					"write import "+v.Pkg+" first, then use its types")
				return nil
			}
			if stdTy := stdType(v.Pkg, v.Name, c); stdTy != nil {
				if len(v.Args) == 0 {
					return stdTy
				}
				inst := &Instance{Base: stdTy}
				for _, a := range v.Args {
					at := c.resolveType(a)
					if at == nil {
						return nil
					}
					inst.Args = append(inst.Args, at)
				}
				if en, isEnum := stdTy.(*Enum); isEnum && len(inst.Args) != len(en.TypeParams) {
					c.errorAt(v.Pos, "wrong number of type arguments",
						v.Pkg+"."+v.Name+" arguments "+itoa(len(inst.Args))+", parameters "+itoa(len(en.TypeParams))+"",
						"type arguments map one-to-one onto the declaration [T, ...]")
					return nil
				}
				return inst
			}
			// 用户包限定类型 util.Reader / util.Box[i32]（可见性在 depType 内判）
			if c.depInfo(v.Pkg) != nil {
				if dt := c.depType(v.Pkg, v.Name, v.Args, v.Pos); dt != nil {
					return dt
				}
				c.errorAt(v.Pos, "no such type in the package", v.Pkg+"."+v.Name,
					"check the spelling; across packages only types starting with an uppercase letter are visible (core design §4)")
				return nil
			}
			c.errorAt(v.Pos, "no such type in the package", v.Pkg+"."+v.Name,
				"check the spelling; a user package's public types are declared in its own files (core design §4)")
			return nil
		}
		// chan[T]：parse 把 `chan[T]` 解析成泛型具名类型，这里归位（§七）。
		if v.Name == "chan" {
			return c.resolveChanType(v)
		}
		// bytes（N7）：预声明类型，与 str/Err 同级，不需要 import。
		if v.Name == "bytes" {
			return TBytes
		}
		// 裸 `Option[T]`：`option` 已 import 时按 `option.Option[T]` 解析。
		// 文档（语言规范 §3/§17）与编译器自己的修复文案都推荐裸拼写，此前只有
		// 限定拼写能解析 —— 照文档写会得到 "unknown type name"。
		if v.Name == "Option" && c.imports["option"] {
			if en, ok := stdType("option", "Option", c).(*Enum); ok && en != nil {
				inst := &Instance{Base: en}
				for _, a := range v.Args {
					at := c.resolveType(a)
					if at == nil {
						return nil
					}
					inst.Args = append(inst.Args, at)
				}
				if len(inst.Args) != len(en.TypeParams) {
					c.errorAt(v.Pos, "wrong number of type arguments",
						"Option arguments "+itoa(len(inst.Args))+", parameters "+itoa(len(en.TypeParams)),
						"Option[T] takes exactly one type argument (core design §14)")
					return nil
				}
				return inst
			}
		}
		// 类型参数在作用域内（泛型类/泛型函数的声明体里）：`T` 就是类型参数。
		if c.typeParams[v.Name] {
			if len(v.Args) > 0 {
				c.errorAt(v.Pos, "a type parameter takes no type arguments", v.Name+"[...]",
					"only a named generic type takes arguments; T itself is already a type")
				return nil
			}
			return &TypeParam{Name: v.Name}
		}
		if sym := c.lookupType(v.Name); sym != nil {
			if len(v.Args) > 0 {
				return c.instantiate(sym, v.Args, v.Pos)
			}
			return sym
		}
		c.errorAt(v.Pos, "unknown type name", v.Name,
			"a type must be declared in this file or come from an imported package; write generics as Box[T]")
		return nil
	case *parse.SliceType:
		elem := c.resolveType(v.Elem)
		if elem == nil {
			return nil
		}
		return &Slice{Elem: elem}
	case *parse.MapType:
		k := c.resolveType(v.Key)
		val := c.resolveType(v.Value)
		if k == nil || val == nil {
			return nil
		}
		if !hashable(k) {
			c.errorAt(v.Pos, "the map key type is not hashable", typeText(k),
				"a key allows numeric / bool / str / payload-free enum / @packed only; class references and containers cannot be keys")
			return nil
		}
		// 审计 ③：**键/值槽位表**（runtime aic_l1_map.h 的实例表，rtsuffix.go 是
		// 唯一事实源）在检查期就验 —— 旧行为是检查器放行、emit 才炸
		// "malformed map symbol"（不可操作的晚期错误）。
		if _, ok := RuntimeMapKeySuffix(k); !ok {
			c.errorAt(v.Pos, "the map key type has no runtime instance", typeText(k),
				"keys available today: i32 / i64 / u32 / u64 / usize / bool / str "+
					"(struct keys and enum keys need per-type hashing, which lands with monomorphized containers)")
			return nil
		}
		if _, ok := RuntimeMapValueSuffix(val); !ok {
			c.errorAt(v.Pos, "the map value type has no runtime instance", typeText(val),
				"values available today: i32 / i64 / f64 / bool / str or a reference (class / interface); "+
					"for byte counters store i64 and narrow on read")
			return nil
		}
		return &MapT{Key: k, Value: val}
	case *parse.SetType:
		elem := c.resolveType(v.Elem)
		if elem == nil {
			return nil
		}
		if !hashable(elem) {
			c.errorAt(v.Pos, "the set element type is not hashable", typeText(elem),
				"elements allow numeric / bool / str / payload-free enum / @packed only")
			return nil
		}
		// 审计 ③：set 的元素槽位表同样在检查期验（旧行为 = emit 期才炸）。
		if _, ok := RuntimeSuffix(elem); !ok {
			c.errorAt(v.Pos, "the set element type has no runtime instance", typeText(elem),
				"elements available today: i8 / i16 / i32 / i64 / u8 / u16 / u32 / u64 / usize / f64 / bool / str "+
					"or a class reference; containers and interfaces are not instantiated yet")
			return nil
		}
		return &SetT{Elem: elem}
	case *parse.ArrayType:
		elem := c.resolveType(v.Elem)
		if elem == nil {
			return nil
		}
		n, nerr := c.constArrayLen(v.Size)
		if nerr != "" {
			c.errorAt(v.Pos, "invalid fixed-size array length", nerr,
				"N is a compile-time integer constant: a literal, a const name, or a foldable intrinsic such as sizeOf(T) (core design §2.2)")
			return nil
		}
		// 把折出来的长度记进表：emit 侧按**同一份结果**发 C 数组维度
		// （此前 emit 自己再折一遍、只认字面量与 const 名 ⇒ `[i32; sizeOf(i32)]` 发出 `[0]`）。
		if v.Size != nil {
			c.arrayLens[v.Size] = n
		}
		return &ArrayT{Elem: elem, N: n}
	case *parse.FuncType:
		ft := &FuncT{}
		for _, p := range v.Params {
			pt := c.resolveType(p)
			if pt == nil {
				return nil
			}
			ft.Params = append(ft.Params, pt)
		}
		if v.Result != nil {
			rt := c.resolveType(v.Result)
			if rt == nil {
				return nil
			}
			ft.Result = rt
		}
		return ft
	}
	return nil
}

// lookupType 按名查本包类型。
func (c *Checker) lookupType(name string) Type {
	if cl, ok := c.classes[name]; ok {
		return cl
	}
	if ifc, ok := c.ifaces[name]; ok {
		return ifc
	}
	if en, ok := c.enums[name]; ok {
		return en
	}
	return nil
}

// instantiate 处理泛型实例 Box[T]（T1 单态化：返回**真实例**，实参随类型携带）。
func (c *Checker) instantiate(base Type, args []parse.TypeExpr, at parse.Pos) Type {
	switch b := base.(type) {
	case *Class:
		if len(b.TypeParams) == 0 {
			c.errorAt(at, "a non-generic type takes no type arguments", b.Name+"[…]",
				"drop [..]; only a class or function declared with [T] takes arguments")
			return nil
		}
		if len(args) != len(b.TypeParams) {
			c.errorAt(at, "wrong number of type arguments",
				b.Name+" arguments "+itoa(len(args))+", parameters "+itoa(len(b.TypeParams))+"",
				"type arguments map one-to-one onto the declaration [T, ...]")
			return nil
		}
		inst := &Instance{Base: b}
		for _, a := range args {
			rt := c.resolveType(a)
			if rt == nil {
				return nil
			}
			inst.Args = append(inst.Args, rt)
		}
		// N3：泛型类实例化点的约束校验（位置 = 使用点，绝不报在模板体内部）。
		m := map[string]Type{}
		for i, tp := range b.TypeParams {
			if i < len(inst.Args) {
				m[tp] = inst.Args[i]
			}
		}
		c.checkCallConstraints(at, b.Name, b.Constraints, m)
		// @packed 的字段白名单在实例化时校验（形参字段此前无法判定）。
		if b.Packed {
			for i := range b.Fields {
				ft := subst(b.Fields[i].Type, b.TypeParams, inst.Args)
				if !packedFieldOK(ft) {
					c.errorAt(at, "@packed field type is not on the whitelist",
						"field "+b.Name+"."+b.Fields[i].Name+" becomes "+ft.String(),
						"@packed fields allow basic types / str / nested @packed / [T;N] / payload-free enums only (core design §4)")
					return nil
				}
			}
		}
		return inst
	case *Enum:
		if len(b.TypeParams) == 0 {
			return base
		}
		if len(args) != len(b.TypeParams) {
			c.errorAt(at, "wrong number of type arguments",
				b.Name+" arguments "+itoa(len(args))+", parameters "+itoa(len(b.TypeParams))+"",
				"type arguments map one-to-one onto the declaration [T, ...]")
			return nil
		}
		inst := &Instance{Base: b}
		for _, a := range args {
			rt := c.resolveType(a)
			if rt == nil {
				return nil
			}
			inst.Args = append(inst.Args, rt)
		}
		return inst
	}
	c.errorAt(at, "only named types take type arguments", typeText(base), "write it as Box[T]")
	return nil
}

// containsTypeParam 报告类型里是否含类型参数（@packed 白名单推迟判定的依据）。
func containsTypeParam(t Type) bool {
	switch v := t.(type) {
	case *TypeParam:
		return true
	case *Slice:
		return containsTypeParam(v.Elem)
	case *SetT:
		return containsTypeParam(v.Elem)
	case *MapT:
		return containsTypeParam(v.Key) || containsTypeParam(v.Value)
	case *ArrayT:
		return containsTypeParam(v.Elem)
	case *FuncT:
		for _, p := range v.Params {
			if containsTypeParam(p) {
				return true
			}
		}
		return containsTypeParam(v.Result)
	case *Instance:
		for _, a := range v.Args {
			if containsTypeParam(a) {
				return true
			}
		}
	}
	return false
}

func hashable(t Type) bool {
	switch v := t.(type) {
	case *Basic:
		return v.Name != "Err" || true // Err 按值可哈希（code+msg 内容哈希）
	case *Class:
		return v.Packed
	case *Enum:
		return !v.HasData
	case *ArrayT:
		return hashable(v.Elem)
	}
	return false
}

func packedFieldOK(t Type) bool {
	switch v := t.(type) {
	case *Basic:
		return true
	case *Class:
		return v.Packed
	case *Enum:
		return !containsReference(v)
	case *ArrayT:
		return packedFieldOK(v.Elem)
	}
	return false
}

func targetList(ts []parse.VarTarget) string {
	var parts []string
	for _, t := range ts {
		parts = append(parts, t.Name)
	}
	return strings.Join(parts, ", ")
}
