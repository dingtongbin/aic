package types

import (
	"fmt"
	"sort"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// ErrCode 码分配（语言规范 §7.2 + E-8 + C26）。
//
//   code = (包身份哈希 & 0xFFFFF000) | 变体序号        —— 高 20 位哈希、低 12 位序号
//
// 裸整数码一律编译错误（E-8）：手写数字在多模块工程里必然相撞，且相撞之后
// `e.is(X.Y)` 会互相冒充 —— 错误处理最热的路径上出现"错误码认错错误"。
//
// 本文件是码分配的**唯一实现**：检查器分配（assignErrCodes）、跨构建图查前缀
// 冲突（checkErrCodeConflicts）、emit 发射枚举常量、`is()` 的比较，三处都只读
// 分配结果，不再各自算一遍。
//
// 包身份现在是**包名**；aic.mod 落地（阶段 5）后换成"模块路径::包路径"，那时
// 码在跨模块范围唯一。位宽与掩码（20/12）不变，故那次替换不改 ABI。
// ---------------------------------------------------------------------------

// errCodeOrdinalBits 是码的低位（变体序号）位宽。
const errCodeOrdinalBits = 12

// errCodeMask 是码的高位（包身份）掩码。
const errCodeMask = uint32(0xFFFFF000)

// fnv1a32 是包身份 → 码前缀的哈希。不需要密码学强度，需要的是**确定性**与
// 跨机器一致（H2：同输入两次编译逐字节相同）。
func fnv1a32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// ErrCodePrefix 是包身份 pkgID 的 20 位码前缀。
func ErrCodePrefix(pkgID string) uint32 { return fnv1a32(pkgID) & errCodeMask }

// ErrCodeValue 是变体码：前缀 | 序号。
func ErrCodeValue(pkgID string, ordinal int) int32 {
	return int32(ErrCodePrefix(pkgID) | uint32(ordinal))
}

// ErrCodeOwner 是一条 ErrCode 枚举在构建图里的归属（冲突检测的输入）。
type ErrCodeOwner struct {
	Pkg  string
	Name string
	Pos  parse.Pos
}

// ErrCodeConflicts 返回前缀相撞的枚举对（同一前缀下 ≥2 条**不同**枚举）。
// 结果按（前缀, 包, 名）排序 —— 诊断顺序必须确定（H2）。
func ErrCodeConflicts(owners []ErrCodeOwner) [][2]ErrCodeOwner {
	byPrefix := map[uint32][]ErrCodeOwner{}
	seen := map[uint32]map[string]bool{}
	for _, o := range owners {
		p := ErrCodePrefix(o.Pkg)
		if seen[p] == nil {
			seen[p] = map[string]bool{}
		}
		key := o.Pkg + "\x00" + o.Name
		if seen[p][key] {
			continue // 同一条枚举的重复输入不是冲突
		}
		seen[p][key] = true
		byPrefix[p] = append(byPrefix[p], o)
	}
	prefixes := make([]uint32, 0, len(byPrefix))
	for p := range byPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i] < prefixes[j] })

	var out [][2]ErrCodeOwner
	for _, p := range prefixes {
		group := byPrefix[p]
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if group[i].Pkg != group[j].Pkg {
				return group[i].Pkg < group[j].Pkg
			}
			return group[i].Name < group[j].Name
		})
		// 逐对报（同一前缀下每条都要能看见自己的对手）。
		for i := 1; i < len(group); i++ {
			out = append(out, [2]ErrCodeOwner{group[0], group[i]})
		}
	}
	return out
}

// assignErrCodes 给本包全部 ErrCode 枚举分配变体码。
func (c *Checker) assignErrCodes() {
	names := make([]string, 0, len(c.enums))
	for n := range c.enums {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		en := c.enums[n]
		if en == nil || !en.ErrCode {
			continue
		}
		en.Codes = make([]int32, len(en.Variants))
		for i := range en.Variants {
			en.Codes[i] = ErrCodeValue(c.pkg, i)
		}
	}
}

// checkErrCodeConflicts 在**构建图**范围内查 20 位前缀冲突（C26）。
// 同前缀 = 两张不同枚举的变体可能得到同一个码 ⇒ `e.is` 会误判，必须编译期拒。
func (c *Checker) checkErrCodeConflicts() {
	var owners []ErrCodeOwner
	names := make([]string, 0, len(c.enums))
	for n := range c.enums {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if en := c.enums[n]; en != nil && en.ErrCode {
			owners = append(owners, ErrCodeOwner{Pkg: c.pkg, Name: en.Name, Pos: en.DeclPos})
		}
	}
	dnames := make([]string, 0, len(c.deps))
	for n := range c.deps {
		dnames = append(dnames, n)
	}
	sort.Strings(dnames)
	for _, dn := range dnames {
		info := c.deps[dn]
		if info == nil {
			continue
		}
		pkg := info.Pkg
		if pkg == "" {
			pkg = dn
		}
		enames := make([]string, 0, len(info.Enums))
		for n := range info.Enums {
			enames = append(enames, n)
		}
		sort.Strings(enames)
		for _, en := range enames {
			if e := info.Enums[en]; e != nil && e.ErrCode {
				owners = append(owners, ErrCodeOwner{Pkg: pkg, Name: e.Name, Pos: e.DeclPos})
			}
		}
	}
	for _, pair := range ErrCodeConflicts(owners) {
		a, b := pair[0], pair[1]
		c.errorAt(a.Pos, "two error-code enums share the same code prefix",
			"enum "+a.Pkg+"."+a.Name+" collides with "+b.Pkg+"."+b.Name+
				fmt.Sprintf(" (both hash to 0x%08X)", ErrCodePrefix(a.Pkg)),
			"rename one enum or move it into a different package: error codes are derived from the package identity (AIC language spec §7.2, C26)")
	}
}

// CodeOf 取变体的错误码（非 ErrCode 枚举 / 未知变体 = 0 与 false）。
// emit 的 `X.V(msg)` 直构与 `is()` 位都由它取值。
func (e *Enum) CodeOf(variant string) (int32, bool) {
	if e == nil || len(e.Codes) == 0 {
		return 0, false
	}
	for i, va := range e.Variants {
		if va.Name == variant {
			if i < len(e.Codes) {
				return e.Codes[i], true
			}
			return 0, false
		}
	}
	return 0, false
}
