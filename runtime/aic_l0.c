/* ============================================================================
 * AIC runtime L0 —— 核心层实现（核心设计 §五/§九/§十四）.
 * 单文件不超 400 行；区域分配器 / 守卫 / trap / str / 最短往返浮点 / print·os.
 * ==========================================================================*/
#include "aic_l0.h"

#include <float.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* --- stdout 二进制模式：Windows 文本模式会把 \n 改写成 \r\n，破坏 H3 逐位一致 --- */
/* --- stdout 二进制模式：平台相关代码统一收敛在 aic_plat.h（唯一平台层） --- */
#include "aic_plat.h"
static void aic_binary_stdout(void) { aic_plat_binary_stdout(); }

/* ============================================================================
 * 区域分配器（L3：**跨任务正确**）
 * ----------------------------------------------------------------------------
 * 每个区域实例 = 一条页链，bump 其中。区域 id **全局唯一、单调递增、永不复用**
 * （陈旧引用查全局表必得 AIC_REGION_DEAD → trap）。
 *
 * 为什么区域表必须**全局**而不是线程局部（L3 实测踩到的真缺陷）：
 * 容器/通道是**跨任务共享**的（§七「spawn 实参 = 共享引用」），子任务往父任务创建的
 * 通道/列表里写会触发"**在父任务的区域里增长**"。若页链按线程局部登记，子任务就会
 * 把共享容器的缓冲分配进自己的区域（任务一结束整条链交还全局空闲链）⇒ 父任务读到
 * 的是被回收复用的内存（实测：两个通道的值互相串成同一个）。故：
 *   - `hdr.reg` = **全局** id（0 = 特殊值"本任务的根区域"，由 t_root 解析）；
 *   - 页链与深度/_行号都放全局分块表（分块 = 永不搬移，守卫可无锁读）；
 *   - 分配用**每区域一把自旋锁**（自己区域无竞争；共享容器增长时才真竞争）。
 * ==========================================================================*/

typedef struct aic_page {
    struct aic_page *next;
    aic_usize used;
    aic_usize cap;
} aic_page;

#define AIC_PAGE_HDR     (32u)               /* align_up(sizeof(aic_page),16) */
#define AIC_PAGE_DEFAULT (64u * 1024u)
#define AIC_ALIGN        (16u)

/* 全局区域槽（分块存放，块一旦分配就不再搬移）。结构定义在 aic_l0.h（守卫要内联读）。 */
static aic_gslot   *aic_gchunk[AIC_GCHUNK_MAX];
aic_gslot         **aic_gtab = aic_gchunk;
static aic_mtx_t    aic_gtab_mtx;   /* 保护块分配与全局 id 分配器 */
static int          aic_gtab_ready = 0;
static aic_u32      aic_gnext = 1;  /* 全局 id 分配器（0 保留给"本任务根区域"） */

/* 线程局部：区域栈（栈顶 = 当前区域）、根区域 id、空闲页链、自旋计数。 */
AIC_TLS aic_u32         aic_depth = 0;
static AIC_TLS aic_u32  aic_region_ids[AIC_REGION_MAX_DEPTH + 1];
static AIC_TLS aic_u32  aic_root_gid = 0;
static AIC_TLS aic_page *aic_page_free = NULL;

/* 全局空闲页链（跨任务复用；锁在首个 aic_task_init 里建好）。 */
static aic_mtx_t aic_page_mtx;
static int       aic_page_mtx_ready = 0;
static aic_page *aic_page_free_global = NULL;

/* 区域自旋锁：tcc 不参与真并行（退化后端单线程跑完），故为空实现，
 * 从而**不需要**任何原子内建（设计 §七 的"唯一非 ISO 例外 = __atomic"只在
 * gcc/clang 侧用到）。 */
#if defined(__TINYC__)
static void rlock(aic_gslot *s) { (void)s; }
static void runlock(aic_gslot *s) { (void)s; }
#else
static void rlock(aic_gslot *s) {
    while (__sync_lock_test_and_set(&s->lock, 1u) != 0) {
        aic_thr_yield();
    }
}
static void runlock(aic_gslot *s) { __sync_lock_release(&s->lock); }
#endif

