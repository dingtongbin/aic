#!/usr/bin/env python3
"""AIC runtime 冒烟测试（R3 过渡；emit 交付后由 golden H2/H3/H4 接管）。

编译 C 运行时并运行同一程序，四配置（gcc-O0 / gcc-O2 / clang-O2 / clang+UBSan）
逐字节比对 stdout（H3 形），再跑两个必 trap 程序校验 stderr 前缀与非零退出码。

用法：python -X utf8 scripts/runtime_smoke.py
"""
import os
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RUNTIME = ROOT / "runtime"
BIN = ROOT / "bin"
BIN.mkdir(exist_ok=True)


def find_tool(name: str) -> str | None:
    found = shutil.which(name)
    if found:
        return found
    for base in (r"C:\msys64\clang64\bin", r"C:\msys64\ucrt64\bin", r"C:\msys64\mingw64\bin"):
        cand = Path(base) / (name + ".exe")
        if cand.exists():
            return str(cand)
    return None


GCC = find_tool("gcc")
CLANG = find_tool("clang")
if not GCC:
    print("[smoke] FAIL: gcc not found")
    sys.exit(1)

COMMON = ["-std=c11", "-Wall", "-Wextra", "-I", str(RUNTIME)]


def compile(cc: str, flags: list[str], sources: list[Path], out: Path) -> None:
    cmd = [cc, *flags, *COMMON, *(str(s) for s in sources), "-o", str(out), "-lm"]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        print("[smoke] FAIL compile:", " ".join(cmd))
        print(r.stdout)
        print(r.stderr)
        sys.exit(1)


def run(exe: Path) -> tuple[int, bytes, bytes]:
    r = subprocess.run([str(exe)], capture_output=True)
    return r.returncode, r.stdout, r.stderr


def tsan_available(cc: str) -> bool:
    """探测工具链是否带 TSan 运行时（minGW 的 clang/gcc 都没有 ⇒ 显式 SKIP）。"""
    probe = BIN / "tsan_probe.c"
    probe.write_text("int main(void){return 0;}\n", encoding="utf-8")
    out = BIN / "tsan_probe.exe"
    r = subprocess.run([cc, "-fsanitize=thread", str(probe), "-o", str(out)],
                       capture_output=True, text=True)
    return r.returncode == 0 and out.exists()


