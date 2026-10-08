package emit

// ---------------------------------------------------------------------------
// mangling 单表 (核心设计 §八, 红线 10: 生成与查找共用, 禁两处各写一份)。
//
//  函数    aic_<pkg>_<name>
//  方法    aic_<pkg>_<Type>_<m>          this 为第一参数
//  泛型    aic_<pkg>_<T>__<args>         双下划线分隔实参
//
//  具名类型同理: aic_<pkg>_<Type> (typedef 名)。
//  extern func 例外: 以 C 原名链接 (零 mangle); export func 亦原名直出。
// ---------------------------------------------------------------------------

// MangleFunc 是函数/方法的 C 符号名 (唯一实现)。
func MangleFunc(pkg, recv, name string) string {
	if recv == "" {
		return "aic_" + pkg + "_" + name
	}
	return "aic_" + pkg + "_" + recv + "_" + name
}

// mangleType 是用户具名类型的 C typedef 名。
func mangleType(pkg, name string) string {
	return "aic_" + pkg + "_" + name
}

// MangleGeneric 是泛型实例的符号名: aic_<pkg>_<T>__<arg1>_<arg2>。
// 实例缓存表把同一组实参映射到同一个名字 (红线 11: 单态化必须实例缓存;
// 表本身在 types 侧, 此处只负责把已定案的名字拼出来)。
func MangleGeneric(pkg, name string, argSuffixes []string) string {
	out := "aic_" + pkg + "_" + name + "__"
	for i, a := range argSuffixes {
		if i > 0 {
			out += "_"
		}
		out += a
	}
	return out
}

// ExternName 是 extern/export 函数的 C 符号名 (原名直出, 零 mangle)。
func ExternName(name string) string { return name }
