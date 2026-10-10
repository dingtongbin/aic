# AIC 基准与内存报告（ benches/report.md ）

> 数据由 `scripts/bench_full.py`（时间）+ `scripts/bench_mem.py`（内存 + UBSan）
> 实测产出，可复跑：`python -X utf8 scripts/bench_full.py` / `scripts/bench_mem.py`
> **方法论**：同算法、同输出（数值等价），AIC/C/Rust **严格交替** 9 次取样本；
> C = 标准 C（gcc 16.2 `-std=c11 -O2`）= **1.00× 基线**；Rust = `rustc -O`。
> min 是性能的稳健估计（噪声只会让时间变长）；mean/stdev 描述稳定性。
>
> **两条测量口径纪律（2026-10 发现并修正，此前报告数字虚高，是假差距）**：
> 1. **AIC 侧必须与 C/Rust 同优化档**。CLI 原来没有透传 flag 的入口，`aic build`
>    产的是 `-O0`，而 C 基线是 `-O2` —— 于是 B6-fib 账面 **3.20×**，同档后 **1.11×**。
>    现在 `AIC_CFLAGS="-O2"` 可传 flag；`bench_full.py` / `bench_mem.py` 已内置同档
>    （门禁走自己的四配置，不经这条路径，H2/H3 判据不受影响）。
> 2. **C 基线不许被编译器折叠掉**。B9-dispatch 的 C 基线里 `drive()` 被 gcc -O2
>    经 IPA 常量传播 + 投机内联后**整个循环被消掉**（循环一次都不执行，基线只剩
>    printf 与进程启动）⇒ 账面 3.00×，真值 **1.06×**。修正：vt/data 放 volatile
>    局部，编译器必须每轮重新装载函数指针（语义不变，见 `benches/c/b9_dispatch.c`
>    头注的完整踩坑记录）。

## 一、时间表（default 规模，9 次交替，取 min）

| bench | AIC min | C min | Rust min | **AIC/C** | AIC/Rust | 输出一致 |
|---|---|---|---|---|---|---|
| B1-matmul 512³ | 240.0 ms | 200.9 ms | 275.8 ms | **1.19×** | 0.87× | yes |
| B1-sieve 1e8 | 1211.6 ms | 1054.3 ms | 1255.7 ms | **1.15×** | 0.96× | yes |
| B2-json 1MB | 13.4 ms | 12.6 ms | 37.0 ms | **1.06×** | 0.36× | yes |
| B4-graph 1e6 节点 | 40.4 ms | 93.7 ms | 2674.6 ms | **0.43×（AIC 快 2.3×）** | 0.02× | yes |
| B6-fib 42 | 753.1 ms | 680.4 ms | 1110.9 ms | **1.11×** | 0.68× | yes |
| B7-string | 10.4 ms | 10.1 ms | 35.0 ms | **1.03×** | 0.30× | yes |
| B8-mapset 200k | 187.8 ms | 167.1 ms | 71.8 ms | **1.12×** | 2.62× | yes |
| B9-dispatch 2e6 | 18.9 ms | 17.8 ms | 35.6 ms | **1.06×** | 0.53× | yes |

**总判读**：**八个 bench 全部落在 0.43×–1.19×**，即与标准 C 同档持平；AIC 只在
B4-graph 明显更快（2.3×，区域整块回收 vs 逐节点 malloc/free），只在
B1-matmul（1.19×）、B1-sieve（1.15×）、B8-mapset（1.12×）小幅落后。
Rust 在 B8-mapset 上仍快 2.62×（标准库哈希容器），其余 7 项 AIC 均快于 Rust。

**抖动**：AIC 在 B1-matmul/B6-fib 上最稳（1–3%）；C 侧个别样本 max 出现 3–5 秒毛刺
（后台扫描/线程迁移，B4-graph/br 的 C 样本 max = 4818ms 而 min = 93.7ms）。
**一律以 min 判读，不用 mean**。

## 二、内存表（峰值 RSS = 运行中轮询工作集，×C）

| case | AIC 峰值 | C 峰值 | Rust 峰值 | AIC/C | 说明 |
|---|---|---|---|---|---|
| B4-graph 1e6 节点同持 | 34 MB | 34 MB | 39 MB | **1.00×** | 峰值严格持平 |
| B5-region 2M 轮建/回收 | 4 MB | 4 MB | 5 MB | **1.00×** | 长跑无泄漏 |
| B10-endurance 300 轮三重复合 | 4 MB | 4 MB | — | **1.00×** | region+容器+spawn 复合长跑 |
| UBSan（clang `-fsanitize=undefined`） | 零报告 | — | — | — | B4/B5/B10 三过 |

**B4 的 RSS 全程追踪（12M 节点，psapi 50ms 采样，判「缓存收敛」vs「真泄漏」）**：

| 时点 | AIC | C |
|---|---|---|
| 爬坡段（节点分配） | 1.9 → 370.5 MB | 1.9 → 370.5 MB |
| **峰值** | **371 MB** | **371 MB** |
| 卸载后（区域弹出 / 逐个 free 完成） | **6.0 MB** | **35.4 MB** |

两侧爬坡与峰值**逐点重合**（371MB = 同一批 12M Node）；差异只在卸载之后：
AIC 回落到 6.0MB，C 因 glibc 堆碎片只回到 35.4MB（**AIC 归还率高 5.9×**）。
所以 `bench_mem.py` 里"B4 尾半 +19MB"是脚本把**构建期爬坡**当作了增长（短跑程的
首半/尾半切法失真），**不是高水位缓存、更不是泄漏**；真泄漏判据是 B5/B10 两个循环型
bench 的尾半是否变平 —— 两者 +0.3/+0.5MB（C 分别 +1.4/+1.5MB，AIC 反而更平）。

