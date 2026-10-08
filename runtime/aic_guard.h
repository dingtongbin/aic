/* ============================================================================
 * 守卫的发射形态（核心设计 §五 R3 末段 + §10.4 第 1 条，H10 断言对象）。
 *
 * 铁律：**热路径检查 = 内联比较 + cold 不返回报告函数**，禁止发成通用外部调用
 * （外部调用会被优化器当成内存屏障，这是「语义等价换法」里收益最大的单点）。
 * 故本宏只做「判空 + 一次深度比较」，慢路径交给 aic_cold_guard_report。
 *
 * 模式（AIC_GUARD_MODE，只影响**静态已证明冗余**的守卫；三档行为必须逐位一致）：
 *   all    = 全部发射（默认）
 *   static = 跳过检查器已证安全的点（静态免检档）
 *   none   = 全部免检（**仅门禁差分断言用，不入交付**，O12）
 * AIC_GUARD_ALWAYS=1 把守卫改成恒 trap（H9 反向差分：证明守卫确实在被保护的点位上）。
 * ==========================================================================*/
#define AIC_GUARD_MODE_ALL 0
#define AIC_GUARD_MODE_STATIC 1
#define AIC_GUARD_MODE_NONE 2

#ifndef AIC_GUARD_MODE
#define AIC_GUARD_MODE AIC_GUARD_MODE_ALL
#endif

#if defined(AIC_GUARD_ALWAYS) && AIC_GUARD_ALWAYS
/* 反向差分：任何守卫点都 trap（用于验证守卫没有被优化掉）。 */
#define AIC_GUARD(obj, limit, file, line) \
    do { if ((obj) != NULL) aic_cold_guard_always(file, line); } while (0)
#elif AIC_GUARD_MODE == AIC_GUARD_MODE_NONE
#define AIC_GUARD(obj, limit, file, line) \
    do { (void)(obj); (void)(limit); (void)(file); (void)(line); } while (0)
#else
/* 热路径：内联判空 + 一次深度比较；违反走 cold 不返回报告函数。 */
#define AIC_GUARD(obj, limit, file, line)                                    \
    do {                                                                     \
        void *aic_g_obj_ = (void *)(obj);                                    \
        if (aic_g_obj_ != NULL) {                                            \
            aic_u32 aic_g_d_ = aic_guard_depth(aic_g_obj_);                  \
            if (AIC_UNLIKELY(aic_g_d_ > (aic_u32)(limit))) {                 \
                aic_cold_guard_report(aic_g_obj_, (aic_u32)(limit),          \
                                      (file), (aic_u32)(line));              \
            }                                                                \
        }                                                                    \
    } while (0)
#endif

/* 内联深度读（热路径：不经过通用调用，只查全局分块表的槽）。 */
static inline aic_u32 aic_guard_depth(void *obj) {
    return aic_region_depth_of(((aic_hdr *)obj)->reg);
}

/* 慢路径：不返回（_Noreturn + cold，§10.4 第 4 条）。 */
AIC_COLD_NORETURN void aic_cold_guard_report(void *obj, aic_u32 limit,
                                             const char *file, aic_u32 line);
AIC_COLD_NORETURN void aic_cold_guard_always(const char *file, aic_u32 line);