static void gtab_init(void) {
    if (!aic_gtab_ready) {
        /* 首个调用者必然是 main（单线程时刻）⇒ 惰性初始化无竞争。 */
        aic_mtx_init(&aic_gtab_mtx);
        aic_gtab_ready = 1;
    }
}

/* 取全局槽（块不存在则返回 NULL；无锁读，供守卫热路径用）。 */
static aic_gslot *gslot(aic_u32 gid) {
    aic_u32 ci = gid >> AIC_GCHUNK_SHIFT;
    if (ci >= AIC_GCHUNK_MAX) {
        return NULL;
    }
    aic_gslot *c = aic_gchunk[ci];
    if (c == NULL) {
        return NULL;
    }
    return &c[gid & (AIC_GCHUNK_SIZE - 1u)];
}

/* 取全局槽（不存在则建块；只有建块与 id 分配走全局锁）。 */
static aic_gslot *gslot_get(aic_u32 gid) {
    aic_gslot *s = gslot(gid);
    if (s != NULL) {
        return s;
    }
    gtab_init();
    aic_mtx_lock(&aic_gtab_mtx);
    aic_u32 ci = gid >> AIC_GCHUNK_SHIFT;
    if (ci < AIC_GCHUNK_MAX && aic_gchunk[ci] == NULL) {
        aic_gslot *c = (aic_gslot *)calloc(AIC_GCHUNK_SIZE, sizeof(aic_gslot));
        if (c == NULL) {
            aic_mtx_unlock(&aic_gtab_mtx);
            aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: region table>", 0);
        }
        aic_gchunk[ci] = c;
    }
    aic_mtx_unlock(&aic_gtab_mtx);
    s = gslot(gid);
    if (s == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: region table>", 0); /* 区域 id 空间耗尽 */
    }
    return s;
}

static aic_u32 gid_new(void) {
    gtab_init();
    aic_mtx_lock(&aic_gtab_mtx);
    aic_u32 gid = aic_gnext++;
    aic_mtx_unlock(&aic_gtab_mtx);
    return gid;
}

/* 诊断用：区域进入点（trap 消息的"创建处"）。 */
void aic_region_site_of(aic_u32 gid, const char **file, aic_u32 *line) {
    aic_gslot *s = gslot(gid);
    if (s == NULL) {
        if (file) *file = "<unknown>";
        if (line) *line = 0;
        return;
    }
    if (file) *file = s->file ? s->file : "<unknown>";
    if (line) *line = s->line;
}

/* 无 TLS 后端（tcc）的区域状态保存/恢复：见 aic_l0.h 的说明。 */
void aic_region_state_save(aic_region_state *st) {
    st->w[0] = aic_depth;
    st->w[1] = aic_root_gid;
    memcpy(&st->w[2], aic_region_ids, (AIC_REGION_MAX_DEPTH + 1) * sizeof(aic_u32));
}

void aic_region_state_restore(const aic_region_state *st) {
    aic_depth = st->w[0];
    aic_root_gid = st->w[1];
    memcpy(aic_region_ids, &st->w[2], (AIC_REGION_MAX_DEPTH + 1) * sizeof(aic_u32));
}

static aic_page *page_acquire(aic_usize payload_cap) {
    aic_page **pp = &aic_page_free;
    while (*pp != NULL) {
        aic_page *p = *pp;
        if (p->cap >= payload_cap) {
            *pp = p->next;
            p->next = NULL;
            p->used = 0;
            return p;
        }
        pp = &p->next;
    }
    /* 本线程的链不够 → 问**全局**空闲链（别的任务结束交还的页）。 */
    if (aic_page_mtx_ready) {
        aic_page *g = NULL;
        aic_mtx_lock(&aic_page_mtx);
        aic_page **gp = &aic_page_free_global;
        while (*gp != NULL) {
            if ((*gp)->cap >= payload_cap) {
                g = *gp;
                *gp = g->next;
                g->next = NULL;
                g->used = 0;
                break;
            }
            gp = &(*gp)->next;
        }
        aic_mtx_unlock(&aic_page_mtx);
        if (g != NULL) {
            return g;
        }
    }
    aic_page *p = (aic_page *)malloc(AIC_PAGE_HDR + payload_cap);
    if (p == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: allocator>", 0);
    }
    p->next = NULL;
    p->used = 0;
    p->cap = payload_cap;
    return p;
}

