#!/usr/bin/env python3
"""KubeArbiter 离线实验分析：把 E1–E7 的原始样本汇总成可复现的统计表。

输入为 JSONL，每行一个样本：

    {"experiment": "E1", "metric": "decision_latency_ms", "value": 12.3,
     "labels": {"policy": "BALANCED"}, "unit": "ms", "synthetic": false}

约束：
1. 只做描述统计与 bootstrap 均值置信区间，不替实验下结论。
2. `synthetic: true` 的样本默认拒绝，必须显式加 `--allow-synthetic`；
   避免把模拟或占位数据混进实验报告。
3. 仅依赖标准库，不引入 pandas/numpy，保证任何机器上可复现。

用法：
    python scripts/analysis/analyze.py --input samples.jsonl
    python scripts/analysis/analyze.py --input samples.jsonl --csv summary.csv --bootstrap 5000
"""

from __future__ import annotations

import argparse
import csv
import json
import math
import random
import sys
from dataclasses import dataclass
from pathlib import Path


class AnalysisError(Exception):
    """输入不合法；由 CLI 转成退出码 2。"""


@dataclass(frozen=True)
class Sample:
    experiment: str
    metric: str
    unit: str
    labels: str
    value: float


def percentile(sorted_values: list[float], q: float) -> float:
    """线性插值分位数（与 numpy 默认的 linear 方法一致）。"""
    if not sorted_values:
        raise ValueError("空序列没有分位数")
    if len(sorted_values) == 1:
        return sorted_values[0]
    pos = (len(sorted_values) - 1) * q
    lo = math.floor(pos)
    hi = min(lo + 1, len(sorted_values) - 1)
    frac = pos - lo
    return sorted_values[lo] + (sorted_values[hi] - sorted_values[lo]) * frac


def bootstrap_ci(values: list[float], iterations: int, seed: int) -> tuple[float, float]:
    """均值的 95% bootstrap 百分位置信区间；固定种子保证结果可复现。"""
    if iterations < 1:
        raise ValueError("bootstrap 次数必须 >= 1")
    if len(values) == 1:
        return values[0], values[0]
    rnd = random.Random(seed)
    n = len(values)
    means = sorted(sum(values[rnd.randrange(n)] for _ in range(n)) / n for _ in range(iterations))
    return percentile(means, 0.025), percentile(means, 0.975)


def summarize(values: list[float], iterations: int, seed: int) -> dict[str, float]:
    ordered = sorted(values)
    mean = sum(ordered) / len(ordered)
    low, high = bootstrap_ci(ordered, iterations, seed)
    return {
        "n": len(ordered),
        "mean": mean,
        "p50": percentile(ordered, 0.50),
        "p95": percentile(ordered, 0.95),
        "p99": percentile(ordered, 0.99),
        "min": ordered[0],
        "max": ordered[-1],
        "ci95_low": low,
        "ci95_high": high,
    }


def _require_text(rec: dict, key: str, lineno: int) -> str:
    value = rec.get(key)
    if not isinstance(value, str) or not value.strip():
        raise AnalysisError(f"第 {lineno} 行缺少非空字符串字段 {key!r}")
    return value.strip()


def load_samples(path: Path, allow_synthetic: bool) -> list[Sample]:
    samples: list[Sample] = []
    with path.open(encoding="utf-8-sig") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError as exc:
                raise AnalysisError(f"第 {lineno} 行不是合法 JSON: {exc}") from exc
            if not isinstance(rec, dict):
                raise AnalysisError(f"第 {lineno} 行必须是 JSON 对象")

            value = rec.get("value")
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise AnalysisError(f"第 {lineno} 行 value 必须是数字")
            value = float(value)
            if not math.isfinite(value):
                raise AnalysisError(f"第 {lineno} 行 value 必须是有限数")

            if rec.get("synthetic") is True and not allow_synthetic:
                raise AnalysisError(
                    f"第 {lineno} 行标记为 synthetic，拒绝进入统计；"
                    "确需分析模拟数据请显式加 --allow-synthetic"
                )

            labels = rec.get("labels") or {}
            if not isinstance(labels, dict):
                raise AnalysisError(f"第 {lineno} 行 labels 必须是对象")
            labels_key = json.dumps(
                {str(k): str(v) for k, v in sorted(labels.items())},
                ensure_ascii=False,
                sort_keys=True,
            )

            samples.append(
                Sample(
                    experiment=_require_text(rec, "experiment", lineno),
                    metric=_require_text(rec, "metric", lineno),
                    unit=str(rec.get("unit") or ""),
                    labels=labels_key,
                    value=value,
                )
            )

    if not samples:
        raise AnalysisError("输入没有任何样本")
    return samples


def group_samples(samples: list[Sample]) -> dict[tuple[str, str, str, str], list[float]]:
    groups: dict[tuple[str, str, str, str], list[float]] = {}
    for s in samples:
        groups.setdefault((s.experiment, s.metric, s.unit, s.labels), []).append(s.value)
    return groups


def build_rows(groups: dict[tuple[str, str, str, str], list[float]],
               iterations: int, seed: int) -> list[dict]:
    rows = []
    for (experiment, metric, unit, labels), values in sorted(groups.items()):
        row = {
            "experiment": experiment,
            "metric": metric,
            "unit": unit,
            "labels": labels,
        }
        row.update(summarize(values, iterations, seed))
        rows.append(row)
    return rows


COLUMNS = [
    "experiment", "metric", "unit", "labels",
    "n", "mean", "p50", "p95", "p99", "min", "max", "ci95_low", "ci95_high",
]


def format_markdown(rows: list[dict]) -> str:
    header = "| " + " | ".join(COLUMNS) + " |"
    divider = "|" + "|".join("---" for _ in COLUMNS) + "|"
    lines = [header, divider]
    for row in rows:
        cells = []
        for col in COLUMNS:
            val = row[col]
            cells.append(f"{val:.4f}" if isinstance(val, float) else str(val))
        lines.append("| " + " | ".join(cells) + " |")
    return "\n".join(lines)


def write_csv(rows: list[dict], path: Path) -> None:
    with path.open("w", encoding="utf-8", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=COLUMNS)
        writer.writeheader()
        writer.writerows(rows)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="KubeArbiter E1–E7 离线统计")
    parser.add_argument("--input", required=True, type=Path, help="JSONL 样本文件")
    parser.add_argument("--csv", type=Path, help="可选：同时导出 CSV")
    parser.add_argument("--bootstrap", type=int, default=2000, help="bootstrap 次数（默认 2000）")
    parser.add_argument("--seed", type=int, default=42, help="bootstrap 随机种子（默认 42）")
    parser.add_argument("--allow-synthetic", action="store_true",
                        help="允许 synthetic=true 的样本（默认拒绝）")
    args = parser.parse_args(argv)

    try:
        samples = load_samples(args.input, args.allow_synthetic)
        rows = build_rows(group_samples(samples), args.bootstrap, args.seed)
    except AnalysisError as exc:
        print(f"输入错误: {exc}", file=sys.stderr)
        return 2
    except OSError as exc:
        print(f"无法读取 {args.input}: {exc}", file=sys.stderr)
        return 2

    print(format_markdown(rows))
    print()
    print(f"样本数 {len(samples)}，分组 {len(rows)}；"
          f"bootstrap {args.bootstrap} 次，seed={args.seed}。")
    if args.csv:
        write_csv(rows, args.csv)
        print(f"CSV 已写入 {args.csv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
