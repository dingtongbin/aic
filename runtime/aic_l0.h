/* ============================================================================
 * AIC runtime L0 —— 核心层（核心设计 §九）.
 * C11 only（唯一非 ISO 例外 = __atomic，圆 3 才引入；本层不用）。
 * 每个产物都链接本层：原语别名 / str / 区域栈与 bump 分配 / 对象头 / 守卫 /
 * trap / 最短往返浮点格式化 / print·os 原语（核心设计 §五、§九、§十四）。
 *
 * 区域模型（§五 R1–R5，R0c-② 修订版）：
 *   - 每任务一个区域栈；任务根 = 深度 0，region 块进入 push(+1)、退出 pop(−1)。
 *   - 堆对象头 aic_hdr = {u32 reg, u32 line}：reg = 区域实例 id（唯一、永不复用），
 *     line = 对象创建行号（trap 消息「创建处」的载体）。
 *   - 区域表 aic_regions[reg] 记录实例当前深度与进入行；弹出区域 = 深度记
 *     AIC_REGION_DEAD（∞）→ 悬垂引用与兄弟区域陈旧引用的存储守卫必 trap。
 *   - 守卫 aic_guard(obj, limit)：查表 + 一次深度比较（判空由调用方先做；
 *     被存对象必须是带头的引用对象）。
 *   - 区域内存只复位不归还（高水位缓存）→ 守卫读陈旧头安全（R0c-② F7）。
 * ==========================================================================*/
#ifndef AIC_L0_H
#define AIC_L0_H

/* 冷路径与分支提示属性（§10.4 第 1/4 条）：trap 与慢路径必须 cold + _Noreturn。
 * 必须定义在 aic_trap 声明之前（属性要用在声明上）。 */
#if defined(__TINYC__)
/* tcc：不认 noreturn/cold 的 GNU 属性形态，且不做优化 → 属性留空（语义不变）。 */
#define AIC_COLD
#define AIC_COLD_NORETURN
#define AIC_LIKELY(x) (x)
#define AIC_UNLIKELY(x) (x)
#elif defined(__GNUC__) || defined(__clang__)
#define AIC_COLD __attribute__((cold))
#define AIC_COLD_NORETURN __attribute__((cold, noreturn))
#define AIC_LIKELY(x) __builtin_expect(!!(x), 1)
#define AIC_UNLIKELY(x) __builtin_expect(!!(x), 0)
#else
#define AIC_COLD
#define AIC_COLD_NORETURN _Noreturn
#define AIC_LIKELY(x) (x)
#define AIC_UNLIKELY(x) (x)
#endif

#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>
#include <string.h>
/* 不 include <stdalign.h>：运行时从未用 alignas/alignof（布局靠声明序 + C 对齐
 * 规则），而 tcc 0.9.27 的 include/ 里没有这个 C11 头 —— 白拿的依赖会挡掉一整个
 * 配置（黄金执行规划 §十一：tcc 缺 C11 特性按单例降级；能不缺就不缺）。 */

/* --- 原语别名（§二.1）------------------------------------------------------ */
typedef int8_t   aic_i8;
typedef int16_t  aic_i16;
typedef int32_t  aic_i32;
typedef int64_t  aic_i64;
typedef uint8_t  aic_u8;
typedef uint16_t aic_u16;
typedef uint32_t aic_u32;
typedef uint64_t aic_u64;
/* aic_usize = 显式 64 位（核心设计 §16 N13 / P6）：与 usize 定案对齐，
 * 不跟随目标平台的 size_t 位宽（32 位目标不承诺）。 */
typedef uint64_t  aic_usize;
typedef ptrdiff_t aic_isize;
typedef float    aic_f32;
typedef double   aic_f64;

/* --- str：不可变值，字节恒任务区域（§二.3）---------------------------------
 * 视图 = 同一对 (p, len) 的再切片，永不悬垂。 */
typedef struct { const char *p; aic_usize len; } aic_str;

/* --- 对象头：8 字节（§五）-------------------------------------------------- */
typedef struct { aic_u32 reg; aic_u32 line; } aic_hdr;

/* 线程局部存储（§七 任务 = 独立区域根）：gcc/clang 用 C11 `_Thread_local`；
 * tcc 不支持任何 TLS（`_Thread_local`/`__thread` 均失败）→ 空宏，该配置退化为
 * 单线程后端（并发语料锚点写成与调度顺序无关，故五配置输出仍逐位一致）。 */