def main() -> int:
    core = [RUNTIME / "aic_l0.c", RUNTIME / "aic_std.c"]
    ok_src = [RUNTIME / "smoke" / "smoke_ok.c", *core]

    configs: list[tuple[str, str, list[str]]] = [
        ("gcc-O0", GCC, ["-O0"]),
        ("gcc-O2", GCC, ["-O2"]),
    ]
    if CLANG:
        configs.append(("clang-O2", CLANG, ["-O2"]))
        configs.append(("clang-UBSan", CLANG, ["-O2", "-fsanitize=undefined", "-fno-sanitize-recover=all"]))

    baseline: bytes | None = None
    for name, cc, flags in configs:
        exe = BIN / f"smoke_ok_{name}.exe"
        compile(cc, flags, ok_src, exe)
        code, out, err = run(exe)
        if code != 0:
            print(f"[smoke] FAIL: {name} exited {code}")
            print(err.decode("utf-8", "replace"))
            return 1
        if name == "clang-UBSan" and err.strip():
            print("[smoke] FAIL: UBSan report:")
            print(err.decode("utf-8", "replace"))
            return 1
        if baseline is None:
            baseline = out
            print(f"[smoke] baseline: {name} ({baseline.count(bytes([10]))} lines)")
        elif out != baseline:
            print(f"[smoke] FAIL: {name} stdout differs from baseline")
            return 1
        print(f"[smoke] OK: {name}")

    # --- R19：分代区域的并发锚点（无锁空闲链 + 每槽状态字）--------------------
    # 4 个真线程同时「task_init → region push/pop + 区域分配」各 2 万轮。
    # 判据 = ① 各配置 stdout 逐位确定（没有槽被两线程同时拿到：那会让两个区域
    # 共享一条页链，表现为数字错乱/崩溃）；② TSan 零报告（本机工具链不带 TSan
    # 运行时 ⇒ 探测可用性，不可用则显式 SKIP，不静默放过）。
    thr_src = [RUNTIME / "smoke" / "smoke_threads.c", *core]
    thr_base: bytes | None = None
    thr_configs = [("gcc-O0", GCC, ["-O0"]), ("gcc-O2", GCC, ["-O2"])]
    if CLANG:
        thr_configs.append(("clang-O2", CLANG, ["-O2"]))
    for name, cc, flags in thr_configs:
        exe = BIN / f"smoke_threads_{name}.exe"
        compile(cc, flags, thr_src, exe)
        code, out, err = run(exe)
        if code != 0:
            print(f"[smoke] FAIL: threads {name} exited {code}")
            print(err.decode("utf-8", "replace"))
            return 1
        if thr_base is None:
            thr_base = out
        elif out != thr_base:
            print(f"[smoke] FAIL: threads {name} stdout differs ({out!r} vs {thr_base!r})")
            return 1
        print(f"[smoke] OK threads: {name} ({out.decode().strip()})")

    if CLANG and tsan_available(CLANG):
        exe = BIN / "smoke_threads_clang-TSan.exe"
        compile(CLANG, ["-O1", "-fsanitize=thread", "-fno-sanitize-recover=all"], thr_src, exe)
        code, out, err = run(exe)
        rep = err.decode("utf-8", "replace")
        if code != 0 or "ThreadSanitizer" in rep:
            print("[smoke] FAIL: threads clang-TSan:")
            print(rep[:2000])
            return 1
        if out != thr_base:
            print(f"[smoke] FAIL: threads clang-TSan stdout differs ({out!r} vs {thr_base!r})")
            return 1
        print(f"[smoke] OK threads: clang-TSan ({out.decode().strip()})")
    else:
        print("[smoke] SKIP threads TSan: no TSan runtime in this toolchain "
              "(minGW clang/gcc); determinism checks above still ran")

    trap_cases = [
        (RUNTIME / "smoke" / "smoke_deeper.c", "reference outlives its region"),
        (RUNTIME / "smoke" / "smoke_escape.c", "reference to a popped region"),
        # R19：槽位回收后，持有旧一代（同槽不同 gen）引用的程序仍须 trap
        # （新形态用 gen 比对取代旧的"id 单调不复用"）。同文件也验证新对象
        # 不被误拦（先过一次活守卫才会走到旧对象那条）。
        (RUNTIME / "smoke" / "smoke_recycle.c", "reference to a popped region"),
    ]
    for src, prefix in trap_cases:
        exe = BIN / f"smoke_{src.stem}.exe"
        compile(GCC, ["-O0"], [src, *core], exe)
        code, _out, err = run(exe)
        if code == 0:
            print(f"[smoke] FAIL: {src.name} did not trap")
            return 1
        if prefix not in err.decode("utf-8", "replace"):
            print(f"[smoke] FAIL: {src.name} prefix missing")
            print(err.decode("utf-8", "replace"))
            return 1
        print(f"[smoke] OK trap: {src.name} (exit {code})")

    # --- L3：真并发（有界通道的阻塞交接 + 死锁判定）-------------------------
    # 并发行为无法只靠 AIC 语料锚点钉住（无 TLS 的 tcc 后端跑的是串行退化路径），
    # 故在运行时层直接验：**两种调度模式 stdout 逐位一致** + 真死锁必须 trap。
    l2_src = [RUNTIME / "smoke" / "smoke_l3_pipeline.c", *core, RUNTIME / "aic_l2.c"]
    l3_base: bytes | None = None
    for mode, env_extra in (("parallel", {}), ("serial", {"AIC_SCHED_SERIAL": "1"})):
        for name, cc, flags in configs[:2]:  # gcc-O0 / gcc-O2 两种优化级别
            exe = BIN / f"smoke_l3_pipeline_{mode}_{name}.exe"
            compile(cc, flags, l2_src, exe)
            env = dict(os.environ)
            env.update(env_extra)
            r = subprocess.run([str(exe)], capture_output=True, env=env, timeout=120)
            if r.returncode != 0:
                print(f"[smoke] FAIL: L3 pipeline {mode}/{name} exited {r.returncode}")
                print(r.stderr.decode("utf-8", "replace"))
                return 1
            if mode == "parallel" and name == "gcc-O0":
                l3_base = r.stdout
                print(f"[smoke] L3 baseline ({mode}/{name}): {l3_base!r}")
            elif r.stdout != l3_base:
                print(f"[smoke] FAIL: L3 pipeline {mode}/{name} stdout differs")
                print(f"  got {r.stdout!r}\n  want {l3_base!r}")
                return 1
            print(f"[smoke] OK: L3 pipeline {mode}/{name}")
    # --- L3 coroutine（T1：协程载体 + 区域根 + C10K）------------------------
    # 协程层是新增的：门禁必须证明它在两种 gcc 优化级下输出一致（区域根独立、
    # 令牌串行 ⇒ 与单线程语义同构），且 tcc 上整体空转不参与链接。
    coro_src = [RUNTIME / "smoke" / "smoke_coro.c", *core,
                RUNTIME / "aic_l2.c", RUNTIME / "aic_l3.c"]
    coro_base: bytes | None = None
    for name, cc, flags in configs[:2]:
        exe = BIN / f"smoke_coro_{name}.exe"
        compile(cc, flags, coro_src, exe)
        r = subprocess.run([str(exe)], capture_output=True, timeout=300)
        if r.returncode != 0:
            print(f"[smoke] FAIL: coro {name} exited {r.returncode}")
            print(r.stderr.decode("utf-8", "replace"))
            return 1
        if coro_base is None:
            coro_base = r.stdout
            print(f"[smoke] coro baseline ({name}): {coro_base!r}")
        elif r.stdout != coro_base:
            print(f"[smoke] FAIL: coro {name} stdout differs")
            print(f"  got {r.stdout!r}\n  want {coro_base!r}")
            return 1
        print(f"[smoke] OK coro: {name}")

    # 协程 churn：N 轮「create → 区域分配 → destroy」必须**稳态**（无泄漏）。
    # RSS 稳态判据（尾段后半 < 1% 且峰值 < 32MB；Windows 堆/线程栈的一次性
    # 水位与 N 无关 —— 实测 400 万轮与 1000 万轮尾段一致）见 build/coro_residency.py。
    churn_src = [RUNTIME / "smoke" / "smoke_coro_churn.c", *core,
                 RUNTIME / "aic_l2.c", RUNTIME / "aic_l3.c"]
    exe = BIN / "smoke_coro_churn.exe"
    compile(GCC, ["-O2"], churn_src, exe)
    r = subprocess.run([str(exe)], capture_output=True, timeout=600,
                       env=dict(os.environ, N="1000000"))
    if r.returncode != 0:
        print(f"[smoke] FAIL: coro churn exited {r.returncode}")
        print(r.stderr.decode("utf-8", "replace"))
        return 1
    if b"coro-churn 1000000 ok" not in r.stdout:
        print(f"[smoke] FAIL: coro churn stdout {r.stdout!r}")
        return 1
    print("[smoke] OK coro: churn 1M rounds")

    # 真死锁：必须 trap（不是挂死）——超时即判负。
    dl_src = [RUNTIME / "smoke" / "smoke_l3_deadlock.c", *core, RUNTIME / "aic_l2.c"]
    for mode, env_extra in (("parallel", {}), ("serial", {"AIC_SCHED_SERIAL": "1"})):
        exe = BIN / f"smoke_l3_deadlock_{mode}.exe"
        compile(GCC, ["-O0"], dl_src, exe)
        env = dict(os.environ)
        env.update(env_extra)
        try:
            r = subprocess.run([str(exe)], capture_output=True, env=env, timeout=120)
        except subprocess.TimeoutExpired:
            print(f"[smoke] FAIL: L3 deadlock {mode} hung (no trap)")
            return 1
        err = r.stderr.decode("utf-8", "replace")
        if r.returncode == 0 or "deadlock: every task is waiting" not in err:
            print(f"[smoke] FAIL: L3 deadlock {mode} did not trap as expected")
            print(err)
            return 1
        print(f"[smoke] OK trap: L3 deadlock {mode} (exit {r.returncode})")

    # select 的真等待（N6 的 aic_select_wait ABI）：时间臂与 recv 臂各验一次。
    sel_src = [RUNTIME / "smoke" / "smoke_l3_select.c", *core, RUNTIME / "aic_l2.c"]
    sel_base: bytes | None = None
    for mode, env_extra in (("parallel", {}), ("serial", {"AIC_SCHED_SERIAL": "1"})):
        exe = BIN / f"smoke_l3_select_{mode}.exe"
        compile(GCC, ["-O0"], sel_src, exe)
        env = dict(os.environ)
        env.update(env_extra)
        try:
            r = subprocess.run([str(exe)], capture_output=True, env=env, timeout=120)
        except subprocess.TimeoutExpired:
            print(f"[smoke] FAIL: L3 select {mode} hung")
            return 1
        if r.returncode != 0:
            print(f"[smoke] FAIL: L3 select {mode} exited {r.returncode}")
            print(r.stdout.decode("utf-8", "replace"))
            print(r.stderr.decode("utf-8", "replace"))
            return 1
        if sel_base is None:
            sel_base = r.stdout
        elif r.stdout != sel_base:
            print(f"[smoke] FAIL: L3 select {mode} stdout differs: {r.stdout!r} vs {sel_base!r}")
            return 1
        print(f"[smoke] OK: L3 select {mode} ({r.stdout!r})")

    # Err 的 cause 链（§16 N5 ④）：运行期 aic_err_wrap 把 cause 拷进当前区域并挂链，
    # aic_err_dump 逐级打印。冻结**逐字节**文本：链的形状是语言级契约（规则 5）。
    chain_src = [RUNTIME / "smoke" / "smoke_err_chain.c", *core, RUNTIME / "aic_l2.c"]
    chain_exe = BIN / "smoke_err_chain.exe"
    compile(GCC, ["-O0"], chain_src, chain_exe)
    try:
        r = subprocess.run([str(chain_exe)], capture_output=True, timeout=60)
    except subprocess.TimeoutExpired:
        print("[smoke] FAIL: Err cause chain hung")
        return 1
    if r.returncode != 0:
        print(f"[smoke] FAIL: Err cause chain exited {r.returncode}")
        print(r.stderr.decode("utf-8", "replace"))
        return 1
    want_chain = (
        b"startup failed\n"
        b"  caused by: cannot load config\n"
        b"  caused by: file not found\n"
    )
    if r.stderr != want_chain:
        print("[smoke] FAIL: Err cause chain stderr differs")
        print(f"  want: {want_chain!r}")
        print(f"  got:  {r.stderr!r}")
        return 1
    # 读无 cause 的错误的 cause = null dereference trap（§九：疑 nil = 运行时 trap）。
    nil_exe = BIN / "smoke_err_nil_cause.exe"
    compile(GCC, ["-O0"], [RUNTIME / "smoke" / "smoke_err_nil_cause.c", *core], nil_exe)
    try:
        r2 = subprocess.run([str(nil_exe)], capture_output=True, timeout=60)
    except subprocess.TimeoutExpired:
        print("[smoke] FAIL: nil cause read hung (no trap)")
        return 1
    if r2.returncode == 0 or b"null dereference" not in r2.stderr:
        print(f"[smoke] FAIL: nil cause read did not trap (exit {r2.returncode})")
        print(r2.stderr.decode("utf-8", "replace")[:400])
        return 1
    print(f"[smoke] OK: Err cause chain ({r.stderr!r})")

    print("[smoke] PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