/* 页交还全局空闲链（跨任务复用是 spawn 循环不泄漏的前提）。 */
static void page_give_global(aic_page *p) {
    p->used = 0;
    aic_mtx_lock(&aic_page_mtx);
    p->next = aic_page_free_global;
    aic_page_free_global = p;
    aic_mtx_unlock(&aic_page_mtx);
}

void aic_region_release_task(void) {
    /* 只走本任务的区域栈（TLS）：根区域 + 当前仍压着的各层。 */
    for (aic_u32 d = 0; d <= aic_depth && d <= AIC_REGION_MAX_DEPTH; d++) {
        aic_u32 gid = aic_region_ids[d];
        aic_gslot *s = gslot(gid);
        if (s == NULL) {
            continue;
        }
        rlock(s);
        aic_page *p = (aic_page *)s->pages;
        while (p != NULL) {
            aic_page *nx = p->next;
            p->used = 0;
            p->next = aic_page_free;
            aic_page_free = p;
            p = nx;
        }
        s->pages = NULL;
        runlock(s);
        s->depth = AIC_REGION_DEAD;
    }
    while (aic_page_free != NULL) {
        aic_page *nx = aic_page_free->next;
        page_give_global(aic_page_free);
        aic_page_free = nx;
    }
    aic_depth = 0;
    aic_root_gid = 0;
}

/* 在指定区域 bump（跨任务可调用：锁住该区域）。 */
static void *region_bump(aic_u32 gid, aic_usize n, aic_u32 line) {
    aic_gslot *s = gslot_get(gid);
    if (s->depth == AIC_REGION_DEAD) {
        // **真实位点**：目标区域自己的进入点（`<alloc>` 这种伪路径 + 调用点行号会指错地方）。
        const char *rf = NULL;
        aic_u32 rl = 0;
        aic_region_site_of(gid, &rf, &rl);
        aic_trap_ctx(AIC_TRAP_REGION_ESCAPED, rf ? rf : "<runtime: region table>", (int)rl,
                     "allocation targets a region that has already been popped");
    }
    aic_usize need = (n + (AIC_ALIGN - 1u)) & ~(aic_usize)(AIC_ALIGN - 1u);
    if (need == 0) {
        need = AIC_ALIGN;
    }
    rlock(s);
    aic_page *p = (aic_page *)s->pages;
    if (p == NULL || p->used + need > p->cap) {
        aic_usize cap = need > AIC_PAGE_DEFAULT ? need : AIC_PAGE_DEFAULT;
        aic_page *np = page_acquire(cap);
        np->next = p;
        s->pages = np;
        p = np;
    }
    unsigned char *base = (unsigned char *)p + AIC_PAGE_HDR + p->used;
    p->used += need;
    runlock(s);
    return base;
}

void aic_task_init(const char *file, aic_u32 line) {
    gtab_init();
    if (!aic_page_mtx_ready) {
        /* 首个 aic_task_init 必然是 main（单线程时刻）⇒ 这里的惰性初始化无竞争。 */
        aic_mtx_init(&aic_page_mtx);
        aic_page_mtx_ready = 1;
    }
    /* 每个任务的根区域 = 一个**新的全局 id**（各任务的根互不相同）。 */
    aic_u32 gid = gid_new();
    aic_gslot *s = gslot_get(gid);
    s->depth = 0;
    s->line = line;
    s->file = file;
    s->pages = NULL;
    aic_depth = 0;
    aic_root_gid = gid;
    aic_region_ids[0] = gid;
}

void aic_region_push(const char *file, aic_u32 line) {
    aic_u32 nd = aic_depth + 1;
    if (nd >= AIC_REGION_MAX_DEPTH) {
        aic_trap(AIC_TRAP_STACK_OVERFLOW, file, (int)line);
    }
    aic_u32 gid = gid_new();
    aic_gslot *s = gslot_get(gid);
    s->depth = nd;
    s->line = line;
    s->file = file;
    s->pages = NULL;
    aic_depth = nd;
    aic_region_ids[nd] = gid;
}

void aic_region_pop(void) {
    if (aic_depth == 0) {
        return;
    }
    aic_u32 gid = aic_region_ids[aic_depth];
    aic_gslot *s = gslot(gid);
    if (s != NULL) {
        rlock(s);
        aic_page *p = (aic_page *)s->pages;
        while (p != NULL) {
            aic_page *nx = p->next;
            p->next = aic_page_free;
            aic_page_free = p;
            p = nx;
        }
        s->pages = NULL;
        runlock(s);
        s->depth = AIC_REGION_DEAD;
    }
    aic_depth--;
}

