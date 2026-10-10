package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// ---------------------------------------------------------------------------
// 内容哈希缓存（核心设计 §十一：产物缓存 ~/.cache/aic，内容哈希失效）。
//
// 键 = 整程序内容哈希（任一源文件改动即换键，故不存在陈旧命中）；值 = 该内容
// 的编译产物。缓存目录可用 AIC_CACHE 覆盖（门禁把缓存指到临时目录，保 H2/H3
// 不受上一轮残留影响）。同一 key 两次 Put 必须写同一份字节（H2 纪律的延伸）。
// ---------------------------------------------------------------------------

// cacheFormat 是缓存布局版本：任何一次布局/语义变更都要 +1，使旧缓存自然失效。
const cacheFormat = "aic-cache-v2"

// CompilerID 是本编译器的身份指纹（K6：缓存键**必须**含编译器身份）。
//
// 为什么必须有：缓存值 = 可执行产物。改了编译器而键不含身份 ⇒ 命中旧产物，
// 于是"改了代码却跑到旧行为"——本轮实测踩到过（typeName 的输出随编译器变，
// 缓存却一直命中第一次成功的那份产物）。指纹 = Go 构建信息 + 可执行文件
// 大小/时间戳；一次进程内只算一次。
var compilerIDOnce string

func CompilerID() string {
	if compilerIDOnce != "" {
		return compilerIDOnce
	}
	parts := []string{cacheFormat}
	if bi, ok := debug.ReadBuildInfo(); ok && bi != nil {
		parts = append(parts, bi.GoVersion, bi.Main.Path, bi.Main.Version, bi.Main.Sum)
		for _, s := range bi.Settings {
			parts = append(parts, s.Key+"="+s.Value)
		}
	}
	if exe, err := os.Executable(); err == nil {
		parts = append(parts, filepath.ToSlash(exe))
		if st, err := os.Stat(exe); err == nil {
			parts = append(parts, fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()))
		}
	}
	compilerIDOnce = Hash(parts...)
	return compilerIDOnce
}

// CacheDir 返回缓存根目录（AIC_CACHE 优先；否则 ~/.cache/aic）。
func CacheDir() string {
	if env := os.Getenv("AIC_CACHE"); env != "" {
		return env
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "aic")
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "aic")
	}
	return filepath.Join(os.TempDir(), "aic-cache")
}

// Hash 是内容哈希的唯一实现（sha256 十六进制；分片之间用 \x00 分隔，
// 避免 ("ab","c") 与 ("a","bc") 撞键）。
func Hash(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UnitHash 是整程序的内容哈希（缓存键）：包名 + 每个文件的相对路径 + 文件内容。
// 用相对路径而非绝对路径：同一工程换目录不影响命中（也不让临时目录名进键）。
func (u *Unit) UnitHash() string {
	parts := []string{cacheFormat, "cc:" + CompilerID()}
	for _, p := range u.Order {
		parts = append(parts, "pkg:"+p.Name)
		for _, f := range p.Files {
			parts = append(parts, f.Path, Hash(f.Src))
		}
	}
	return Hash(parts...)
}

// CacheGet 读一条缓存；未命中或读失败都返回 (nil, false)（缓存永不影响正确性）。
func CacheGet(key string) ([]byte, bool) {
	b, err := os.ReadFile(cachePath(key))
	if err != nil {
		return nil, false
	}
	return b, true
}

// CachePut 写一条缓存（原子替换：先写 .tmp 再 rename，避免半截文件被读到）。
func CachePut(key string, data []byte) error {
	path := cachePath(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// cachePath 把键映射到文件路径（两级目录，避免单目录塞满）。
func cachePath(key string) string {
	if len(key) < 4 {
		key = Hash(key)
	}
	return filepath.Join(CacheDir(), key[:2], key[2:4], key+".bin")
}

// CacheStats 汇总缓存占用（诊断用：aic build --cache-stats 打印）。
func CacheStats() (files int, bytes int64) {
	root := CacheDir()
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes
}

// Describe 返回缓存位置与占用的一行摘要（CLI 诊断输出）。
func Describe() string {
	files, bytes := CacheStats()
	return fmt.Sprintf("%s (%d artifacts, %.1f KiB)", filepath.ToSlash(CacheDir()), files, float64(bytes)/1024)
}

// PkgList 是拓扑序的包名列表（诊断用，禁用于 mangling）。
func (u *Unit) PkgList() string {
	names := make([]string, 0, len(u.Order))
	for _, p := range u.Order {
		names = append(names, p.Name)
	}
	return strings.Join(names, " → ")
}

// ---------------------------------------------------------------------------
// CacheStore：带命名空间前缀的对象式缓存句柄（F1 函数级缓存用）。
// 与 CacheGet/Put 同一目录同一原子写口径，只是把前缀并进键，避免不同用途
// （整 exe 产物 / 函数 C 文本）互相覆盖。
// ---------------------------------------------------------------------------

// CacheStore 是一个命名空间化的缓存（dir 为根，prefix 内所有键共享）。
type CacheStore struct {
	dir    string
	prefix string
}

// NewCacheStore 建缓存句柄（dir 空 = CacheDir()；prefix 空 = 无前缀）。
func NewCacheStore(dir, prefix string) *CacheStore {
	if dir == "" {
		dir = CacheDir()
	}
	return &CacheStore{dir: dir, prefix: prefix}
}

// Get 读一条（键含命名空间前缀）。
func (cs *CacheStore) Get(key string) ([]byte, bool) {
	b, err := os.ReadFile(cs.path(key))
	if err != nil {
		return nil, false
	}
	return b, true
}

// Put 写一条（原子替换）。
func (cs *CacheStore) Put(key string, data []byte) error {
	path := cs.path(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// path 把（前缀 + 键）映射到两级目录下的文件路径。
func (cs *CacheStore) path(key string) string {
	k := key
	if cs.prefix != "" {
		k = Hash(cs.prefix, key)
	}
	if len(k) < 4 {
		k = Hash(k)
	}
	return filepath.Join(cs.dir, k[:2], k[2:4], k+".bin")
}
