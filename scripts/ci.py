#!/usr/bin/env python3
"""AIC CI —— 门禁权威入口（scripts/ci.py）—— 规划方资产。

所有权：规划方资产（黄金执行规划 §四 D6）。AI 执行者只读，修改需走决议条目。
本文件是 scripts/ci.ps1 的 Python 等价物 + R3 接通；判定语义与条文一一对应：
H1 计时 / H2 确定性 / H3 四配置逐位 / H4 UBSan / H5 三段式 / H6 无隐式转换 /
H7 表面冻结 / #20 禁 cgo。§三 三层内测（build / vet / test）在最前面。

每项给出「真实跑 / 合法 SKIP / 失败」三判定之一（§八 验收机）。
SKIP 仅有理由 = 功能尚未就位的占位门禁，且白名单按阶段枚举（§四.3）：
R3 起无任何合法 SKIP，故本脚本默认拒绝一切 SKIP（--stage 仅供白名单阶段过渡）。

用法：
  python scripts/ci.py                 # 全量验收（H1–H7 + #20）
  python scripts/ci.py --fast          # 跳过 H3/H4（仍跑其余门禁；不得用于验收）
  python scripts/ci.py --stage R1      # 过渡期：允许 R1 白名单内的 SKIP
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from aici_exec import BIN, ROOT, backends, decode, ir_only_count, run, utf8_stdio  # noqa: E402

SCRIPTS = Path(__file__).resolve().parent
from aici_gates import GateResult, Verdict  # noqa: E402
from aici_gates_t2 import (  # noqa: E402
    gate_anchors,
    gate_h10,
    gate_h8,
    gate_h9,
)
from aici_gates_h import (  # noqa: E402
    gate_h1,
    gate_h2,
    gate_h3,
    gate_h4,
    gate_h5,
    gate_h6,
    gate_h7,
    gate_no_cgo,
)

# §四.3：按阶段的合法 SKIP 白名单。R3 起为空（曾真实启用的门禁不得回退 SKIP）。
STAGE_SKIPS: dict[str, set[str]] = {
    "R0": {"H1", "H2", "H3", "H4", "H5", "H6"},
    "R1": {"H1", "H2", "H3", "H4", "H6"},
    "R2": {"H1", "H2", "H3", "H4"},
    "R3": set(),
}

# go test 里允许的 SKIP（§三.3：除白名单外不允许新增 t.Skip）
GO_TEST_SKIP_ALLOW = {"TestUpdateErrGoldens"}

WORK = ROOT / "build" / ".ci"

COLORS = {
    Verdict.PASS: "\x1b[32m",
    Verdict.FAIL: "\x1b[31m",
    Verdict.SKIP: "\x1b[33m",
}
RESET = "\x1b[0m"


def paint(text: str, verdict: Verdict, color: bool) -> str:
    if not color:
        return text
    return f"{COLORS[verdict]}{text}{RESET}"


def log(msg: str) -> None:
    print(f"[ci] {msg}", flush=True)


class Recorder:
    def __init__(self) -> None:
        self.results = []
        self.notes: list[str] = []
        self.started = time.perf_counter()

    def record(self, res) -> None:
        self.results.append(res)
        self.notes.extend(res.notes)
        tag = paint(f"{res.verdict.value:<4}", res.verdict, sys.stdout.isatty())
        log(f"{tag} {res.gate:<4} {res.detail}")

    @property
    def failed(self) -> list:
        return [r for r in self.results if r.verdict is Verdict.FAIL]

    @property
    def skipped(self) -> list:
        return [r for r in self.results if r.verdict is Verdict.SKIP]


# ------------------------------------------------------------ §三 三层内测

def go_cmd(args: list[str]):
    return run(["go", *args], cwd=ROOT)


def go_test_suite() -> tuple[bool, str, int, int, list[str]]:
    """go test ./... 并统计 pass/fail/skip；SKIP 只允许白名单（§三.3）。"""
    r = go_cmd(["test", "./...", "-count=1", "-json"])
    text = decode(r.out)
    passed = failed = skipped = 0
    problems: list[str] = []
    names: dict[str, str] = {}
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            if line:
                problems.append(line)
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        action, test = ev.get("Action"), ev.get("Test")
        if action == "output" and not test:
            out = ev.get("Output", "").rstrip("\n")
            if out and not out.startswith(("ok ", "---", "=== RUN", "PASS", "FAIL")):
                problems.append(out)
        if not test:
            continue
        if action == "skip":
            skipped += 1
            names[test] = "skip"
        elif action == "pass":
            passed += 1
            names[test] = "pass"
        elif action == "fail":
            failed += 1
            names[test] = "fail"
    if r.code != 0 and failed == 0:
        failed = 1
    illegal = sorted(n for n, s in names.items() if s == "skip" and n not in GO_TEST_SKIP_ALLOW)
    for n in illegal:
        problems.append(f"非法 SKIP：{n}（§三.3 除白名单外不允许 t.Skip）")
    detail = f"PASS {passed} / FAIL {failed} / SKIP {skipped}（白名单外 {len(illegal)}）"
    return (r.code == 0 and not illegal), detail, passed, failed, problems


def acceptance_layers(rec: Recorder) -> bool:
    log("--- §三 三层内测：go build / go vet / go test ---")
    b = go_cmd(["build", "./..."])
    if b.code != 0:
        log(paint("FAIL", Verdict.FAIL, True) + " go build ./...\n" + (b.stdout + b.stderr)[-2000:])
        return False
    log("go build ./...    : PASS")
    v = go_cmd(["vet", "./..."])
    if v.code != 0:
        log(paint("FAIL", Verdict.FAIL, True) + " go vet ./...\n" + (v.stdout + v.stderr)[-2000:])
        return False
    log("go vet ./...      : PASS")
    good, detail, _p, _f, problems = go_test_suite()
    if not good:
        log(paint("FAIL", Verdict.FAIL, True) + f" go test ./... : {detail}")
        for p in problems[:40]:
            log("    " + p)
        return False
    log(f"go test ./...     : PASS ({detail})")
    # 运行时冒烟（含 L3 并发：有界通道阻塞交接 + 死锁判定，两调度模式逐位一致）。
    # 并发行为钉不到 AIC 语料上（无 TLS 的 tcc 后端走串行退化路径），故在运行时层验。
    sm = run([sys.executable, "-X", "utf8", str(SCRIPTS / "runtime_smoke.py")], cwd=ROOT)
    if sm.code != 0:
        log(paint("FAIL", Verdict.FAIL, True) + " runtime_smoke.py\n" + (sm.stdout + sm.stderr)[-2500:])
        return False
    tail = [ln for ln in sm.stdout.splitlines() if "PASS" in ln or "baseline" in ln]
    log("runtime_smoke.py  : PASS" + (f" ({tail[-1].strip()})" if tail else ""))
    return True


def build_cli() -> bool:
    """CI 自己构建 CLI：靠遗留 bin/aic.exe 会让 H1/H5 在干净检出上静默 SKIP。"""
    BIN.mkdir(parents=True, exist_ok=True)
    r = go_cmd(["build", "-o", str(BIN / "aic.exe"), "./cmd/aic"])
    if r.code != 0:
        log(paint("FAIL", Verdict.FAIL, True) + " 构建 aic CLI 失败\n" + (r.stdout + r.stderr)[-1500:])
        return False
    log(f"aic CLI 就绪: {BIN / 'aic.exe'}")
    return True


def main() -> int:
    utf8_stdio()
    ap = argparse.ArgumentParser(description="AIC 门禁权威入口（黄金执行规划 §四）")
    ap.add_argument(
        "--stage",
        default="R2",
        choices=sorted(STAGE_SKIPS),
        help="SKIP 白名单阶段；默认 R2=emit/CLI 交接窗口（H2/H3/H4 为合法占位 SKIP），"
        "emit 交付后一律 --stage R3（无任何合法 SKIP）",
    )
    ap.add_argument(
        "--strict",
        action="store_true",
        help="等同 --stage R3：任何 SKIP 都判失败（emit 交付后的验收姿势）",
    )
    ap.add_argument("--fast", action="store_true", help="跳过 H3/H4（仅内循环；验收禁用）")
    ap.add_argument("--selftest", action="store_true", help="跑门禁自测（合成输入钉住判负能力）后再跑全量")
    ns = ap.parse_args()
    stage = "R3" if ns.strict else ns.stage

    started = time.perf_counter()
    be_list, be_notes = backends()
    log(
        "AIC CI (Python) 启动, "
        + ", ".join(f"{b.name}={b.exe}" for b in be_list)
        + (f"; 注意: {'; '.join(be_notes)}" if be_notes else "")
    )

    rec = Recorder()
    rec.record(gate_no_cgo())

    if not acceptance_layers(rec):
        return 2
    if not build_cli():
        return 2

    if ns.selftest:
        import gate_selftest

        if gate_selftest.main(verbose=False) != 0:
            rec.record(GateResult("SELFTEST", Verdict.FAIL, "门禁自测未通过（门禁自身失效）"))
            return 3
        rec.record(
            GateResult("SELFTEST", Verdict.PASS, "门禁判负能力自测通过（合成输入：好 PASS / 坏 FAIL）")
        )

    shutil.rmtree(WORK, ignore_errors=True)
    WORK.mkdir(parents=True, exist_ok=True)

    log("--- 硬约束门禁（黄金执行规划 §四.1）---")
    rec.record(gate_h1())
    rec.record(gate_h2(WORK))
    if ns.fast:
        log("SKIP H3/H4：--fast 仅用于内循环，验收必须全量")
    else:
        h3 = gate_h3(WORK)
        rec.record(h3)
        rec.record(gate_h4(WORK))
    rec.record(gate_h5())
    rec.record(gate_h6())
    rec.record(gate_h7())
    # H8–H10 属 T2（§四.1）；实现落地后即真实执行，不再占位 SKIP。
    rec.record(gate_h8())
    rec.record(gate_h9())
    rec.record(gate_h10())
    # 锚点清单（T3：红线逐条转 ok + 显式锚点清单）；--strict 下缺口判负。
    rec.record(gate_anchors(strict=getattr(ns, "strict", False)))

    allowed = STAGE_SKIPS[stage]
    illegal = [r for r in rec.skipped if r.gate not in allowed]
    if ns.fast:
        illegal = [r for r in illegal if r.gate not in ("H3", "H4")]

    total = time.perf_counter() - started
    log(f"--- 汇总（{total:.1f}s, stage={stage}）---")
    for r in rec.results:
        log(f"    {r.gate:<8} {r.verdict.value:<4} {r.detail}")
    # IR-ONLY 进度条（T-A 的路径切换计分）：带 `// aic:ir-only` 标记的语料只在
    # IR 路径实现、暂不参与 C 编译/运行门禁（H1–H4/H9）。**L5 收口时必须为 0**
    # —— 那时 IR→C 发射器已覆盖全部语料，标记逐个删除。ok/ 与 trap/ 都要报，
    # 否则"少测了多少"会被静默吞掉。
    n_ok_only = ir_only_count("ok")
    n_trap_only = ir_only_count("trap")
    if n_ok_only or n_trap_only:
        log(
            f"    IR-ONLY  ok/ {n_ok_only} + trap/ {n_trap_only} 个语料带 `// aic:ir-only` 标记"
            "（只在 IR 路径实现，未参与 H1–H4/H9）；H8 仍覆盖它们；L5 收口时该计数必须为 0"
        )
    if rec.skipped and stage != "R3":
        log(
            f"注意：stage={stage} 是交接窗口，合法 SKIP 白名单 = {sorted(allowed)}；"
            "emit 交付后请用 --strict（= stage R3，无任何 SKIP）验收"
        )
    if rec.failed:
        return 1
    if illegal:
        for r in illegal:
            log(paint("FAIL", Verdict.FAIL, True) + f" {r.gate} SKIP 不在 stage={stage} 白名单内")
        return 1
    log(paint("CI 完成（绿灯）", Verdict.PASS, sys.stdout.isatty()))
    return 0


if __name__ == "__main__":
    if sys.version_info < (3, 10):
        sys.exit("scripts/ci.py 需要 Python 3.10+")
    sys.exit(main())
