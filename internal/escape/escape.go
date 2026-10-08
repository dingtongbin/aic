// Package escape 是 AIR 上的逃逸与区域分析（核心设计 §10.3）。
//
// **证明与消除分成两个 pass**：`Prove` 产事实（每个值与宿主的深度界），`Elim` 只
// 消费事实（不得自行推导）。消去 = 四条可解释规则 E1–E4，各自独立计数；verifier
// 复核每条消去的前提（V8），不接受「分析器说安全」。
//
// 本包**不管释放**（aic 无 GC/RC/free）：产出是①消去存储点守卫 ②为 restrict 提供
// 依据 ③为值语义聚合的 SROA 提供依据 ④校验区域归属决策。
package escape

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aic/internal/air"
)

// Bound 是一个值的深度界：Depth = 相对当前 region 深度的偏移（exact = 精确界）。
// 深度语义与检查器同一套（§五 R3）：创建 = 精确偏移；变量读 = 声明偏移；
// 字段/元素读 = 宿主界；参数 = 0；调用 = 调用点偏移 + 摘要(=0)。
type Bound struct {
	Depth int
	Exact bool
	Known bool // false = 未知（保守：不可消去）
}

func unknown() Bound      { return Bound{} }
func exact(d int) Bound   { return Bound{Depth: d, Exact: true, Known: true} }
func inexact(d int) Bound { return Bound{Depth: d, Exact: false, Known: true} }
func (b Bound) le(o Bound) bool {
	if !b.Known || !o.Known {
		return false
	}
	return b.Depth <= o.Depth
}

// Fact 是一条证明事实：某个 store 的守卫点的值与宿主界。
type Fact struct {
	Fn       string
	Block    string
	Val      string
	ValB     Bound
	Host     string
	HostB    Bound
	Limit    string
	PlaceVar string // 目标 place 的根变量名（E1 用）
	Live     bool   // 目标变量带 @live（E1 必须保留的反例）
	DeclIn   bool   // 目标变量声明在**本 region 实例内**（E1 的充分条件）
}

// Report 是分析报告（--report-guards 的 AIR 侧）。
type Report struct {
	Total      int
	Eliminated int
	Kept       int
	ByRule     map[string]int
	ByKind     map[string]int
	Facts      []Fact
	Elims      []Elim
}

// Elim 是一条消去记录（V8：每条消去都能在 IR 里查到对应 store + 规则 + 理由）。
type Elim struct {
	Fn, Block string
	Rule      string
	Reason    string
}

// Facts 的键 = (函数, 块, store 序号)。
type storeKey struct {
	fn, block string
	idx       int
}

// Analyze 跑「先 prove 后 elim」，返回报告；mod 里被消去的守卫其 Limit 被清空。
func Analyze(mod *air.Module) *Report {
	rep := &Report{ByRule: map[string]int{}, ByKind: map[string]int{}}
	facts := map[storeKey]Fact{}
	for _, f := range mod.Funcs {
		proveFunc(f, facts)
	}
	for _, f := range mod.Funcs {
		elimFunc(f, facts, rep)
	}
	for _, ft := range facts {
		rep.Facts = append(rep.Facts, ft)
		rep.Total++
	}
	sort.Slice(rep.Facts, func(i, j int) bool {
		if rep.Facts[i].Fn != rep.Facts[j].Fn {
			return rep.Facts[i].Fn < rep.Facts[j].Fn
		}
		return rep.Facts[i].Block < rep.Facts[j].Block
	})
	return rep
}

// --- prove -------------------------------------------------------------------

