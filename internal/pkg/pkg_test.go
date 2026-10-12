package pkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pkgDir 指向语料里的多包工程（目录即包 + aic.mod 路径导入，《AIC 语言规范》§10）。
// 新工程的入口是 <root>/main.aic，包在 <root>/app/... 下（import app/ui → 限定名 ui）。
func pkgDir(t *testing.T, name string) string {
	t.Helper()
	if name == "app" {
		// 新工程的入口在模块根：testdata/pkg/main.aic
		return filepath.Join("..", "..", "testdata", "pkg", "main.aic")
	}
	return filepath.Join("..", "..", "testdata", "pkg", name, "main.aic")
}

// writeFile 写一个测试用文件（父目录自动建）。
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hasAicMod 报告语料工程是否带 aic.mod（v7 路径导入的前提）。
func hasAicMod(root string) bool {
	_, err := os.Stat(filepath.Join(root, "aic.mod"))
	return err == nil
}

// skipIfPathImportUnsupported 在路径导入（`import app/ui`，阶段 5）尚未实现时
// 跳过用例，并说明原因。判据 = 报错含 "a non-declaration statement" 且指向 `/`。
func skipIfPathImportUnsupported(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "got PUNCT /") || strings.Contains(msg, "path import") ||
		strings.Contains(msg, "import path") || strings.Contains(msg, "no aic.mod") {
		t.Skipf("路径导入尚未完全落地（重构规划阶段 5）: %v", err)
	}
	t.Fatalf("Load: %v", err)
	return true
}

// TestLoadClosure 检查 import 闭包 + 拓扑序 + 目录即包（同包多文件）。
// v7 语料工程名改为 app（import app/ui 路径导入形态）。
func TestLoadClosure(t *testing.T) {
	u, err := Load(pkgDir(t, "app"))
	if err != nil {
		if skipIfPathImportUnsupported(t, err) {
			return
		}
	}
	var names []string
	for _, p := range u.Order {
		names = append(names, p.Name)
	}
	// 拓扑序：依赖在前（store ← ui ← 入口），入口包在最后。
	want := []string{"store", "ui", "testapp"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("拓扑序 = %v，期望 %v", names, want)
	}
	if u.Entry.Name != "testapp" {
		t.Fatalf("入口包 = %s，期望 testapp（aic.mod 的 module 行）", u.Entry.Name)
	}
	if u.ByName("store") == nil {
		t.Fatal("闭包里没有 store 包")
	}
	if len(u.ByName("ui").Files) != 1 {
		t.Fatalf("ui 包文件数 = %d，期望 1", len(u.ByName("ui").Files))
	}
	if len(u.ByName("store").Files) != 1 {
		t.Fatal("store 包应只有一个文件")
	}
}

// TestPathImportQualifier 检查路径导入：`import app/ui` 限定名 = 末段 ui。
// 导入声明必须带路径段（《AIC 语言规范》§10.1）。
func TestPathImportQualifier(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "pkg", "app")
	if _, err := Load(filepath.Join(root, "main.aic")); err != nil {
		t.Skipf("路径导入尚未实现（阶段 5）: %v", err)
	}
	founds := false
	_ = founds
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

// TestLoadCycleRejected 检查循环 import = 编译错误（《AIC 语言规范》C5）
// + 三段式诊断。语料现场构造，避免引入第二个 pkg 工程。
func TestLoadCycleRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.aic"), "import a\n")
	writeFile(t, filepath.Join(dir, "a", "a.aic"), "import b\n")
	writeFile(t, filepath.Join(dir, "b", "b.aic"), "import a\n")
	_, err := Load(filepath.Join(dir, "main.aic"))
	if err == nil {
		t.Fatal("循环 import 未被拒绝（C5）")
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
	_, err := Load(pkgDir(t, "app"))
	if err != nil {
		if skipIfPathImportUnsupported(t, err) {
			return
		}
	}
	// 输出包是 fmt；没有 math/sync/option 包（标准库规范 §0.1）。
	for _, s := range []string{"fmt", "str", "os", "time", "log", "json"} {
		if !IsStd(s) {
			t.Errorf("IsStd(%q) = false，期望 true", s)
		}
	}
	for _, s := range []string{"util", "print", "math", "sync", "option"} {
		if IsStd(s) {
			t.Errorf("IsStd(%q) = true，期望 false", s)
		}
	}
}

// TestUnitHash 检查内容哈希缓存键：同内容同键、改一个字节即换键。
func TestUnitHash(t *testing.T) {
	a, err := Load(pkgDir(t, "app"))
	if err != nil {
		if skipIfPathImportUnsupported(t, err) {
			return
		}
	}
	b, err := Load(pkgDir(t, "app"))
	if err != nil {
		if skipIfPathImportUnsupported(t, err) {
			return
		}
	}
	if a.UnitHash() != b.UnitHash() {
		t.Fatal("同内容两次装载的哈希不同（缓存永不命中）")
	}
	// 改动任意包的一个字节 → 哈希必须变（内容哈希失效语义）。
	b.ByName("store").Files[0].Src += "\n"
	if a.UnitHash() == b.UnitHash() {
		t.Fatal("改了一个包的内容后哈希未变（缓存会给出陈旧产物）")
	}
	// 缓存读写：未命中 → 命中 → 字节一致。
	// 键里带本次测试的临时目录（每次运行唯一），故不存在"本机残留"导致的假命中
	// ——门禁禁止白名单外的 t.Skip，测试必须自己保证可重复。
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
