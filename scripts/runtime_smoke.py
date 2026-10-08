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

    trap_cases = [
        (RUNTIME / "smoke" / "smoke_deeper.c", "reference outlives its region"),
        (RUNTIME / "smoke" / "smoke_escape.c", "reference to a popped region"),
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