// proveFunc 为每个带守卫的 store 计算事实（值界 / 宿主界 / 目标变量属性）。
func proveFunc(f *air.Func, out map[storeKey]Fact) {
	// 变量界：名字 → 界（声明点定值；遮蔽按块覆盖，这里取最后声明 = 最近的）
	vars := map[string]Bound{}
	live := map[string]bool{}
	declDepth := map[string]int{}     // 变量声明时的 region 深度
	declInRegion := map[string]bool{} // 声明点是否在某个 region 块内（E1 依据）
	params := map[string]bool{}
	for _, p := range f.Params {
		params[p.Name] = true
		// 参数界 = **非精确**的入口位 0。
		//
		// 设计 §五 R3 的参数定理只保证「参数派生链写本函数**局部变量**恒安全」（局部的
		// 存储深度 ≥ 0 ≥ 参数的相对界）；它**不**保证两个参数之间的深度顺序。曾经这里
		// 写 exact(0)，于是 `func sink(h Holder, t Buf) { h.b = t }` 的字段存被 E2
		// （两边都是"精确 0"）消去 —— 实测：`region { var t = Buf{}; sink(h, t) }`
		// 之后 h.b 悬垂，区域复用内存时 h.b.n 静默变成别的值（**静默内存损坏**）。
		// 标成非精确后：E2/E3 不再对参数值/参数宿主生效，E1（目标声明在本 region 实例内）
		// 与 E4（返回值精确 0）不受影响 —— 该留的守卫留下，该消的仍然消。
		vars[p.Name] = inexact(0)
	}

	depth := 0
	for _, b := range f.Blocks {
		// 深度按块内指令序推进（region.enter/exit 是块内指令）
		idx := 0
		for _, in := range b.Insts {
			switch v := in.(type) {
			case *air.RegionEnter:
				depth++
			case *air.RegionExit:
				depth--
			case *air.Var:
				vars[v.Name] = boundOfInit(v.Init, depth, vars)
				live[v.Name] = v.Live
				declDepth[v.Name] = depth
				declInRegion[v.Name] = depth > 0 && !v.Live
			case *air.Let:
				vars[v.Tmp] = boundOfRHS(v.Rhs, depth, vars)
			case *air.Store:
				if v.Limit == "" {
					continue
				}
				host, placeVar := placeRoot(v.Place)
				hb := vars[host]
				if placeVar != "" {
					hb = vars[placeVar]
				}
				out[storeKey{fn: f.Sym, block: b.Label, idx: idx}] = Fact{
					Fn: f.Sym, Block: b.Label, Val: v.Val,
					ValB:     boundOfVal(v.Val, depth, vars),
					Host:     host,
					HostB:    hb,
					Limit:    v.Limit,
					PlaceVar: placeVar,
					Live:     live[placeVar],
					DeclIn:   declInRegion[placeVar],
				}
				idx++
			}
		}
	}
}

// boundOfInit 由初始化值定变量的界。
func boundOfInit(init string, depth int, vars map[string]Bound) Bound {
	if init == "" {
		return unknown()
	}
	return boundOfVal(init, depth, vars)
}

// boundOfRHS 由右值形态定临时量的界（与检查器的界演算同构）。
func boundOfRHS(r air.RHS, depth int, vars map[string]Bound) Bound {
	switch v := r.(type) {
	case *air.Alloc:
		switch v.Kind {
		case "task":
			return exact(0)
		case "live":
			if depth > 0 {
				return exact(depth - 1)
			}
			return exact(0)
		default: // region
			return exact(depth)
		}
	case *air.TmpRef:
		return boundOfVal(v.Name, depth, vars)
	case *air.VarRef:
		return boundOfVal(v.Name, depth, vars)
	case *air.Call:
		// 调用表达式 = 调用点偏移 + 返回摘要(=0)（§五 R3：摘要必须 = 0）→ 非精确
		return inexact(depth)
	case *air.CallInd:
		return inexact(depth) // 间接调用：实参升 Top，界不可知
	case *air.Box:
		return boundOfVal(v.Val, depth, vars)
	case *air.FieldRHS:
		base, _ := placeRoot(v.Place)
		return vars[base]
	case *air.ElemRHS:
		base, _ := placeRoot(v.Place)
		return vars[base]
	case *air.Nil:
		return exact(0)
	case *air.Const:
		return exact(0)
	}
	return unknown()
}

// boundOfVal 由值文本定界（临时量 / 变量 / 常量 / nil）。
func boundOfVal(val string, depth int, vars map[string]Bound) Bound {
	switch {
	case val == "" || val == "nil":
		return exact(0)
	case strings.HasPrefix(val, "const "):
		return exact(0)
	case strings.HasPrefix(val, "tmp "):
		return vars[strings.TrimPrefix(val, "tmp ")]
	case strings.HasPrefix(val, "var "):
		return vars[strings.TrimPrefix(val, "var ")]
	}
	if b, ok := vars[val]; ok {
		return b
	}
	return unknown()
}

