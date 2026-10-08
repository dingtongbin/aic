package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 整程序发射（核心设计 §十 阶段 1 收集 + 红线 23：发射前必须收集完整程序，
// 禁按单文件各自生成 C）。
//
// 一个「发射单元」= 一个源文件 + 它所属包的检查产物 Info。同一包的多个文件
// 共享同一份 Info（包级命名空间），跨包引用经 deps 表按包名限定解析。
//
// 发射顺序（整程序一份 C 翻译单元）：
//   ① 各包前向 typedef → ② 各包完整类型定义 → ③ 泛型实例 → ④ 全部原型
//   → ⑤ 全部函数体 → ⑥ 入口 wrapper
// 顺序理由：类型定义必须先于任何使用点；原型让跨包调用与声明序无关
// （C 里被调方定义可以在调用方之后，但类型不行）。
// ---------------------------------------------------------------------------

// FileUnit 是发射的一个源文件。
type FileUnit struct {
	// Path 是仓库根相对路径（诊断与 #line 回指，红线 9）。
	Path string
	// File 是该文件的 AST。
	File *parse.File
	// Info 是该文件所属包的检查产物（同包多文件共享）。
	Info *types.Info
}

// Unit 是一个完整程序的发射单元，Files 按拓扑序（依赖包在前）。
type Unit struct {
	// Files 是全部包的全部源文件（依赖在前；包内按文件序）。
	Files []*FileUnit
	// Entry 是入口包名（含 main 的包）。
	Entry string
}

// Single 把单文件检查结果包成一个发射单元（兼容单文件路径与旧测试）。
func Single(f *parse.File, path string, info *types.Info) *Unit {
	pkgName := "main"
	if info != nil && info.Pkg != "" {
		pkgName = info.Pkg
	}
	return &Unit{Files: []*FileUnit{{Path: path, File: f, Info: info}}, Entry: pkgName}
}

// use 把某个文件设为「当前发射单元」（c.Path/File/Info 是它的视图）。
// 名字表随之重建：pass-1 名字表只覆盖最后一个文件，而发射期逐文件都要用
// （跨包时 curFuncName 相同但形参表不同，共用一份会查不到类型）。
func (c *Ctx) use(fu *FileUnit) {
	c.cur = fu
	c.Path = fu.Path
	c.File = fu.File
	c.Info = fu.Info
	c.names = c.buildNameTypes()
}

// depInfo 取某个包的检查产物（跨包调用/类型解析用；未收集到 = nil）。
func (c *Ctx) depInfo(pkg string) *types.Info {
	if c.deps == nil {
		return nil
	}
	return c.deps[pkg]
}

// entryUnit 定位入口包的文件单元（main 所在文件优先）。
func (c *Ctx) entryUnit(u *Unit) *FileUnit {
	for _, fu := range u.Files {
		if fu.Info == nil || fu.Info.Pkg != u.Entry {
			continue
		}
		for _, d := range fu.File.Decls {
			if fd, isFn := d.(*parse.FuncDecl); isFn && fd.Name == "main" && fd.Recv == "" {
				return fu
			}
		}
	}
	for _, fu := range u.Files {
		if fu.Info != nil && fu.Info.Pkg == u.Entry {
			return fu
		}
	}
	return nil
}
