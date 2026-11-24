package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/bench"
)

func main() {
	seed := flag.Int64("seed", 42, "确定性场景随机种子")
	rounds := flag.Int("rounds", 200, "每个场景、每个变体的放置轮数")
	out := flag.String("out", "docs/benchmark.md", "Markdown 报告输出路径")
	jsonOut := flag.String("json", "", "可选 JSON 报告输出路径")
	chart := flag.Bool("chart", true, "在标准输出打印控制台条形图")
	chartOnly := flag.Bool("chart-only", false, "仅打印控制台图表，不写 Markdown/JSON 文件")
	flag.Parse()

	if err := run(*seed, *rounds, *out, *jsonOut, *chart, *chartOnly); err != nil {
		fmt.Fprintf(os.Stderr, "arbiter-bench: %v\n", err)
		os.Exit(1)
	}
}

func run(seed int64, rounds int, out, jsonOut string, chart, chartOnly bool) error {
	result, err := bench.Run(seed, rounds)
	if err != nil {
		return err
	}
	if chart || chartOnly {
		fmt.Println()
		fmt.Print(bench.RenderConsole(result))
	}
	if chartOnly {
		return nil
	}
	if err := writeFile(out, []byte(bench.RenderMarkdown(result, time.Now()))); err != nil {
		return fmt.Errorf("写入 Markdown 报告: %w", err)
	}
	fmt.Printf("已生成 %s\n", out)
	if jsonOut != "" {
		data, err := bench.RenderJSON(result)
		if err != nil {
			return err
		}
		if err := writeFile(jsonOut, data); err != nil {
			return fmt.Errorf("写入 JSON 报告: %w", err)
		}
		fmt.Printf("已生成 %s\n", jsonOut)
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
