package pkg

import (
	"path/filepath"
	"strings"
	"testing"
)

// pkgDir 指向语料里的多包工程（目录即包，红线 23 的锚点）。
func pkgDir(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "pkg", name, "main.aic")
}

// TestLoadClosure 检查 import 闭包 + 拓扑序 + 目录即包（同包多文件）。
func TestLoadClosure(t *testing.T) {
	u, err := Load(pkgDir(t, "chain3"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var names []string
	for _, p := range u.Order {
		names = append(names, p.Name)
	}
	// 拓扑序：依赖在前（base ← util ← chain3），入口包在最后。
	want := []string{"base", "util", "chain3"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("拓扑序 = %v，期望 %v", names, want)
	}
	if u.Entry.Name != "chain3" {
		t.Fatalf("入口包 = %s，期望 chain3", u.Entry.Name)
	}
	util := u.ByName("util")
	if util == nil {
		t.Fatal("闭包里没有 util 包")
	}
	if len(util.Files) != 2 {
		t.Fatalf("util 包文件数 = %d，期望 2（目录即包）", len(util.Files))
	}
	if len(u.ByName("base").Files) != 1 {
		t.Fatal("base 包应只有一个文件")
	}
	// 入口包是单文件程序（入口文件名不是 main.aic 时的规则不在此例）。
	if len(u.Entry.Files) != 1 {
		t.Fatalf("chain3 入口文件数 = %d，期望 1", len(u.Entry.Files))
	}
}

// TestLoadSingleFileEntry 检查「入口文件不是 main.aic → 只编译该文件」：
// 语料 ok/ 里 87 个互不相干的单文件程序共处一目录，不能被并成一个包。
func TestLoadSingleFileEntry(t *testing.T) {
	u, err := Load(filepath.Join("..", "..", "testdata", "ok", "001_hello.aic"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(u.Order) != 1 {
		t.Fatalf("单文件程序的包数 = %d，期望 1", len(u.Order))
	}
	if len(u.Entry.Files) != 1 {
		t.Fatalf("入口包文件数 = %d，期望 1", len(u.Entry.Files))
	}
	if u.Entry.Name != "ok" {
		t.Fatalf("包名 = %s，期望 ok（目录名）", u.Entry.Name)
	}
}

// TestLoadCycleRejected 检查循环 import = 编译错（红线 21）+ 三段式诊断。
func TestLoadCycleRejected(t *testing.T) {
	_, err := Load(pkgDir(t, "cycle"))
	if err == nil {
		t.Fatal("循环 import 未被拒绝（红线 21）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cyclic package import") {
		t.Fatalf("诊断未点明循环 import：%s", msg)
	}
	if !strings.Contains(msg, "a → b → a") {
		t.Fatalf("诊断未给出环路：%s", msg)
	}
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("诊断不是三段式（%d 行）：\n%s", len(lines), msg)
	}
	if !strings.Contains(lines[0], ":1:1: error: ") {
		t.Fatalf("第一段缺位置：%s", lines[0])
	}
	if !strings.HasPrefix(lines[2], "    fix: ") {
		t.Fatalf("第三段缺修复：%s", lines[2])
	}
}

// TestMissingPackageRejected 检查找不到包 = 编译错（目录即包）。
func TestMissingPackageRejected(t *testing.T) {
	_, err := Load(pkgDir(t, "chain3"))
	if err != nil {
		t.Fatalf("基线工程应可装载：%v", err)
	}
	if IsStd("print") != true || IsStd("util") != false {
		t.Fatal("IsStd 判定错误：print 是 std，util 不是")
	}
}

// TestUnitHash 检查内容哈希缓存键：同内容同键、改一个字节即换键。
func TestUnitHash(t *testing.T) {
	a, err := Load(pkgDir(t, "chain3"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, err := Load(pkgDir(t, "chain3"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.UnitHash() != b.UnitHash() {
		t.Fatal("同内容两次装载的哈希不同（缓存永不命中）")
	}
	// 改动任意包的一个字节 → 哈希必须变（内容哈希失效语义）。
	b.ByName("base").Files[0].Src += "\n"
	if a.UnitHash() == b.UnitHash() {
		t.Fatal("改了一个包的内容后哈希未变（缓存会给出陈旧产物）")
	}
	// 缓存读写：未命中 → 命中 → 字节一致。
	// 键里带本次测试的临时目录（每次运行唯一），故不存在"本机残留"导致的假命中
	// ——门禁禁止白名单外的 t.Skip（§三.3），测试必须自己保证可重复。
	key := Hash("test", a.UnitHash(), t.TempDir())
	if _, ok := CacheGet(key); ok {
		t.Fatal("新键竟然已命中（缓存目录被复用？）")
	}
	if err := CachePut(key, []byte("payload")); err != nil {
		t.Fatalf("CachePut: %v", err)
	}
	got, ok := CacheGet(key)
	if !ok || string(got) != "payload" {
		t.Fatalf("缓存读回不符：%q ok=%v", got, ok)
	}
}