void *aic_alloc(aic_usize n, aic_u32 line) {
    return region_bump(aic_region_ids[aic_depth], n, line);
}

void *aic_alloc_hdr(aic_usize n, aic_u32 line) {
    aic_u32 reg = aic_region_ids[aic_depth];
    void *base = region_bump(reg, n, line);
    ((aic_hdr *)base)->reg = reg;
    ((aic_hdr *)base)->line = line;
    return base;
}

void *aic_alloc_live(aic_usize n, aic_u32 line) {
    /* 当前动态深度 > 0 时退一层：对象归外层区域，region 块弹出后仍存活。 */
    aic_u32 d = aic_depth > 0 ? aic_depth - 1 : 0;
    aic_u32 reg = aic_region_ids[d];
    void *base = region_bump(reg, n, line);
    ((aic_hdr *)base)->reg = reg;
    ((aic_hdr *)base)->line = line;
    return base;
}

/* reg = 0 是**约定值**："调用者所在任务的根区域"（运行时有 7 处这样用：
 * str 字节、os 参数、读文件缓冲…）。其余 reg 一律是全局区域 id。 */
void *aic_alloc_in(aic_u32 reg, aic_usize n, aic_u32 line) {
    if (reg == 0) {
        reg = aic_root_gid;
    }
    if (reg == 0) {
        aic_trap(AIC_TRAP_REGION_ESCAPED, "<runtime: scheduler>", (int)line); /* 调度层未初始化 */
    }
    return region_bump(reg, n, line);
}

/* 带头的定向分配（容器/通道在自身区域增长时用：头里的 reg 必须是**落点** id）。 */
void *aic_alloc_hdr_in(aic_u32 reg, aic_usize n, aic_u32 line) {
    if (reg == 0) {
        reg = aic_root_gid;
    }
    void *base = region_bump(reg, n, line);
    ((aic_hdr *)base)->reg = reg;
    ((aic_hdr *)base)->line = line;
    return base;
}

/* --- 存储点守卫（§五 R3）--------------------------------------------------- */
/* 慢路径入口（§10.4 第 1/4 条）：只在**内联比较判定违反**时进入，故标 cold+noreturn。 */
AIC_COLD_NORETURN void aic_cold_guard_report(void *obj, aic_u32 limit,
                                             const char *file, aic_u32 line) {
    aic_guard(obj, limit, file, line);
    abort(); /* aic_guard 命中即 abort；此处兜底保证 _Noreturn 语义成立 */
}

/* 反向差分（H9）：AIC_GUARD_ALWAYS=1 时任何守卫点都 trap，证明守卫没被优化掉。 */
AIC_COLD_NORETURN void aic_cold_guard_always(const char *file, aic_u32 line) {
    fprintf(stderr, "aic trap: guard always-fail mode (AIC_GUARD_ALWAYS)\n");
    fprintf(stderr, "  at %s:%u\n", file ? file : "<unknown>", (unsigned)line);
    fprintf(stderr, "  fix: this mode exists only to prove guards are still emitted\n");
    fflush(stderr);
    abort();
}

void aic_guard(void *obj, aic_u32 limit, const char *file, aic_u32 line) {
    if (obj == NULL) {
        return; /* 存 nil 合法 */
    }
    aic_hdr *h = (aic_hdr *)obj;
    aic_u32 reg = h->reg;
    aic_u32 d = aic_region_depth_of(reg);
    const char *rfile = "<unknown>";
    aic_u32 rline = 0;
    aic_region_site_of(reg, &rfile, &rline);
    if (d == AIC_REGION_DEAD) {
        fprintf(stderr, "aic trap: reference to a popped region\n");
        fprintf(stderr, "  at %s:%u\n", file ? file : "<unknown>", (unsigned)line);
        fprintf(stderr, "  object created at line %u; its region was entered at %s:%u (popped)\n",
                (unsigned)h->line, rfile, (unsigned)rline);
        fprintf(stderr, "  fix: add @live at the creation, or create it in the task region\n");
        fflush(stderr);
        abort();
    }
    if (d > limit) {
        fprintf(stderr, "aic trap: reference outlives its region\n");
        fprintf(stderr, "  at %s:%u\n", file ? file : "<unknown>", (unsigned)line);
        fprintf(stderr, "  object created at line %u (region depth %u > storage depth %u)\n",
                (unsigned)h->line, (unsigned)d, (unsigned)limit);
        fprintf(stderr, "  its region was entered at %s:%u\n", rfile, (unsigned)rline);
        fprintf(stderr, "  fix: add @live at the creation, or create it in an outer region\n");
        fflush(stderr);
        abort();
    }
}