## 三、分代区域（R19）与空转成本

**R19 分代区域**：区域实例身份从"全局单调 id（2B/实例、永不回收）"换成
**（槽号, 代龄）** —— 对象头不变（8B）：`reg = slot:12 | gen:20`。区域槽 =
4096 个固定槽 + Treiber 无锁空闲链；push 取槽后 `gen++`，用一次 `u32` 存储
换发整槽状态（守卫无锁读因此相干：一次加载同时看 gen/depth/dead）；pop 回滚
bump 后把槽标 DEAD 归还。陈旧引用 = 头里 gen 与槽位 gen 不符 ⇒ 与旧形态
`depth = 0` 同义 trap（`runtime/smoke/smoke_recycle.c` 是判别性锚点：内存未被
覆写的旧一代对象必 trap，只看 depth 会放过）。容量 4096 × 2^20 ≈ **4.3G 实例**
（旧上限 16M）；记账 = 32B ×（存活 + 已退役）⇒ B5 2M 轮只退役 2 个槽 = 64B。
region 空转：单对 push+pop 62.4ns → **28.1ns**（`runtime/smoke/smoke_threads.c`
四线程 × 2 万轮压力锚点，stdout 逐位确定）。

## 四、协程（A1/T1）与容器层

**协程 = 真用户态协程**：Windows Fiber / POSIX ucontext 切换（`runtime/aic_l3.{h,c}`），
**8ns/轮**（OS 线程 181µs，**2.3 万倍**）。协程 = 任务 = 独立区域根；初始栈 16KB
可动态增长。100 万轮 create/destroy：RSS 4025.0 → 4025.2 KB（+0.01%），峰值 4.5MB
—— **长跑零泄漏**。`smoke_coro.c` + `smoke_coro_churn.c` 进门禁，两 gcc 优化级
stdout 逐位一致。**T2（chan/Mutex/sleep 协程化，阻塞点让出）与 T3（M:N 工作池 +
工作窃取）未做** —— 当前阻塞语义是线程级（`aic_l2.c`），语义正确但无"多路复用"。

**容器层（B3 实测）**：map 的哈希函数、探测方式、负载因子（0.75）、墓碑语义与 C
**逐条相同**（FNV-1a 同常数、同为 find+slot 两趟线性探测）—— 所以差距不在这里。
差分定位后 map 层只占 B8 总差距的约 8%，大头在字符串层（600k 次 `str.fromI64`
= 栈格式化 + 堆分配 + memcpy）。已做：`keys[]/vals[]` 合成单槽 `slots[]`
（探测链只碰 slots+used 两条流，rehash 分配 5→4 次，微基准 1.13–1.16×）；
并把**运行期布局从编译器里解耦**——`printgen` 曾硬编码 `v->keys[...]`/`v->vals[...]`/
`v->slots[...]`，改为运行期的布局中立宏 `AIC_MAP_SLOT_KEY/VAL` + `AIC_SET_SLOT`
（槽字段名只在运行期定义一次）。B8 端到端仍 1.12×。

## 五、结论与优先级

1. **性能已经不是缺口**：8 项全部与 C 同档（0.43×–1.19×）。曾经的"B6-fib 3.2× /
   B9-dispatch 3.0× / B1-matmul 6.3×"三条记录**全部是测量口径错误**（§开篇两条纪律），
   正确修法是修 CLI 的 flag 透传 + 修 C 基线，**不是**在编译器里加内联/去虚拟化。
2. **仍真实落后的三项**：B1-matmul 1.19×、B1-sieve 1.15×、B8-mapset 1.12×。
   matmul/sieve 的可见短板在 list 迭代路径（`AIC_LIST_GET_CACHED` 已覆盖循环热点，
   剩余在边界检查与 `str.fromI64` 式格式化路径）；mapset 的短板在字符串层。
   Rust 侧 B8（2.62×）是标准库哈希容器的成熟度差距，属持续追赶项。
3. **内存已与 C 持平且归还更好**：三项峰值全 1.00×；B4 卸载后回落到基线的能力
   AIC 5.9× 优于 C；B5/B10 长跑零泄漏且比 C 更平。
4. **真正的缺口在语义不在数字**：T2/T3（阻塞原语协程化 + M:N 工作窃取）未做；
   E1–E4 文档脱节未修；`src/` 根目录残留 shell 脚本未迁 Go；ci.py 未接 GitHub CI
   （win/mac/linux/安卓/iOS 五平台）。

## 六、复现与扩展

- 新增 case：`benches/b<N>_<name>.aic` + `benches/c/` 同构 C 基线 +
  `scripts/bench_full.py` 的 `CASES`/`RUST_SRC` 两张表注册；
  判据 = 输出数值等价（浮点 1e-6 相对误差）。
- **基线必须防折叠**：C 基线的被测函数加 `__attribute__((noinline, noclone))`，
  且经 volatile 装载函数指针（否则 gcc -O2 会把循环消掉，测到的是启动时间）。
- 内存探针：`benches/b5_region.*` 的形态（N 轮 region 建/回收看尾半是否变平）；
  短跑进程不要用首半/尾半判增长（爬坡会伪装成增长），要看峰值与卸载后末值。
- 本机基线：gcc 16.2 / rustc 1.97.0 / clang 22.1；数据受本机噪声影响，
  **跨机对比只信同时同跑的 AIC/C 比值**。
