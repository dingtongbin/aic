package parse

import "strings"

// ---------------------------------------------------------------------------
// 注解落点白名单（核心设计 §一 的 4 个注解；R20 起 @live 退役为 live 关键字）。
//
// 为什么必须校验：注解此前**只被"用到的地方"读**（@packed/@derive 在类上、
// @noblock/@blocking 在 extern func 上），写在别的声明上会被**静默忽略** ——
// `@packed func` / `@derive(Hash) func` / `@noblock class` 全部通过编译，
// 用户以为生效了，实际什么都没发生。注解写错必须报（与"非法能力名 =
// 编译错"同一条纪律）。
//
// 落点表：
//   @packed            类（值类型布局）
//   @derive(...)       类（派生能力集）
//   @noblock/@blocking extern 函数（C 边界三层；当前只记录，阶段 O 生效）
//   live               不是注解 —— R20 起是关键字，只出现在 `var live x`
//                      （parseVarTarget；误用由 parseStmt/checker 定向报错）
// ---------------------------------------------------------------------------

// checkAnnotationPlacement 校验顶层声明上的注解落点。
func (p *Parser) checkAnnotationPlacement(d Decl, annotations []string) {
	if len(annotations) == 0 {
		return
	}
	at := p.pos()
	for _, ann := range annotations {
		name := ann
		if i := strings.IndexByte(name, '('); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "@packed":
			if _, isClass := d.(*ClassDecl); !isClass {
				p.errorHere(at, "@packed applies to classes only", "@packed "+declKindName(d),
					"@packed marks a class as a value type (C struct semantics); move it onto the class declaration (core design §4)")
			}
		case "@derive":
			if _, isClass := d.(*ClassDecl); !isClass {
				p.errorHere(at, "@derive applies to classes only", ann+" "+declKindName(d),
					"@derive(ToString/Compare/Hash/Default/Clone) generates methods for a class; move it onto the class declaration (core design §4)")
			}
		case "@noblock", "@blocking":
			fd, isFunc := d.(*FuncDecl)
			if !isFunc || !fd.Extern {
				p.errorHere(at, name+" applies to extern functions only", name+" "+declKindName(d),
					"the C boundary annotations describe how an extern call blocks: write them on an `extern func` binding (core design §7)")
			}
		default:
			// 未知注解由词法负责（H7 冻结表 + @live 的迁移提示）；这里不重复报。
		}
	}
}

// declKindName 给诊断用的声明种类名（只出现语言级名字）。
func declKindName(d Decl) string {
	switch d.(type) {
	case *ImportDecl:
		return "import"
	case *ConstDecl:
		return "const"
	case *FuncDecl:
		return "func"
	case *ClassDecl:
		return "class"
	case *InterfaceDecl:
		return "interface"
	case *EnumDecl:
		return "enum"
	case *ComptimeBlock:
		return "comptime"
	}
	return "declaration"
}