#if defined(__TINYC__)
#define AIC_TLS
/* 无 TLS 后端：区域状态是**全局**的 ⇒ 同线程跑任务时必须手动保存/恢复（见下）。 */
#define AIC_NO_TLS 1
#else
#define AIC_TLS _Thread_local
#define AIC_NO_TLS 0
#endif

/* --- 区域实例：**全局** id → 深度/进入行（弹出记 AIC_REGION_DEAD）------------
 * id 单调递增、永不复用（R0c-② F7：复用会让陈旧引用误判为活）；
 * 表按需分块扩容（含 region 的循环每轮消耗一个 id，静态表必溢出）。
 * file 记录区域进入点文件：trap 消息的「创建处」按「创建文件经区域进入点解析」取用。
 *
 * **L3：id 与页链都是全局的**（不再线程局部）—— 容器/通道跨任务共享，子任务往里写
 * 会"在父任务的区域里增长"，页链按线程局部登记会把共享缓冲分配进子任务区域并在其
 * 结束时回收（实测：两个通道的值互相串）。深度仍语义上属于"任务的区域栈"，但存在
 * 全局槽里，守卫因此可以无锁查（§五 R3）。 */
#define AIC_REGION_DEAD      0xFFFFFFFFu
#define AIC_REGION_MAX_DEPTH 256u

/* 全局区域槽（分块存放：块一旦分配就**永不搬移**，故守卫可无锁读）。
 * lock = 每区域一把自旋锁（自己区域无竞争；共享容器跨任务增长时才真竞争）。 */
typedef struct {
    aic_u32 lock;
    aic_u32 depth; /* AIC_REGION_DEAD = 已弹出 */
    aic_u32 line;
    const char *file;
    void *pages;   /* aic_page *（内部类型，头里不需要） */
} aic_gslot;

#define AIC_GCHUNK_SHIFT 16
#define AIC_GCHUNK_SIZE  (1u << AIC_GCHUNK_SHIFT) /* 65536 个区域实例/块 */
#define AIC_GCHUNK_MAX   256u                     /* 上限 16M 个区域实例 */

/* 分块指针数组（全局 id >> 16 → 块）。块不存在 = 该 id 从未分配。 */
extern aic_gslot **aic_gtab;

/* 线程局部：当前动态深度（任务根 = 0）；生成代码的守卫用 `aic_depth` 作 limit。 */
extern AIC_TLS aic_u32 aic_depth;

/* 守卫热路径内联读：区域实例当前深度（AIC_REGION_DEAD = 已弹出/不存在）。 */
static inline aic_u32 aic_region_depth_of(aic_u32 gid) {
    aic_gslot *c = aic_gtab[gid >> AIC_GCHUNK_SHIFT];
    if (c == NULL) {
        return AIC_REGION_DEAD;
    }
    return c[gid & (AIC_GCHUNK_SIZE - 1u)].depth;
}

/* 诊断：区域进入点（trap 消息的"创建处"）。 */
void aic_region_site_of(aic_u32 gid, const char **file, aic_u32 *line);

/* --- 无 TLS 后端的区域状态搬运（tcc）---------------------------------------
 * 该后端把任务**在同线程**上跑完（真并行会串区），而区域栈是全局的 ⇒ 任务体
 * 覆盖了调用者的区域状态，任务结束后调用者再分配就会落进已被弹出的区域
 * （实测：`reference to a popped region`）。故进任务前保存、出任务后恢复。 */
#define AIC_REGION_STATE_WORDS (AIC_REGION_MAX_DEPTH + 3)
typedef struct { aic_u32 w[AIC_REGION_STATE_WORDS]; } aic_region_state;
void aic_region_state_save(aic_region_state *st);
void aic_region_state_restore(const aic_region_state *st);

/* 区域页链与深度都在**全局**分块表里（跨任务共享的容器要在其自身区域增长），
 * 线程局部只有：区域栈（当前深度）、根区域 id、空闲页链、自旋计数 —— 见 aic_l0.c。 */