/* ============================================================================
 * trap（§九：bug 不是错误）
 * ==========================================================================*/
static const char *trap_name(aic_trap_code code) {
    switch (code) {
    case AIC_TRAP_INDEX_OUT_OF_BOUNDS:  return "index out of bounds";
    case AIC_TRAP_DIVIDE_BY_ZERO:       return "division by zero";
    case AIC_TRAP_INTEGER_OVERFLOW:     return "integer overflow";
    case AIC_TRAP_NULL_DEREF:           return "null dereference";
    case AIC_TRAP_ASSERT_FAILED:        return "assertion failed";
    case AIC_TRAP_ZERO_CONTAINER_WRITE: return "write to a zero-value container";
    case AIC_TRAP_UNREACHABLE:          return "unreachable code";
    case AIC_TRAP_OUT_OF_MEMORY:        return "out of memory";
    case AIC_TRAP_STACK_OVERFLOW:       return "defer stack overflow";
    case AIC_TRAP_REGION_ESCAPED:       return "reference to a popped region";
    case AIC_TRAP_REGION_DEEPER:        return "reference outlives its region";
    case AIC_TRAP_CHAN_FULL:            return "bounded channel is full";
    case AIC_TRAP_DEADLOCK:             return "deadlock: every task is waiting and none can make progress";
    case AIC_TRAP_SHIFT_RANGE:          return "shift count is out of range";
    }
    return "unknown trap";
}

static const char *trap_fix(aic_trap_code code) {
    switch (code) {
    case AIC_TRAP_INDEX_OUT_OF_BOUNDS:
        return "check the index against the length; use len() as the loop bound";
    case AIC_TRAP_DIVIDE_BY_ZERO:
        return "test the divisor first: if d != 0 { ... }";
    case AIC_TRAP_INTEGER_OVERFLOW:
        return "widen the type with an explicit conversion, or range-check before the operation";
    case AIC_TRAP_NULL_DEREF:
        return "test for nil before dereferencing: if p != nil { ... }";
    case AIC_TRAP_ASSERT_FAILED:
        return "check whether the asserted condition really holds";
    case AIC_TRAP_ZERO_CONTAINER_WRITE:
        return "a zero-value container is read-only; initialise it: var xs T[] = []";
    case AIC_TRAP_UNREACHABLE:
        return "the match is exhaustive yet control reached here; check arm coverage";
    case AIC_TRAP_OUT_OF_MEMORY:
        return "allocate less; confirm objects promoted by @live really need the heap";
    case AIC_TRAP_STACK_OVERFLOW:
        return "region nesting or defer depth exceeded the static capacity; split the function";
    case AIC_TRAP_REGION_ESCAPED:
        return "add @live at the creation, or create the object in the task region";
    case AIC_TRAP_REGION_DEEPER:
        return "add @live at the creation, or create the object in an outer region";
    case AIC_TRAP_CHAN_FULL:
        return "receive before sending again, or size the channel for the peak queue (chan[T].new(cap))";
    case AIC_TRAP_DEADLOCK:
        return "check channel usage: a bounded channel that is full, a receive nobody will send to, or a select whose arms can never become ready (core design §16 N6/N11)";
    case AIC_TRAP_SHIFT_RANGE:
        return "keep the shift count below the left operand's width and non-negative; C leaves both cases undefined (core design §1)";
    }
    return "";
}

