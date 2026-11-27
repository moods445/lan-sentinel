# 10 · SNMP 接口指标标签 off-by-one、死代码与格式问题

- **严重度**：P3（展示错误 / 代码规范）
- **状态**：已修复（2026-11-27）
- **位置**
  - `internal/api/ui.html:563`（`metricLabel`）
  - `internal/config/config.go`（`envInt` 及 `var _ = envInt`）
  - `internal/rules/rules.go`（`Engine` 字段对齐）

## 成因 A：标签截断位置算错（off-by-one）

```js
if (k.startsWith("if_in_bps:"))  return "接口 " + k.slice(11) + " 接收";
if (k.startsWith("if_out_bps:")) return "接口 " + k.slice(12) + " 发送";
if (k.startsWith("if_oper_up:")) return "接口 " + k.slice(12) + " 状态";
```

前缀实际长度：`if_in_bps:` = 10、`if_out_bps:` = 11、`if_oper_up:` = 11，三者各多切了 1 个字符。

## 影响

SNMP 交换机的接口指标在面板上显示为「接口  接收」「接口  状态」，**索引号被吃掉**。有多个接口时无法区分是哪个口在告警（`if_oper_up:7` 与 `if_oper_up:8` 显示完全一样），排障时得回查 `decisions` 里的 `metric` 原文。

## 成因 B：死代码

```go
func envInt(k string, def int) int { ... }
var _ = envInt   // 仅为消除"未使用"而写的占位
```

`config` 包没有任何地方使用 `envInt`（`FromEnv` 只用 `env` 和 `os.Getenv`），`var _ = envInt` 是掩盖未使用函数的补丁，让 `deadcode`/linter 无法发现问题。

## 成因 C

`Engine` 结构体字段未对齐（`gofmt` 未跑）。

## 修复

```js
if (k.startsWith("if_in_bps:"))  return "接口 " + k.slice(10) + " 接收";
if (k.startsWith("if_out_bps:")) return "接口 " + k.slice(11) + " 发送";
if (k.startsWith("if_oper_up:")) return "接口 " + k.slice(11) + " 状态";
```

- 删除 `envInt` 与其占位变量（以及因此不再需要的 `strconv` import）；
- `gofmt -w internal/rules/rules.go`。

## 验证

- 内嵌前端 JS 语法校验：抽出 `<script>` 段后 `node --check` 通过；
- `gofmt -l .` 无输出，`go vet ./...` / `go build ./...` 通过；
- 人工回归：接一台 SNMP 交换机（`config/collectors.yaml` 的 `interfaces: [2, 3]`），面板节点详情应显示「接口 2 接收 / 接口 3 状态」，且多接口可区分。
