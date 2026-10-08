package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// sync.Mutex 发射（核心设计 §七；运行时见 runtime/aic_l2.h 的 aic_mutex）。
//
//   sync.Mutex.new()  →  ((aic_mutex){ 0 })      /* 值类型零值 = 未加锁 */
//   m.lock()          →  aic_mutex_lock(&m)
//   m.unlock()        →  aic_mutex_unlock(&m)
// ---------------------------------------------------------------------------

// tryMutexCall 发射 sync.Mutex 的构造与方法调用。
func (c *Ctx) tryMutexCall(v *parse.Call, want types.Type) (string, bool, error) {
	if field, ok := v.Fn.(*parse.Field); ok && field.Name == "new" {
		if inner, isF := field.X.(*parse.Field); isF {
			if id, isID := inner.X.(*parse.Ident); isID && id.Name == "sync" && inner.Name == "Mutex" {
				c.need("l2")
				return "((aic_mutex){ 0 })", true, nil
			}
		}
	}
	field, ok := v.Fn.(*parse.Field)
	if !ok {
		return "", false, nil
	}
	if _, isMutex := c.ti(field.X).(*types.MutexT); !isMutex {
		return "", false, nil
	}
	switch field.Name {
	case "lock", "unlock":
		// 接收者必须是可取址的存储（变量/字段/下标）：取地址传给运行时。
		recv, err := c.expr(field.X, nil)
		if err != nil {
			return "", true, err
		}
		c.need("l2")
		return fmt.Sprintf("aic_mutex_%s(&%s)", field.Name, paren("", recv)), true, nil
	}
	return "", true, fmt.Errorf("emit: sync.Mutex has no method %s (line %d)", field.Name, v.Pos.Line)
}