AIC_COLD_NORETURN void aic_trap_ctx(aic_trap_code code, const char *file, int line,
                                    const char *detail) {
    fflush(stdout);
    fprintf(stderr, "aic trap: %s\n", trap_name(code));
    // 位点：有真实行号就报 `file:line`；只有内部位点（调度/分配器/OS）时报
    // `<runtime: …>` 而**不**打 `:0`（`:0` 会让人以为源码第 0 行有问题）。
    if (line > 0) {
        fprintf(stderr, "  at %s:%d\n", file ? file : "<unknown>", line);
    } else if (file != NULL && file[0] == '<') {
        fprintf(stderr, "  at %s\n", file);
    } else {
        fprintf(stderr, "  at %s (runtime)\n", file ? file : "<runtime>");
    }
    if (detail != NULL) {
        fprintf(stderr, "  %s\n", detail);
    }
    fprintf(stderr, "  fix: %s\n", trap_fix(code));
    fflush(stderr);
    abort();
}

AIC_COLD_NORETURN void aic_trap(aic_trap_code code, const char *file, int line) {
    aic_trap_ctx(code, file, line, NULL);
}

/* ============================================================================
 * str
 * ==========================================================================*/
bool aic_str_eq(aic_str a, aic_str b) {
    if (a.len != b.len) {
        return false;
    }
    if (a.len == 0) {
        return true;
    }
    return memcmp(a.p, b.p, a.len) == 0;
}

aic_str aic_str_alloc(aic_usize n, aic_u32 line) {
    void *p = aic_alloc_in(0, n ? n : 1, line);
    return (aic_str){ (const char *)p, 0 };
}

/* Err.wrap(code, msg, cause)：把 cause **拷贝进当前区域**并挂上链（§16 N5 ④）。
 * 拷贝而不是存指针：cause 常是栈上的临时 Err（`return 0, Err.wrap(...)`），
 * 存指针会在函数返回后悬垂。节点随区域回收（§五 R1：无 GC/无 RC）。 */
aic_Err aic_err_wrap(aic_i32 code, aic_str msg, aic_Err cause) {
    aic_Err *node = (aic_Err *)aic_alloc_in(0, sizeof(aic_Err), 0);
    *node = cause;
    return (aic_Err){ code, msg, node };
}

/* 判空解引用（§九）：p == NULL 就 trap NULL_DEREF（带调用点位置），否则返回 p。
 * Err.cause 是语言级的"值"，但实现是指针 ⇒ 读它必须先过这道闸。 */
void *aic_deref_or_trap(void *p, const char *file, int line) {
    if (p == NULL) {
        aic_trap_ctx(AIC_TRAP_NULL_DEREF, file, line, "reading the cause of an error that has no cause");
    }
    return p;
}

/* 报告 cause 链（**stderr**）：第一级 msg，其后逐级 "  caused by: msg"。
 * 深度上限 8：链是显式构造的，不可能是环；上限只为防御性确定性（H3）。
 * 流必须是 stderr —— 语言规范：main 逃逸的 Err = 退出码 1 + **stderr** 打印；
 * 此前走 aic_print_str（stdout）与规范不符。 */
static void err_put(aic_str s) {
    if (s.len != 0) {
        fwrite(s.p, 1, s.len, stderr);
    }
}

void aic_err_report(aic_Err e) {
    int depth = 0;
    aic_plat_binary_stderr(); /* Windows 文本模式会把 \n 变 \r\n（文本要逐字节冻结） */
    err_put(e.msg);
    err_put((aic_str){ "\n", 1 });
    const aic_Err *p = e.cause;
    while (p != NULL && depth < 8) {
        err_put((aic_str){ "  caused by: ", 13 });
        err_put(p->msg);
        err_put((aic_str){ "\n", 1 });
        p = p->cause;
        depth++;
    }
    fflush(stderr);
}

/* 字节字典序比较（§三：str 的 < <= > >= 语义 = 字节序 = memcmp 序）。 */
int aic_str_cmp(aic_str a, aic_str b) {
    aic_usize n = a.len < b.len ? a.len : b.len;
    if (n != 0) {
        int c = memcmp(a.p, b.p, n);
        if (c != 0) {
            return c;
        }
    }
    if (a.len == b.len) {
        return 0;
    }
    return a.len < b.len ? -1 : 1;
}

aic_str aic_str_concat(aic_str a, aic_str b, aic_u32 line) {
    aic_usize n = a.len + b.len;
    char *p = (char *)aic_alloc_in(0, n ? n : 1, line);
    if (a.len != 0) {
        memcpy(p, a.p, a.len);
    }
    if (b.len != 0) {
        memcpy(p + a.len, b.p, b.len);
    }
    return (aic_str){ p, n };
}

