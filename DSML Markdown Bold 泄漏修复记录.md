# DSML Markdown Bold 泄漏修复记录

## 问题现象

通过 hermes-agent CLI 使用 ds2api 代理调用 `deepseek-v4-pro` 时，模型输出的工具调用偶尔会以**纯文本**形式泄漏到聊天界面，而不是被解析执行。

泄漏的文本形如：

```
<**DSML|tool_calls**><**DSML|invoke name="execute_code"**><**DSML|parameter name="code"**>print("hello")</**DSML|parameter**></**DSML|invoke**></**DSML|tool_calls**>
```

## 根因分析

### 1. 数据流

```
hermes-agent CLI
  → ds2api（Go 代理）
    → DeepSeek API
      → 模型输出 DSML 工具调用
    ← ds2api 解析 DSML → 转为 OpenAI tool_calls 格式
  ← hermes-agent 收到标准 tool_calls，正常执行
```

正常情况下，模型输出标准 DSML 标签 `<|DSML|tool_calls>`，ds2api 的 scanner 识别后转为 OpenAI 格式。

### 2. 根本原因

`deepseek-v4-pro` 有时会在 DSML 标签中加入 **Markdown 加粗标记 `**`**：

| 正常输出 | 模型异常输出 |
|---------|------------|
| `<\|DSML\|tool_calls>` | `<**DSML\|tool_calls**>` |
| `<\|DSML\|invoke name="xxx">` | `<**DSML\|invoke name="xxx"**>` |
| `</\|DSML\|parameter>` | `</**DSML\|parameter**>` |

ds2api 的 scanner（`toolcalls_scan.go` 中的 `consumeToolMarkupNamePrefixOnce()`）不认识 `<` 和 `D` 之间的 `*` 字符，导致整个工具调用块无法被识别，直接作为纯文本透传。

### 3. 为什么选择在 ds2api 修复

hermes-agent 是通用 AI 框架，不应耦合特定模型的输出怪癖。ds2api 作为 DeepSeek 专用代理，职责就是适配模型输出。修复应在最靠近问题源头的地方。

## 修复方案

双层修复：scanner 级别（流式 + 非流式）+ normalizer 级别（非流式保障）。

### 第一层：scanner 跳过 `*`（流式 + 非流式均生效）

hermes-agent 使用**流式模式**调用 ds2api。流式路径走 `scanToolMarkupTagAt` → `consumeToolMarkupNamePrefixOnce`，原来的 scanner 不认识 `*`，所以 `<**DSML|tool_calls**>` 整块被当纯文本放行。

修改 `toolcalls_scan.go` 中三处：

| 位置 | 改动 |
|------|------|
| `consumeToolMarkupNamePrefixOnce()` | 增加 `*` 跳过分支，让 `<**DSML|` 的前缀被识别 |
| `scanToolMarkupTagAt()` 标签名后 | 跳过 `**` 后再检查 boundary，避免 `<**DSML|tool_calls**>` 中 `tool_calls` 后面的 `**` 阻断 boundary 检测 |
| `rewriteDSMLToolMarkupOutsideIgnored()` | DSMLLike 标签输出时剥离 suffix 中 `>` 前的 `**` |

### 第二层：normalizer 预处理（非流式保障）

在 `normalizeDSMLToolCallMarkup()` 入口处添加预处理函数 `stripMarkdownBoldFromDSMLTags()`，在任何扫描逻辑执行之前先把 `**` 剥掉。

### 修改的文件

| 文件 | 改动 |
|------|------|
| `internal/toolcall/toolcalls_scan.go` | scanner 三处修改：`consumeToolMarkupNamePrefixOnce` 跳过 `*`、`scanToolMarkupTagAt` 标签名后跳过 `**`、`hasToolMarkupBoundary` 前跳过 `**` |
| `internal/toolcall/toolcalls_dsml.go` | 新增 `stripMarkdownBoldFromDSMLTags()` 预处理函数；rewrite 输出时剥离尾部 `**` |
| `internal/toolcall/toolcalls_test.go` | 新增 6 个测试函数：strip 单元测试、scanner 单元测试、端到端测试（含真实泄漏样本） |

### 处理逻辑

```
stripMarkdownBoldFromDSMLTags(text)
│
├─ 快速路径：文本不含 "*DSML|" → 直接返回，零开销
│
├─ Phase 1a（正则）：剥离开标签的 ** 前缀
│   正则：\*{1,2}(DSML\|)
│   <**DSML|tool_calls**>  →  <|DSML|tool_calls**>
│   <*DSML|invoke*>        →  <|DSML|invoke*>
│
├─ Phase 1b（字符串替换）：剥离闭标签的 *
│   <*/DSML|  →  </|DSML|
│   </*DSML|  →  </|DSML|
│   <**/DSML| →  </|DSML|
│   </**DSML| →  </|DSML|
│   （模型可能输出 <*/ 或 </* 两种顺序，都覆盖）
│
└─ Phase 2（逐行替换）：剥离尾部 ** 或 * 
    只处理含 "DSML|" 的行，避免影响普通 Markdown 加粗文本
    **>  →  >
    *>   →  >
```

#### Phase 3：展平嵌套裸 parameter 标签

DeepSeek-V4 在加粗标记场景下还会产生一种**嵌套裸 parameter** 变体：

