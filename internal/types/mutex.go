package types

// ---------------------------------------------------------------------------
// sync.Mutex 最小面（核心设计 §七：写纪律由 Mutex 承担；sync 包完整面归圆 3）。
//
// T1 形态：值类型（零值 = 未加锁），方法 lock() / unlock()。串行 L2 下二者是
// 空操作（单线程无竞争），但 API 面与语义位（值类型、可作字段）保持真实：
// 换真调度（圆 3）时只换运行时实现，语料与 emit 不动。
// ---------------------------------------------------------------------------

// MutexT 是 sync.Mutex 的语义类型（单例：无类型参数、无字段）。
type MutexT struct{}

func (*MutexT) typeNode()      {}
func (*MutexT) String() string { return "sync.Mutex" }

// TMutex 是唯一实例（值类型，可拷贝——与 Go 的 sync.Mutex 不同，AIC 的 T1 面
// 不禁止拷贝，但拷贝即两个独立锁；文档里写明这一点）。
var TMutex = &MutexT{}

// mutexMethod 返回 sync.Mutex 的内建方法签名（§七）。
func mutexMethod(t Type, name string) (*FuncSig, bool) {
	if _, ok := t.(*MutexT); !ok {
		return nil, false
	}
	switch name {
	case "lock", "unlock":
		return &FuncSig{Name: name}, true
	}
	return nil, false
}