/* ============================================================================
 * 值 → str（N2 字符串插值）：**唯一**一套格式化
 *
 * 判据（红线 20 / §15.6 C8）：整数十进制、浮点走 aic_f64_fmt 的最短往返、
 * bool = "true"/"false"、str = 自身。output 字节入任务区域（line = 源行）。
 * ==========================================================================*/

aic_str aic_fmt_str(aic_str s) { return s; }

aic_usize aic_u64_fmt(aic_u8 *buf, aic_u64 v) {
    aic_u8 digits[20];
    aic_usize d = 0;
    do {
        digits[d++] = (aic_u8)('0' + (aic_u8)(v % 10u));
        v /= 10u;
    } while (v != 0);
    aic_usize n = 0;
    while (d != 0) {
        buf[n++] = digits[--d];
    }
    return n;
}

aic_usize aic_i64_fmt(aic_u8 *buf, aic_i64 v) {
    aic_usize n = 0;
    aic_u64 u;
    if (v < 0) {
        buf[n++] = (aic_u8)'-';
        u = (aic_u64)(-(v + 1)) + 1u; /* 取绝对值：避免 INT64_MIN 取负溢出 */
    } else {
        u = (aic_u64)v;
    }
    return n + aic_u64_fmt(buf + n, u);
}

aic_str aic_fmt_i64(aic_i64 v, aic_u32 line) {
    aic_u8 buf[24];
    aic_usize n = aic_i64_fmt(buf, v);
    aic_str s = aic_str_alloc(n, line);
    if (n != 0) {
        memcpy((void *)s.p, buf, n);
    }
    return (aic_str){ s.p, n };
}

aic_str aic_fmt_u64(aic_u64 v, aic_u32 line) {
    aic_u8 buf[20];
    aic_usize n = aic_u64_fmt(buf, v);
    aic_str s = aic_str_alloc(n, line);
    if (n != 0) {
        memcpy((void *)s.p, buf, n);
    }
    return (aic_str){ s.p, n };
}

aic_str aic_fmt_f64(aic_f64 v, aic_u32 line) {
    char buf[40];
    aic_usize n = aic_f64_fmt(buf, v);
    aic_str s = aic_str_alloc(n, line);
    if (n != 0) {
        memcpy((void *)s.p, buf, n);
    }
    return (aic_str){ s.p, n };
}

aic_str aic_fmt_bool(bool v) {
    if (v) {
        return (aic_str){ "true", 4 };
    }
    return (aic_str){ "false", 5 };
}

/* ============================================================================
 * 最短往返浮点（红线 20）
 * ==========================================================================*/
aic_usize aic_f64_fmt(char buf[40], aic_f64 v) {
    if (v != v) { /* NaN */
        memcpy(buf, "nan", 3);
        return 3;
    }
    if (v > DBL_MAX) {
        memcpy(buf, "inf", 3);
        return 3;
    }
    if (v < -DBL_MAX) {
        memcpy(buf, "-inf", 4);
        return 4;
    }
    int p;
    for (p = 1; p <= 17; p++) {
        int n = snprintf(buf, 40, "%.*g", p, (double)v);
        if (n > 0 && n < 40 && strtod(buf, NULL) == v) {
            return (aic_usize)n;
        }
    }
    int n = snprintf(buf, 40, "%.17g", (double)v);
    return (aic_usize)(n > 0 ? n : 0);
}

/* ============================================================================
 * print 原语（polymorphic 分派由 emit 生成）
 * ==========================================================================*/
AIC_TLS aic_u32 aic_print_depth = 0;

void aic_print_str(aic_str s) {
    aic_binary_stdout();
    if (s.len != 0) {
        fwrite(s.p, 1, s.len, stdout);
    }
}

void aic_println_str(aic_str s) {
    aic_print_str(s);
    fputc('\n', stdout);
}

/* 整数打印：与 aic_fmt_* 同一套自实现十进制（无 libc printf、无裸 long）。 */
static void aic_put_u64(aic_u64 v) {
    aic_u8 buf[20];
    aic_usize n = aic_u64_fmt(buf, v);
    fwrite(buf, 1, n, stdout);
}