/* --- 区域栈与分配 ----------------------------------------------------------- */
/* aic_alloc       当前区域 bump 分配（不写头；用于容器 data 等内部存储）。
 * aic_alloc_hdr   分配并写对象头，返回的对象首字段即 aic_hdr。
 * aic_alloc_in    定向到指定区域 id（容器在其自身区域增长；**reg=0 = 本任务根区域**，
 *                 这是运行时的约定值：str 字节 / os 参数 / 读文件缓冲都用 0）。
 * aic_alloc_hdr_in 带头的定向分配（头里的 reg 必须写**落点** id）。
 * aic_str_alloc   任务区域分配字节（str 字节恒任务区域）。 */
void *aic_alloc(aic_usize n, aic_u32 line);
void *aic_alloc_hdr(aic_usize n, aic_u32 line);
/* @live（§五 R4）：在**外一层**区域分配，使对象活过当前 region 块的弹出。
 * 深度 0（任务区域）时就是任务区域本身（已在最外层，等价于普通分配）。 */
void *aic_alloc_live(aic_usize n, aic_u32 line);
void *aic_alloc_in(aic_u32 reg, aic_usize n, aic_u32 line);
void *aic_alloc_hdr_in(aic_u32 reg, aic_usize n, aic_u32 line);
void  aic_region_push(const char *file, aic_u32 line);
void  aic_region_pop(void);
void  aic_task_init(const char *file, aic_u32 line); /* main 入口：建任务根区域 */
/* 任务结束：把本任务的区域页交还**全局**空闲链（跨任务复用）。
 * 不交还的话每个 spawn 泄漏一整条页链 —— spawn 循环下是致命的（L3 的必需项）。 */
void  aic_region_release_task(void);

/* 守卫：被存引用对象必须活且其区域深度 ≤ limit（§五 R3 存储点 ⓪–⑥）。
 * obj 指向带头的引用对象；调用方须先判空。违反 = trap（含存储点/创建行）。 */
void aic_guard(void *obj, aic_u32 limit, const char *file, aic_u32 line);

/* --- trap：bug 不是错误（§九）---------------------------------------------- */
typedef enum {
    AIC_TRAP_INDEX_OUT_OF_BOUNDS  = 1,
    AIC_TRAP_DIVIDE_BY_ZERO       = 2,
    AIC_TRAP_INTEGER_OVERFLOW     = 3,
    AIC_TRAP_NULL_DEREF           = 4,
    AIC_TRAP_ASSERT_FAILED        = 5,
    AIC_TRAP_ZERO_CONTAINER_WRITE = 6,
    AIC_TRAP_UNREACHABLE          = 7,
    AIC_TRAP_OUT_OF_MEMORY        = 8,
    AIC_TRAP_STACK_OVERFLOW       = 9,
    AIC_TRAP_REGION_ESCAPED       = 10, /* 悬垂引用：对象所在区域已弹出 */
    AIC_TRAP_REGION_DEEPER        = 11  /* 引用区域比存储更深：需 @live */
    , AIC_TRAP_CHAN_FULL           = 12  /* 有界 channel 满：真并发下阻塞，无人能收则 trap */
    , AIC_TRAP_DEADLOCK            = 13  /* 全体任务都在等待且无进展：真死锁 */
    , AIC_TRAP_SHIFT_RANGE         = 14  /* 移位计数 ≥ 位宽（或为负）：C 里是 UB */
} aic_trap_code;

/* file/line 经 #line 回指 .aic 源（红线 9）。trap 退出码非 0。 */
AIC_COLD_NORETURN void aic_trap(aic_trap_code code, const char *file, int line);
/* 带一行上下文的 trap（如死锁的"谁在等什么"）：detail 为 NULL 时与 aic_trap 等价。 */
AIC_COLD_NORETURN void aic_trap_ctx(aic_trap_code code, const char *file, int line,
                                    const char *detail);

#define AIC_IDX_CHECK(i, n, file, line) \
    do { if ((aic_usize)(i) >= (aic_usize)(n)) \
         aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); } while (0)

#define AIC_DIV_CHECK(d, file, line) \
    do { if ((d) == 0) aic_trap(AIC_TRAP_DIVIDE_BY_ZERO, file, line); } while (0)

/* 移位范围（§一「移位语义」）：计数 ≥ 左操作数位宽 = trap；有符号负计数 = trap
 * （C 里两者都是 UB）。计数为无符号时负值不可能，故按有符号比较也无害。 */