```
<**DSML|parameter name="file_glob"**><**DSML|parameter>*DSML*Markdown*Bold*</**DSML|parameter**>
```

去掉 `**` 后变成：

```
<|DSML|parameter name="file_glob"><|DSML|parameter>*DSML*Markdown*Bold*</|DSML|parameter>
```

外层 `<|DSML|parameter name="file_glob">` 有 `name` 属性但没有值，紧跟一个无属性的裸 `<|DSML|parameter>` 才有值。ds2api parser 不支持这种嵌套格式，会返回 0 calls。

Phase 3 在 Phase 2 之后用正则删除所有无属性的裸 `<|DSML|parameter>` 标签：

```
正则：<(?:\|)?DSML\|parameter>
匹配：<|DSML|parameter>          （无属性，直接删除）
不匹配：<|DSML|parameter name="x">  （有属性，保留）
```

展平后变成标准单层格式：

```
<|DSML|parameter name="file_glob">*DSML*Markdown*Bold*</|DSML|parameter>
```

这个处理是安全的，因为标准 DSML 格式中 `<|DSML|parameter>` 必须带 `name` 属性，裸标签只会出现在模型异常输出中。

### 为什么这个方案安全

1. **快速路径无开销**：不含 `*DSML|` 的文本直接返回，不进入任何处理逻辑
2. **精确匹配**：只处理含 `DSML|` 的行/标签，普通 Markdown 加粗 `**text**` 不受影响
3. **最小改动**：不改 scanner，不改 parser，只在预处理阶段做文本清理
4. **不影响已有格式**：标准 `<|DSML|tool_calls>` 不含 `*`，不会被触碰

## 测试覆盖

### 单元测试：`TestStripMarkdownBoldFromDSMLTags`（6 个用例）

| 用例 | 输入 | 验证点 |
|------|------|--------|
| `no bold markers` | 标准标签 | 无 `*` 时不改动 |
| `double bold on opening tags` | `<**DSML\|tool_calls**>` 全套 | 双星号开闭标签完整剥离 |
| `single bold star` | `<*DSML\|tool_calls*>` 全套 | 单星号、闭标签 `<*/` 变体 |
| `mixed bold and clean tags` | 部分 `**`、部分标准 | 混合格式正确处理 |
| `bold with attributes` | `<**DSML\|parameter name="code"**>` | 带属性的标签 |
| `plain text bold untouched` | `**bold text**` + DSML 标签 | 普通加粗文本不被破坏 |

### 端到端测试（4 个用例）

| 测试函数 | 验证点 |
|---------|--------|
| `TestParseToolCallsToleratesMarkdownBoldDSMLTags` | 从 `<**DSML\|...**>` 完整解析出工具名和参数 |
| `TestParseToolCallsToleratesMarkdownBoldDSMLWithCDATA` | 带 CDATA 的 bold 标签完整解析 |
| `TestParseToolCallsToleratesMarkdownBoldDSMLRealWorldLeak` | 基于 hermes 真实泄漏样本：3 个 invoke、嵌套裸 `<**DSML|parameter>` 标签、参数值含 `*`，验证 Phase 3 展平后正确解析 |
| `TestScanToolMarkupTagAtMarkdownBold` | scanner 级别验证：双星号、单星号、带属性、闭标签、混合格式 |

## 调试过程中踩的坑

### 坑 1：正则匹配范围过大

第一次用 `</?\*{1,2}DSML\|` 匹配（含 `<`），替换时删 `*` 但丢失了 `|` 符号：

```
<**DSML|tool_calls>  →  <DSML|tool_calls>  （缺 |，后续 scanner 不认识）
```

修复：正则只匹配 `**DSML|` 部分，替换时输出 `|DSML|`（补上 `|`）。

### 坑 2：闭标签 `*` 和 `/` 的顺序

模型输出的闭标签有两种变体：

```
</*DSML|parameter*>   （* 在 / 前面）—  这不是合法 HTML/XML，但模型就是会输出
<*/DSML|parameter*>   （* 在 / 前面，另一种写法）
```

第一次只处理了 `</*DSML|`，漏掉了 `<*/DSML|`。补上后全部通过。

### 坑 3：正则和字符串替换的执行顺序

如果 Phase 1a（正则）先处理了 closing tag 中的 `*DSML|`，Phase 1b（字符串替换）就匹配不到了。解决方案：Phase 1b 用精确字符串匹配，确保两种变体都被覆盖。

## 提交历史

| Commit | 说明 |
|--------|------|
| `4cbe84a` | 初始实现：添加 `stripMarkdownBoldFromDSMLTags()` 和测试 |
| `2257bab` | 修复正则替换丢失 `\|` 符号的问题 |
| `e50bc81` | 闭标签改用简单字符串替换，避免正则互相干扰 |
| `6292d14` | 补上 `<*/DSML\|` 闭标签变体 |
| `9dfebfa` | 新增 Phase 3：展平嵌套裸 parameter 标签；测试用例改用真实 hermes 泄漏样本（3 个 invoke） |

## 部署位置

- Fork 仓库：`https://github.com/shiniantianlang/ds2api`
- 上游仓库：`https://github.com/CJackHwang/ds2api`
- 本地路径：`ds2api-main/internal/toolcall/toolcalls_dsml.go`
