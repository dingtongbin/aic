#!/usr/bin/env python3
"""AIC 记忆化签名/形状工具（scripts/aici_shape.py）—— 规划方资产。

只放「纯函数」判据：三段式形状、emit 文本归一。门禁流程在 aici_gates.py；
把纯判据单独放一处，是为了让 gate_selftest.py 能直接钉住它们（§二 W2）。
"""

from __future__ import annotations

import re

# H5 三段式（核心设计 §三）：位置+描述 / 缩进上下文 / fix:
# 标记词是英语（编译器面向用户的输出一律英语），段数与缩进判据不变。
_HEAD = re.compile(r":\d+:\d+: error: ")
_CONTEXT = re.compile(r"^ {4}\S")
_FIX = re.compile(r"^ {4}fix: \S")

# emit 文本里与语义无关的环境差异（临时目录名等）；只吃「看起来像绝对路径」的串
_NORM_PATTERNS = (
    (re.compile(r"[A-Za-z]:[\\/][^\s\"'<>]*"), "<path>"),
    (re.compile(r"/(?:tmp|var|home)/[^\s\"'<>]*"), "<path>"),
)


def three_part_shape(text: str) -> str | None:
    """三段式形状校验；返回不合法原因，合法返回 None（H5 缺一不可）。"""
    lines = [ln for ln in text.replace("\r\n", "\n").split("\n") if ln.strip()]
    if len(lines) < 3:
        return f"有效行 {len(lines)} < 3（每个错误须三段）"
    if len(lines) % 3 != 0:
        return f"有效行数 {len(lines)} 不是 3 的倍数"
    for i in range(0, len(lines), 3):
        head, ctx, fix = lines[i], lines[i + 1], lines[i + 2]
        if not _HEAD.search(head):
            return f"第 {i + 1} 行缺「位置+描述」：{head[:120]}"
        if not _CONTEXT.match(ctx):
            return f"第 {i + 2} 行缺缩进上下文：{ctx[:120]}"
        if not _FIX.match(fix):
            return f"第 {i + 3} 行缺「修复：」：{fix[:120]}"
    return None


def normalize_emit(text: str) -> str:
    """归一化 emit 文本里与语义无关的环境差异（临时目录名等）。

    只吃绝对路径；#line 的行号、标识符、代码本身一律保留，所以真正的非确定性
    （实例顺序、mangling、布局）依然会被逐字节比对抓住。
    """
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    for pat, repl in _NORM_PATTERNS:
        text = pat.sub(repl, text)
    return text