static void aic_put_i64(aic_i64 v) {
    aic_u8 buf[24];
    aic_usize n = aic_i64_fmt(buf, v);
    fwrite(buf, 1, n, stdout);
}

void aic_print_i32(aic_i32 v) { aic_binary_stdout(); aic_put_i64((aic_i64)v); }
void aic_println_i32(aic_i32 v) { aic_print_i32(v); fputc('\n', stdout); }
void aic_print_i64(aic_i64 v) { aic_binary_stdout(); aic_put_i64(v); }
void aic_println_i64(aic_i64 v) { aic_print_i64(v); fputc('\n', stdout); }
void aic_print_u32(aic_u32 v) { aic_binary_stdout(); aic_put_u64((aic_u64)v); }
void aic_println_u32(aic_u32 v) { aic_print_u32(v); fputc('\n', stdout); }
void aic_print_usize(aic_usize v) { aic_binary_stdout(); aic_put_u64((aic_u64)v); }
void aic_println_usize(aic_usize v) { aic_print_usize(v); fputc('\n', stdout); }

void aic_print_f64(aic_f64 v) {
    char buf[40];
    aic_usize n = aic_f64_fmt(buf, v);
    aic_binary_stdout();
    fwrite(buf, 1, n, stdout);
}

void aic_println_f64(aic_f64 v) { aic_print_f64(v); fputc('\n', stdout); }

void aic_print_bool(bool v) { aic_binary_stdout(); fputs(v ? "true" : "false", stdout); }
void aic_println_bool(bool v) { aic_print_bool(v); fputc('\n', stdout); }
void aic_println_nil(void) { aic_binary_stdout(); fputs("nil", stdout); fputc('\n', stdout); }

/* ============================================================================
 * os（同步最小面，D17）
 * ==========================================================================*/
static int aic_os_argc = 0;
static char **aic_os_argv = NULL;

void aic_os_init(int argc, char **argv) {
    aic_os_argc = argc;
    aic_os_argv = argv;
}

aic_usize aic_os_args_len(void) {
    return (aic_usize)(aic_os_argc < 0 ? 0 : aic_os_argc);
}

aic_str aic_os_args_at(aic_usize i) {
    if (i >= (aic_usize)aic_os_argc || aic_os_argv == NULL) {
        return (aic_str){ NULL, 0 };
    }
    const char *s = aic_os_argv[i];
    return (aic_str){ s, strlen(s) };
}

static aic_str read_all(FILE *f, aic_u32 line) {
    aic_usize cap = 8192;
    aic_usize len = 0;
    char *buf = (char *)malloc(cap);
    if (buf == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: os>", (int)line);
    }
    for (;;) {
        if (len == cap) {
            cap *= 2;
            char *nb = (char *)realloc(buf, cap);
            if (nb == NULL) {
                free(buf);
                aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: os>", (int)line);
            }
            buf = nb;
        }
        size_t got = fread(buf + len, 1, cap - len, f);
        len += got;
        if (got == 0) {
            break;
        }
    }
    aic_str out = aic_str_alloc(len, line);
    char *dst = (char *)out.p;
    if (len != 0) {
        memcpy(dst, buf, len);
    }
    free(buf);
    return (aic_str){ dst, len };
}

aic_str aic_os_read_file(aic_str path, aic_Err *err) {
    if (err != NULL) {
        *err = AIC_ERR_NONE;
    }
    char *p = (char *)aic_alloc_in(0, path.len + 1, 0);
    if (path.len != 0) {
        memcpy(p, path.p, path.len);
    }
    p[path.len] = 0;
    FILE *f = fopen(p, "rb");
    if (f == NULL) {
        if (err != NULL) {
            err->code = 2;
            /* 长度必须与字面量一致：少一位会把消息尾部吃掉（曾写 15 = 截掉末字母）。 */
            err->msg = (aic_str){ "cannot open file", 16 };
        }
        return (aic_str){ NULL, 0 };
    }
    aic_str s = read_all(f, 0);
    fclose(f);
    return s;
}

aic_str aic_os_read_stdin(aic_Err *err) {
    if (err != NULL) {
        *err = AIC_ERR_NONE;
    }
    return read_all(stdin, 0);
}

void aic_os_exit(aic_i32 code) {
    fflush(stdout);
    fflush(stderr);
    exit((int)code);
}