#define AIC_SHIFT_CHECK(c, width, file, line) \
    do { if ((aic_i64)(c) < 0 || (aic_u64)(c) >= (aic_u64)(width)) \
         aic_trap(AIC_TRAP_SHIFT_RANGE, file, line); } while (0)

#define AIC_NONNULL(p, file, line) \
    do { if ((p) == NULL) aic_trap(AIC_TRAP_NULL_DEREF, file, line); } while (0)

/* 零值容器 = NULL 句柄：读作空、写即 trap（§二.3 两个状态）。 */
#define AIC_LIST_WRITABLE(handle, file, line) \
    do { if ((handle) == NULL) \
         aic_trap(AIC_TRAP_ZERO_CONTAINER_WRITE, file, line); } while (0)

/* 定长数组下标（§二.3：`[T;N]` 内联值，越界 trap；004/112 锚点）。
 * 形态 = 内联比较 + cold 不返回报告（§10.4 第 1 条），**左值可用**（读与写同一形态：
 * 三目两端同为左值 ⇒ 整个表达式是左值，`AT(a) = v` 合法）。
 * n == 0 时 `i < n` 恒假 ⇒ 只可能走 trap 分支，`(p)[0]` 是死代码（不执行，
 * 只为让两支同型）；下标值在 AIR 里恒为无副作用的临时量（V 系规则），双求值安全。 */
#define AIC_ARRAY_AT(p, n, i, file, line) \
    (*((aic_usize)(i) < (aic_usize)(n) ? &(p)[i] \
     : (aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line), &(p)[0])))

/* 存储点守卫的发射形态（§五 R3 + §10.4 第 1 条）：**内联比较 + cold 不返回报告**。
 * emit 在每个被标守卫的存储点生成 AIC_GUARD(obj, limit, file, line)；
 * 形态定义在 aic_guard.h（H10 断言对象：产物里不得出现通用外部调用形态）。
 * 复合值（带引用负载的 enum / [T;N] of class）按下标/字段逐个发射本宏。 */
#include "aic_guard.h"

/* --- T[]：引用对象（头 + data/len/cap），句柄 = 指向本结构的指针，零值 = NULL --
 * 句柄共享；增长在列表自身区域重分配（aic_alloc_in）。 */
#define AIC_LIST_OF(short, ctype) \
    typedef struct aic_list_##short { \
        aic_hdr hdr; ctype *data; aic_usize len; aic_usize cap; \
    } aic_list_##short;

AIC_LIST_OF(i8, aic_i8)
AIC_LIST_OF(i16, aic_i16)
AIC_LIST_OF(i32, aic_i32)
AIC_LIST_OF(i64, aic_i64)
AIC_LIST_OF(u8, aic_u8)
AIC_LIST_OF(u16, aic_u16)
AIC_LIST_OF(u32, aic_u32)
AIC_LIST_OF(u64, aic_u64)
AIC_LIST_OF(usize, aic_usize)
AIC_LIST_OF(f32, aic_f32)
AIC_LIST_OF(f64, aic_f64)
AIC_LIST_OF(bool, bool)
AIC_LIST_OF(str, aic_str)
AIC_LIST_OF(Box, void *) /* 用户类指针 / 其他列表指针按 void* 槽位复用 */

/* --- Err：按值，code 0 = 无错（nil）（§六 / §16 N5 ④ ------------------------
 * 结构 = {code i32, msg str, cause Err*}：**cause 链**由 `Err.wrap(code,msg,cause)`
 * 建立（节点在当前区域分配，生命周期随区域，§五 R1）。`?` 只**原样传播**，不伪造链
 * （设计里 Err 没有位置字段，"原始出错位置"由 msg 文本与 trap 位点承担）。 */
typedef struct aic_Err { aic_i32 code; aic_str msg; const struct aic_Err *cause; } aic_Err;
#define AIC_ERR_NONE (aic_Err){ 0, (aic_str){ 0, 0 }, NULL }

/* 构造一个带上链的 Err：cause 会被**拷贝进当前区域**（调用方无需关心栈变量寿命）。 */
aic_Err aic_err_wrap(aic_i32 code, aic_str msg, aic_Err cause);

/* 报告 cause 链（**stderr**：§语言规范「main 内逃逸 = 退出码 1 + stderr」）：
 * 第一级 msg，其后逐级 "  caused by: msg"。生成的 main 走 `-> Err` 时调它。 */