// placeRoot 取 place 的根变量与宿主文本（E1/E2 用）。
func placeRoot(p air.Place) (string, string) {
	switch v := p.(type) {
	case *air.VarPlace:
		return v.Name, v.Name
	case *air.FieldPlace:
		root, _ := placeRoot(v.Base)
		return "field", root
	case *air.ElemPlace:
		root, _ := placeRoot(v.Base)
		return "elem", root
	case *air.StrViewPlace:
		root, _ := placeRoot(v.Base)
		return "strview", root
	}
	return "", ""
}

// --- elim（只消费事实，不自行推导） -------------------------------------------

func elimFunc(f *air.Func, facts map[storeKey]Fact, rep *Report) {
	idx := 0
	for _, b := range f.Blocks {
		for _, in := range b.Insts {
			st, isStore := in.(*air.Store)
			if !isStore || st.Limit == "" {
				continue
			}
			ft, has := facts[storeKey{fn: f.Sym, block: b.Label, idx: idx}]
			idx++
			if !has {
				rep.Kept++
				continue
			}
			rule, reason, eliminate := decide(ft)
			if !eliminate {
				rep.Kept++
				rep.ByKind["kept"]++
				continue
			}
			st.Limit = "" // 消去：守卫是 store 的修饰符，去掉即去掉检查
			rep.Eliminated++
			rep.ByRule[rule]++
			rep.Elims = append(rep.Elims, Elim{Fn: f.Sym, Block: b.Label, Rule: rule, Reason: reason})
		}
	}
}

// decide 按 E1–E4 判定（每条独立，理由必须能写出来）。
func decide(ft Fact) (rule, reason string, eliminate bool) {
	// E1 声明域活性：存入「在本 region 实例内声明」的变量槽 → 恒真
	if ft.DeclIn && !ft.Live {
		return "E1", fmt.Sprintf("target %s is declared inside the current region instance", ft.PlaceVar), true
	}
	// E4 返回点摘要：limit = fnentry 且返回值界精确为 0
	if ft.Limit == "fnentry" && ft.ValB.Known && ft.ValB.Exact && ft.ValB.Depth == 0 {
		return "E4", "return value bound is exactly 0 (entry slot)", true
	}
	// E2 宿主深度常量：宿主与值的界都是常量且 D(v) ≤ D(h)
	if ft.ValB.Known && ft.HostB.Known && ft.ValB.Exact && ft.HostB.Exact && ft.ValB.le(ft.HostB) {
		return "E2", fmt.Sprintf("exact bounds: value %d <= host %d", ft.ValB.Depth, ft.HostB.Depth), true
	}
	// E3 常量偏移算术：limit = depth − k，值界 = depth − j 且 j ≥ k
	k, ok := parseLimitDelta(ft.Limit)
	if ok && ft.ValB.Known {
		j := ft.ValB.Depth
		if ft.ValB.Exact && j >= k {
			return "E3", fmt.Sprintf("inductive offset: value depth %d >= limit delta %d", j, k), true
		}
	}
	return "", "", false
}

// parseLimitDelta 解析 `(aic_depth - 2u)` 形态的 limit → k = 2。
func parseLimitDelta(limit string) (int, bool) {
	i := strings.Index(limit, "-")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(limit[i+1:])
	rest = strings.TrimSuffix(rest, "u")
	rest = strings.TrimSuffix(rest, ")")
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return 0, false
	}
	return n, true
}

// Summary 是报告的一行摘要（--report-guards 与门禁用）。
func (r *Report) Summary() string {
	rules := make([]string, 0, len(r.ByRule))
	for k := range r.ByRule {
		rules = append(rules, fmt.Sprintf("%s=%d", k, r.ByRule[k]))
	}
	sort.Strings(rules)
	rs := "none"
	if len(rules) > 0 {
		rs = strings.Join(rules, ",")
	}
	return fmt.Sprintf("escape: total=%d eliminated=%d kept=%d (rules %s)",
		r.Total, r.Eliminated, r.Kept, rs)
}
