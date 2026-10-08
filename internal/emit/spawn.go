package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// scope / spawn 发射（核心设计 §七；T1 最小面 = 串行 L2，见 runtime/aic_l2.h）。
//
//   scope { … }   →  aic_scope _sc; aic_scope_enter(&_sc); 区域 push; 体; 区域 pop;
//                    aic_scope_exit(&_sc)   /* join：按登记序跑完 */
//   spawn f(a)    →  实参块（void* 槽）+ 薄 thunk（按被调函数一个，TU 末尾发）
//                    + aic_scope_spawn(&_sc, thunk, env)
//
// 实参传递约定（T1 定案，写在这里是为了让它只有一处定义）：
//   - 标量/bool/引用句柄 → 直接塞进 void* 槽（值类型走 intptr 往返）；
//   - str/@packed/[T;N]/Err 等**按值承载的结构** → 先拷进 L2 实参块，槽里放指针，
//     由 thunk 取用后释放（避免匿名结构类型不兼容的 C 陷阱）。
// 为什么不用「每站点一个具名结构」：那需要在发射体之前预扫全部 spawn 站点，
// 而本约定让站点处只依赖 void* 槽，thunk 定义可推迟到 TU 末尾。
// ---------------------------------------------------------------------------

// scopeState 是当前 scope 的发射状态（嵌套 scope 用栈式恢复）。
type scopeState struct {
	varName string
}

// spawnThunk 是一个被 spawn 的函数对应的薄 thunk（每函数一份）。
type spawnThunk struct {
	name string
	sig  *types.FuncSig
	envs []int // 需要解引用并释放的槽位下标（按值承载的结构）
}

// emitScope 发射 scope { }：进入任务域 + 区域层，退出时 join。
func (c *Ctx) emitScope(v *parse.ScopeStmt, fnHasErr bool) error {
	c.need("l2") // 按需链接：用 scope 才拉 L2（红线 16）
	c.srcLine(v.Pos)
	name := c.tmp("scope")
	c.line("aic_scope %s;", name)
	c.line("aic_scope_enter(&%s);", name)
	c.line("aic_region_push(%s, %d);", cstr(c.Path), v.Pos.Line)
	c.line("{")
	saved := c.scopeCur
	c.scopeCur = &scopeState{varName: name}
	err := c.emitBody(v.Body, fnHasErr)
	c.scopeCur = saved
	if err != nil {
		return err
	}
	c.line("}")
	c.line("aic_region_pop();")
	c.line("aic_scope_exit(&%s);", name) // join（含未接收结果的任务）
	return nil
}

// emitSpawn 发射 spawn f(args)：实参块 + 登记（thunk 定义见 emitSpawnThunks）。
func (c *Ctx) emitSpawn(v *parse.SpawnStmt) error {
	if c.scopeCur == nil {
		return fmt.Errorf("emit: spawn outside a scope (line %d)", v.Pos.Line)
	}
	call, ok := v.Call.(*parse.Call)
	if !ok {
		return fmt.Errorf("emit: spawn needs a call (line %d)", v.Pos.Line)
	}
	id, ok := call.Fn.(*parse.Ident)
	if !ok {
		return fmt.Errorf("emit: spawn needs a free function (line %d)", v.Pos.Line)
	}
	sig, has := c.Info.Funcs[id.Name]
	if !has || sig == nil {
		return fmt.Errorf("emit: undeclared function %s (line %d)", id.Name, v.Pos.Line)
	}
	if len(sig.Results) > 0 {
		return fmt.Errorf("emit: a spawned function must not return a value (line %d)", v.Pos.Line)
	}
	th, err := c.registerSpawnThunk(sig)
	if err != nil {
		return err
	}
	c.srcLine(v.Pos)
	env := c.tmp("env")
	n := len(sig.Params)
	if n == 0 {
		n = 1
	}
	c.line("void *%s[%d] = {0};", env, n)
	c.line("void *%s_raw = aic_task_env(sizeof(void *) * %d);", env, n)
	c.line("(void)%s_raw;", env)
	// 原型在**原型段**统一发（emitSpawnThunkProtos）：块作用域不允许 static 函数
	// 声明，而 10.4 第 4 条要求生成函数一律 static —— 故原型提前到文件作用域。
	for i := range sig.Params {
		var wt types.Type
		if i < len(sig.ParamTypes) {
			wt = sig.ParamTypes[i]
		}
		e, err := c.expr(call.Args[i], wt)
		if err != nil {
			return err
		}
		if byValueStruct(wt) {
			tmp := c.tmp("arg")
			c.line("%s *%s = (%s *)aic_task_env(sizeof(%s));", c.cTypeName(wt), tmp, c.cTypeName(wt), c.cTypeName(wt))
			c.line("*%s = %s;", tmp, e)
			c.line("%s[%d] = (void *)%s;", env, i, tmp)
			continue
		}
		c.line("%s[%d] = (void *)(intptr_t)(%s);", env, i, e)
	}
	c.line("memcpy(%s_raw, %s, sizeof(void *) * %d);", env, env, n)
	c.line("aic_scope_spawn(&%s, %s, %s_raw);", c.scopeCur.varName, th.name, env)
	return nil
}

