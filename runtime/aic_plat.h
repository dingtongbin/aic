/* AIC runtime —— **唯一的平台层**（核心设计 §十四 可移植性纪律）。
 *
 * 纪律（用户直令 2026-10）：**必须用可移植 C；任意平台代码均需隔离**。
 * 本文件是 runtime 里**唯一**允许出现 `_WIN32` / `windows.h` / `pthread.h` 的地方；
 * 其余 runtime 与生成代码一律 ISO C11，不得直接调用平台 API。
 *
 * 后端（按事实选择，记录在案）：
 *   - POSIX / mingw（gcc、clang）：pthreads + clock_gettime + nanosleep
 *   - Windows（含 tcc，其 windows.h 无 CONDITION_VARIABLE）：Win32 子集
 *     CreateThread / WaitForSingleObject / CRITICAL_SECTION / Sleep / QueryPerformanceCounter
 *
 * 为什么不用 C11 `<threads.h>` / `<time.h>` 的 timespec_get：本机三套工具链都没有
 * `threads.h`（mingw 的 winpthreads 不提供 C11 threads），且 tcc 不支持任何 TLS。
 * 故线程与高精度时钟统一走本层，语义在各平台**一致**（同一套毫秒口径）。
 *
 * 说明：`AIC_TLS` 在 tcc 下为空宏（该配置退化为单线程后端）；并发语料锚点必须写成
 * 与调度顺序无关，故五配置输出仍逐位一致。
 */
#ifndef AIC_PLAT_H
#define AIC_PLAT_H

#include "aic_l0.h"

/* stdio 要在 windows.h 之前显式包含：本层用 stdout / _fileno / _setmode
 * （二进制 stdout），自足才能被任何 TU 直接包含。 */
#include <stdio.h>

/* --------------------------------------------------------------------------
 * 线程与互斥
 * ------------------------------------------------------------------------*/
#if defined(_WIN32)

#include <windows.h>

typedef HANDLE aic_thr;
typedef CRITICAL_SECTION aic_mtx_t;

/* 线程入口签名两平台不同：Win32 = DWORD(__stdcall *)(LPVOID)；POSIX = void *(*)(void *)。 */
typedef DWORD(__stdcall *aic_thr_fn)(LPVOID);
#define AIC_THR_ENTRY(name) static DWORD __stdcall name(LPVOID raw)
#define AIC_THR_RETURN() return 0

static inline int aic_thr_create(aic_thr *t, aic_thr_fn fn, void *arg) {
    DWORD id = 0;
    *t = CreateThread(NULL, 0, fn, arg, 0, &id);
    return *t != NULL;
}
static inline void aic_thr_join(aic_thr t) {
    WaitForSingleObject(t, INFINITE);
    CloseHandle(t);
}
static inline void aic_mtx_init(aic_mtx_t *m) { InitializeCriticalSection(m); }
static inline void aic_mtx_lock(aic_mtx_t *m) { EnterCriticalSection(m); }
static inline void aic_mtx_unlock(aic_mtx_t *m) { LeaveCriticalSection(m); }
static inline void aic_thr_yield(void) { Sleep(0); }

/* 单调毫秒：QueryPerformanceCounter（与 POSIX 侧同口径：毫秒、单调） */
static inline aic_i64 aic_plat_monotonic_ms(void) {
    LARGE_INTEGER f, c;
    QueryPerformanceFrequency(&f);
    QueryPerformanceCounter(&c);
    return (aic_i64)((c.QuadPart / (f.QuadPart / 1000)) + ((c.QuadPart % (f.QuadPart / 1000)) * 1000 / (f.QuadPart / 1000)));
}
/* 墙钟毫秒（Unix epoch） */
static inline aic_i64 aic_plat_now_ms(void) {
    FILETIME ft;
    ULARGE_INTEGER u;
    GetSystemTimeAsFileTime(&ft);
    u.LowPart = ft.dwLowDateTime;
    u.HighPart = ft.dwHighDateTime;
    return (aic_i64)(u.QuadPart / 10000ULL) - 11644473600000LL; /* 1601→1970 */
}
static inline void aic_plat_sleep_ms(aic_i64 ms) {
    if (ms > 0) Sleep((DWORD)ms);
}

#else /* POSIX */

#include <pthread.h>
#include <sched.h>
#include <time.h>

typedef pthread_t aic_thr;
typedef pthread_mutex_t aic_mtx_t;

typedef void *(*aic_thr_fn)(void *);
#define AIC_THR_ENTRY(name) static void *name(void *raw)
#define AIC_THR_RETURN() return NULL

static inline int aic_thr_create(aic_thr *t, aic_thr_fn fn, void *arg) {
    return pthread_create(t, NULL, fn, arg) == 0;
}
static inline void aic_thr_join(aic_thr t) { pthread_join(t, NULL); }
static inline void aic_mtx_init(aic_mtx_t *m) { pthread_mutex_init(m, NULL); }
static inline void aic_mtx_lock(aic_mtx_t *m) { pthread_mutex_lock(m); }
static inline void aic_mtx_unlock(aic_mtx_t *m) { pthread_mutex_unlock(m); }
static inline void aic_thr_yield(void) { sched_yield(); }

static inline aic_i64 aic_plat_monotonic_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (aic_i64)ts.tv_sec * 1000 + (aic_i64)(ts.tv_nsec / 1000000);
}
static inline aic_i64 aic_plat_now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (aic_i64)ts.tv_sec * 1000 + (aic_i64)(ts.tv_nsec / 1000000);
}
static inline void aic_plat_sleep_ms(aic_i64 ms) {
    if (ms > 0) {
        struct timespec ts;
        ts.tv_sec = (time_t)(ms / 1000);
        ts.tv_nsec = (long)((ms % 1000) * 1000000);
        nanosleep(&ts, NULL);
    }
}

#endif

/* --------------------------------------------------------------------------
 * 标准输出：Windows 文本模式会把 \n 写成 \r\n，直接破坏 H3 逐位一致
 * ------------------------------------------------------------------------*/
#if defined(_WIN32)
#include <fcntl.h>
#include <io.h>
static inline void aic_plat_binary_stdout(void) {
    static int done = 0;
    if (!done) {
        _setmode(_fileno(stdout), _O_BINARY);
        done = 1;
    }
}

/* stderr 同样要二进制模式：Windows 文本模式会把 `\n` 改写成 `\r\n`，
 * 而 Err 报告是**逐字节冻结**的语言级文本（H3 的确定性口径）。 */
static inline void aic_plat_binary_stderr(void) {
    static int done = 0;
    if (!done) {
        _setmode(_fileno(stderr), _O_BINARY);
        done = 1;
    }
}
#else
static inline void aic_plat_binary_stdout(void) { }
static inline void aic_plat_binary_stderr(void) { }
#endif

#endif /* AIC_PLAT_H */