void aic_err_report(aic_Err e);

/* 判空解引用（§九：疑 nil = 运行时 trap）。返回 p，p 为 NULL 则 trap
 * （消息含调用点位置）。用于语言级"值语义的指针"—— Err.cause 的读取。 */
void *aic_deref_or_trap(void *p, const char *file, int line);

/* --- interface：两机器字 {data, vt}（§四）----------------------------------
 * vt = 见证表首地址（表 = const void* 数组，槽位 = 接口方法声明序；§十）。
 * 类型写成「指向指针数组」而非 void*：void* 不能取下标（vt[槽] 非法）。 */
typedef struct { void *data; const void *const *vt; } aic_iface;

AIC_LIST_OF(iface, aic_iface) /* 接口元素列表：两字结构，不能塞 void* 槽（§四） */

/* --- str 基础 --------------------------------------------------------------- */
bool    aic_str_eq(aic_str a, aic_str b);
/* 字节字典序比较：< 0 / 0 / > 0（核心设计 §三：str 的 < <= > >= 用字节序）。 */
int     aic_str_cmp(aic_str a, aic_str b);
aic_str aic_str_alloc(aic_usize n, aic_u32 line);   /* 任务区域分配字节 */
aic_str aic_str_concat(aic_str a, aic_str b, aic_u32 line);

/* --- 最短往返浮点（红线 20：禁 libc printf 直接格式化）----------------------
 * 精度搜索 1..17：%.{p}g 写缓冲 → strtod 回读比对，首个无损精度即最短。
 * inf/-inf/nan 按规范打印。 */
aic_usize aic_f64_fmt(char buf[40], aic_f64 v);

/* 整数十进制写入栈缓冲，返回字节数（**不用 libc printf**：跨 libc 的整数格式化
 * 虽无差异，但统一走自实现可让「产物与运行时的数字形态」只有一处实现）。
 * buf 容量下界：u64 最多 20 位、i64 最多 20 位 + 符号。 */
aic_usize aic_u64_fmt(aic_u8 *buf, aic_u64 v);
aic_usize aic_i64_fmt(aic_u8 *buf, aic_i64 v);

/* --- 值 → str（N2 字符串插值的落点；**唯一**一套格式化，禁第二份）-----------
 * 语义与 print 族逐条一致：整数十进制、浮点走 aic_f64_fmt 的最短往返、
 * bool 为 "true"/"false"、str 为自身。字节入任务区域（line = 源行）。 */
aic_str aic_fmt_str(aic_str s);
aic_str aic_fmt_i64(aic_i64 v, aic_u32 line);
aic_str aic_fmt_u64(aic_u64 v, aic_u32 line);
aic_str aic_fmt_f64(aic_f64 v, aic_u32 line);
aic_str aic_fmt_bool(bool v);

/* --- print 原语（std/print 的落点；polymorphic 分派由 emit 生成）------------- */
void aic_print_str(aic_str s);
void aic_println_str(aic_str s);
void aic_print_i64(aic_i64 v);
void aic_println_i64(aic_i64 v);
void aic_print_i32(aic_i32 v);
void aic_println_i32(aic_i32 v);
void aic_print_u32(aic_u32 v);
void aic_println_u32(aic_u32 v);
void aic_print_usize(aic_usize v);
void aic_println_usize(aic_usize v);
void aic_print_f64(aic_f64 v);
void aic_println_f64(aic_f64 v);
void aic_print_bool(bool v);
void aic_println_bool(bool v);
void aic_println_nil(void);

/* 打印嵌套深度（println 容器嵌套上限 4 层，超出打 …，§十四）。
 * **线程局部**：真并发下每个任务有自己的深度计数（否则并行打印会互相踩）。 */
extern AIC_TLS aic_u32 aic_print_depth;

/* --- os（同步最小面，D17；两段式异步属圆 3）-------------------------------- */
void     aic_os_init(int argc, char **argv);
aic_usize aic_os_args_len(void);
aic_str   aic_os_args_at(aic_usize i);
aic_str   aic_os_read_file(aic_str path, aic_Err *err);
aic_str   aic_os_read_stdin(aic_Err *err);
void      aic_os_exit(aic_i32 code);

#endif /* AIC_L0_H */