// registerSpawnThunk 登记（或复用）某个被 spawn 函数的薄 thunk（每函数一份）。
func (c *Ctx) registerSpawnThunk(sig *types.FuncSig) (*spawnThunk, error) {
	name := "aic_spthunk_" + sanitize(c.cFuncName(sig))
	if c.spawnSeen == nil {
		c.spawnSeen = map[string]bool{}
	}
	for _, th := range c.spawnThunks {
		if th.name == name {
			return th, nil
		}
	}
	if c.seeded && !c.spawnSeen[name] {
		return nil, fmt.Errorf("emit: spawn thunk %s only surfaced on the second pass", name)
	}
	c.spawnSeen[name] = true
	th := &spawnThunk{name: name, sig: sig}
	for i := range sig.Params {
		var wt types.Type
		if i < len(sig.ParamTypes) {
			wt = sig.ParamTypes[i]
		}
		if byValueStruct(wt) {
			th.envs = append(th.envs, i)
		}
	}
	c.spawnThunks = append(c.spawnThunks, th)
	return th, nil
}

// emitSpawnThunks 在 TU 末尾发射全部 thunk（取槽 → 直调目标函数 → 释放按值块）。
func (c *Ctx) emitSpawnThunks() {
	for _, th := range c.spawnThunks {
		c.line("static void %s(void *raw) {", th.name)
		c.line("    void **e = (void **)raw;")
		args := ""
		for i := range th.sig.Params {
			if i > 0 {
				args += ", "
			}
			var wt types.Type
			if i < len(th.sig.ParamTypes) {
				wt = th.sig.ParamTypes[i]
			}
			if byValueStruct(wt) {
				args += fmt.Sprintf("*(%s *)e[%d]", c.cTypeName(wt), i)
				continue
			}
			args += fmt.Sprintf("(%s)(intptr_t)e[%d]", c.cTypeName(wt), i)
		}
		c.line("    %s(%s);", c.cFuncName(th.sig), args)
		for _, i := range th.envs {
			c.line("    free(e[%d]);", i) // 只释放按值实参的拷贝
		}
		/* env 块归 L2（aic_task_run 里 free），此处**不得**再 free(raw)。 */
		c.line("}")
		c.line("")
	}
}

// byValueStruct 报告该类型是否按值承载（str/@packed/[T;N]/Err）——这些类型经
// void* 槽传递时必须先拷贝（C 里不能把结构塞进指针，匿名结构类型也不兼容）。
func byValueStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	if types.IsStrType(t) || types.IsErrType(t) {
		return true
	}
	if _, ok := types.IsClass(t); ok {
		return types.IsValueType(t) // @packed
	}
	if types.IsArray(t) {
		return true
	}
	if _, ok := types.IsEnum(t); ok {
		return true // 带数据 enum 按值承载 {tag, union}
	}
	return false
}

// emitSpawnThunkProtos 在原型段发射 spawn thunk 原型（static；定义在 TU 末尾）。
func (c *Ctx) emitSpawnThunkProtos() {
	for _, th := range c.spawnThunks {
		c.line("static void %s(void *);", th.name)
	}
	if len(c.spawnThunks) > 0 {
		c.line("")
	}
}
