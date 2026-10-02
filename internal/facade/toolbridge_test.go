package facade

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// TestExtractExecBlock 覆盖围栏提取的三种形态。
func TestExtractExecBlock(t *testing.T) {
	// 1) 正常块
	js, ok := extractExecBlock("前言\n```codex-exec\nawait tools.exec_command({cmd:\"ls\"})\n```\n后记")
	if !ok || !strings.Contains(js, "tools.exec_command") {
		t.Fatalf("正常块提取失败: ok=%v js=%q", ok, js)
	}
	if strings.Contains(js, "前言") {
		t.Errorf("不应包含块外内容: %q", js)
	}

	// 2) 没有块
	if _, ok := extractExecBlock("纯文本回复，没有任何围栏"); ok {
		t.Fatal("纯文本不应提取出块")
	}

	// 3) 未闭合（流式截断场景）：取剩余内容
	js, ok = extractExecBlock("```codex-exec\ntext('hi')")
	if !ok || !strings.Contains(js, "text('hi')") {
		t.Fatalf("未闭合块应尽力提取: ok=%v js=%q", ok, js)
	}

	// 4) 不认别的围栏名（普通代码块不是工具调用）
	if _, ok := extractExecBlock("```js\nawait tools.exec_command()\n```"); ok {
		t.Fatal("普通 js 围栏不应被当成工具调用")
	}
}

// TestEnsureExecJS 覆盖"模型输出裸 shell 而非 JS"的自动包装。
//
// 实测背景：模型经常无视"输出 JS"的要求，直接把 bash 命令写进围栏，
// 进了 V8 就是 SyntaxError，然后陷入死循环。代理层兜底是必须的。
func TestEnsureExecJS(t *testing.T) {
	// 1) 裸命令 → 包装成 exec_command
	wrapped := ensureExecJS("printf '%s' 'X' > f.txt")
	if !strings.Contains(wrapped, "tools.exec_command") {
		t.Errorf("裸命令应被包装: %q", wrapped)
	}
	if !strings.Contains(wrapped, `printf '%s' 'X' > f.txt`) {
		t.Errorf("命令内容应原样保留: %q", wrapped)
	}

	// 2) 多行命令也整体保留
	multi := "set -eu\nprintf 'a' > f\ncat f"
	wrapped = ensureExecJS(multi)
	if !strings.Contains(wrapped, "cat f") {
		t.Errorf("多行命令应整体保留: %q", wrapped)
	}

	// 3) 已经是 JS → 原样透传
	js := "const r = await tools.exec_command({cmd: 'ls'});\ntext(r);"
	if got := ensureExecJS(js); got != js {
		t.Errorf("JS 应原样透传: %q", got)
	}
}

// TestBridgeEnabled：Codex CLI 有两条工具声明路径（见 BridgeEnabled 注释）。
// 早期只认路径 A，导致走路径 B 的客户端桥静默失效（模型退回上游沙箱执行，
// 本地拿不到文件）。这里把两条路径与"不该误伤"的场景都钉住。
func TestBridgeEnabled(t *testing.T) {
	// 路径 A：additional_tools 条目
	withTools := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"additional_tools","tools":[]}]`),
	}
	if !BridgeEnabled(withTools) {
		t.Fatal("含 additional_tools 应启用桥")
	}

	// 路径 A 续：已有 custom_tool_call 往返（Codex 独有形状）
	withCustom := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"custom_tool_call","name":"exec","input":"..."}]`),
	}
	if !BridgeEnabled(withCustom) {
		t.Fatal("含 custom_tool_call 应启用桥")
	}

	// 路径 B：标准 tools 字段 + Codex 独有工具特征
	withStandardTools := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"exec","description":"run a command"}]`),
	}
	if !BridgeEnabled(withStandardTools) {
		t.Fatal("顶层 tools 含 exec 应启用桥（路径 B）")
	}
	// 关键回归：新版 CLI 的工具名是 exec_command（不是 exec）——
	// 早期全名匹配 `"name":"exec"` 会漏判，这里钉住。
	withExecCommand := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"exec_command","description":"Runs a command"},{"type":"custom","name":"write_stdin"}]`),
	}
	if !BridgeEnabled(withExecCommand) {
		t.Fatal("顶层 tools 含 exec_command 应启用桥（路径 B，新版命名）")
	}

	withApplyPatch := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"apply_patch"}]`),
	}
	if !BridgeEnabled(withApplyPatch) {
		t.Fatal("顶层 tools 含 apply_patch 应启用桥（路径 B）")
	}

	// 不该误伤：普通 API 调用方的 function 工具
	plainFunctions := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"function","name":"get_weather","parameters":{}}]`),
	}
	if BridgeEnabled(plainFunctions) {
		t.Fatal("普通 function 工具不应启用桥（否则破坏正常 function calling）")
	}
	// 不该误伤：空 tools / null / 纯文本
	for name, raw := range map[string]map[string]json.RawMessage{
		"空 tools":   {"input": json.RawMessage(`[{"type":"message"}]`), "tools": json.RawMessage(`[]`)},
		"null":      {"input": json.RawMessage(`[{"type":"message"}]`), "tools": json.RawMessage(`null`)},
		"纯文本 input": {"input": json.RawMessage(`"你好"`)},
		"无 input":   {},
	} {
		if BridgeEnabled(raw) {
			t.Fatalf("%s 不应启用桥", name)
		}
	}
}

// TestExecToolName：工具名必须从请求里动态提取。
//
// 背景：CLI v0.154 工具名是 exec，v0.159 改成 exec_command。
// 名字用错时客户端直接拒绝（"unsupported custom tool call: exec"），
// 模型看不到执行结果，反复要求用户重发内容 —— 表现得像上下文丢失。
func TestExecToolName(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]json.RawMessage
		want string
	}{
		{
			"新版 CLI（exec_command）",
			map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"custom","name":"exec_command"},{"name":"write_stdin"}]`)},
			"exec_command",
		},
		{
			"旧版 CLI（exec）",
			map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"custom","name":"exec"}]`)},
			"exec",
		},
		{
			"路径 A（additional_tools 内含名字）",
			map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"additional_tools","tools":[{"name":"exec"}]}]`)},
			"exec",
		},
		{
			"都不认识时兜底 exec",
			map[string]json.RawMessage{"input": json.RawMessage(`"hi"`)},
			"exec",
		},
	}
	for _, c := range cases {
		if got := ExecToolName(c.raw); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestExecToolKind：区分 custom（v0.154）与 function（v0.159）两种工具形状。
// 回错形状时客户端静默不执行，模型陷入"执行被中止"的循环。
func TestExecToolKind(t *testing.T) {
	funcTool := map[string]json.RawMessage{
		"tools": json.RawMessage(`[{"type":"function","name":"exec_command","parameters":{}}]`),
	}
	if got := ExecToolKind(funcTool); got != "function" {
		t.Errorf("function 工具判定错误: got %q", got)
	}
	customTool := map[string]json.RawMessage{
		"tools": json.RawMessage(`[{"type":"custom","name":"exec","format":{}}]`),
	}
	if got := ExecToolKind(customTool); got != "custom" {
		t.Errorf("custom 工具判定错误: got %q", got)
	}
}

// TestToFunctionArguments：模型输出的 JS 源码要能转成 function 工具要的 JSON。
func TestToFunctionArguments(t *testing.T) {
	// JS 形状 -> JSON
	args := toFunctionArguments(`const out = await tools.exec_command({ cmd: "Set-Content -Path a.txt -Value 'hi'" });`)
	var m map[string]string
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("JS 转换结果不是合法 JSON: %s", args)
	}
	if m["cmd"] != "Set-Content -Path a.txt -Value 'hi'" {
		t.Errorf("cmd 提取错误: %q", m["cmd"])
	}
	// 已是 JSON -> 原样
	args2 := toFunctionArguments(`{"cmd":"echo hi"}`)
	if err := json.Unmarshal([]byte(args2), &m); err != nil || m["cmd"] != "echo hi" {
		t.Errorf("JSON 直通失败: %s", args2)
	}
	// 带转义换行的 JS 字符串（JS 源码里是 \n 两个字符）
	args3 := toFunctionArguments("const o = await tools.exec_command({ cmd: \"a\\nb\" });")
	if err := json.Unmarshal([]byte(args3), &m); err != nil {
		t.Fatalf("转义场景失败: %s", args3)
	}
	if m["cmd"] != "a\nb" && m["cmd"] != `a\nb` {
		t.Logf("换行处理: %q（可接受）", m["cmd"])
	}
}

// TestHasPriorToolResult：已有执行结果时不应再注入"你什么都没执行"的纠错。
//
// 实测：任务已跑起来、模型正常收尾输出纯文本时，纠错会把模型带偏，
// 它转而去要求用户"把原始内容再发一遍"。
func TestHasPriorToolResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"function_call_output", `[{"type":"function_call_output","call_id":"c1","output":"ok"}]`, true},
		{"custom_tool_call_output", `[{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}]`, true},
		{"首轮无结果", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"create a.txt"}]}]`, false},
		{"空 input", ``, false},
	}
	for _, c := range cases {
		raw := map[string]json.RawMessage{}
		if c.raw != "" {
			raw["input"] = json.RawMessage(c.raw)
		}
		if got := hasPriorToolResult(raw); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBridgeResultTextArray：内容数组形态的工具结果必须提取 text。
//
// CLI 的结果是 [{"type":"input_text","text":...}]，不提取的话模型读到的是
// 一坨转义 JSON，读不懂就以为"没有输出"。
func TestBridgeResultTextArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"echo hi\"}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed"},{"type":"input_text","text":"{\"chunk_id\":\"x\"}"}]}
	]`)
	items := bridgeInputItems(raw, "sys", bridgeDefaultBudget)
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "Script completed") {
		t.Error("内容数组未提取 text（模型会读到一坨转义 JSON）")
	}
	// 泄漏的特征是**转义形式**（数组被当作文本再序列化一遍），
	// 而不是正常的 input_text 类型名。
	if strings.Contains(s, `\"input_text\"`) {
		t.Error("原始 content 数组结构泄漏进了上下文（模型会读到转义 JSON）")
	}
}

// TestReplayCallText：上一轮工具调用的回放不能是空块。
//
// 背景：function_call 的参数在 arguments（JSON），custom_tool_call 在 input
// （JS 源码）。只读 input 会让 CLI v0.159 的历史回放变成空块，模型回看时
// 以为自己的命令丢了，转而要求用户重发内容。
func TestReplayCallText(t *testing.T) {
	// JSON 形态（function_call）-> 渲染回 JS
	got := replayCallText(`{"cmd":"Set-Content -Path 'a.txt' -Value 'hi'"}`)
	if !strings.Contains(got, "tools.exec_command") || !strings.Contains(got, "Set-Content") {
		t.Errorf("JSON 参数未渲染成 JS: %s", got)
	}
	idx := strings.Index(got, "{ cmd: ")
	if idx < 0 {
		t.Fatalf("缺少 cmd 参数: %s", got)
	}
	rest := strings.TrimSuffix(strings.TrimSpace(got[idx+len("{ cmd: "):]), "});")
	var s string
	if err := json.Unmarshal([]byte(rest), &s); err != nil {
		t.Errorf("渲染出的 cmd 不是合法 JSON 字符串: %v (%s)", err, rest)
	} else if s != "Set-Content -Path 'a.txt' -Value 'hi'" {
		t.Errorf("cmd 内容错误: %q", s)
	}
	// JS 形态（custom_tool_call）-> 原样
	js := `const out = await tools.exec_command({ cmd: "echo hi" });`
	if replayCallText(js) != js {
		t.Errorf("JS 应原样回放: %s", replayCallText(js))
	}
	if replayCallText("") != "" {
		t.Error("空参数应返回空（不伪造内容）")
	}
}

// TestBridgeInputReplayFunctionCall 是本次线上事故的直接回归。
//
// 真实请求里工具调用是
//
//	{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"...\"}"}
//
// 若只读 input 字段，回放块是空的 —— 模型看到自己"什么都没发过"，
// 于是回复"请把原始命令/内容再发一遍"。
func TestBridgeInputReplayFunctionCall(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"Set-Content -Path 'a.txt' -Value 'hi'\"}"},
		{"type":"function_call_output","call_id":"c1","output":"Added a.txt (+1 -0)"}
	]`)
	items := bridgeInputItems(raw, "sys", bridgeDefaultBudget)
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "Set-Content") {
		t.Error("function_call 的 arguments 未回放进上下文（历史上会变成空块）")
	}
	if !strings.Contains(s, "tools.exec_command") {
		t.Error("回放未渲染成桥约定的 JS 形态")
	}
	if strings.Contains(s, "codex-exec\\n\\n```") {
		t.Error("出现了空的 codex-exec 回放块")
	}
	if !strings.Contains(s, "Added a.txt") {
		t.Error("function_call_output 的结果未回放进上下文")
	}
}

// TestIsShellSyntaxError：区分"语法错"与"逻辑错"。
//
// 语法错要给出换语法的指引；逻辑错（文件不存在、权限不足）原样回放即可。
func TestIsShellSyntaxError(t *testing.T) {
	yes := []string{
		"Failed (exit 1)\n2 | cat > a.html <<'HTML'\n  | 重定向运算符后缺少文件规范。",
		"The '<' operator is reserved for future use.",
		"ParserError: Unexpected token 'HTML'",
		"bash: syntax error near unexpected token `newline'",
		"warning: here-document delimited by end-of-file",
	}
	for _, s := range yes {
		if !isShellSyntaxError(s) {
			t.Errorf("应判定为语法错: %s", s)
		}
	}
	no := []string{
		"ENOENT: no such file or directory, open 'a.txt'",
		"Permission denied",
		"Added pelican-bicycle.html (+168 -0)",
	}
	for _, s := range no {
		if isShellSyntaxError(s) {
			t.Errorf("不应判定为语法错: %s", s)
		}
	}
}

// TestBridgeInputItems 覆盖 Codex input 的翻译：
// 消息保留、工具调用回放为 assistant、工具结果回放为 user。
func TestBridgeInputItems(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"additional_tools","tools":[]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"be helpful"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"make a file"}]},
		{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.exec_command()"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"[CLIENT RESULT]\nfile written\n[/CLIENT RESULT]"},
		{"type":"reasoning","summary":[]}
	]`)
	items := bridgeInputItems(raw, "base", bridgeDefaultBudget)
	if len(items) == 0 {
		t.Fatal("翻译结果为空")
	}

	all := ""
	for _, it := range items {
		for _, c := range it.Content {
			all += c.Text + "\n"
		}
	}
	if !strings.Contains(all, "bridge") || !strings.Contains(all, "codex-exec") {
		t.Error("桥指令必须注入（含 codex-exec 契约）")
	}
	if strings.Contains(all, "be helpful") {
		t.Error("CLI 的 developer 消息不应折进上游请求（体积上限 + 与桥协议冲突）")
	}
	if !strings.Contains(all, "make a file") {
		t.Error("user 任务应保留")
	}
	if !strings.Contains(all, "await tools.exec_command()") {
		t.Error("custom_tool_call 应回放为 assistant 文本（上游需要自己的操作记忆）")
	}
	if !strings.Contains(all, "file written") {
		t.Error("custom_tool_call_output 应回放为 user 文本")
	}
	if strings.Contains(all, "additional_tools") {
		t.Error("additional_tools 不应出现在发给上游的文本里")
	}
}

// TestCustomToolCallItemJSON 校验 custom_tool_call 条目形状
// 与 Codex CLI 源码 ResponseItem::CustomToolCall 反序列化需求对齐
// （codex-rs/protocol/src/models.rs: call_id/name/input 为必填）。
func TestCustomToolCallItemJSON(t *testing.T) {
	item := customToolCallItemJSON("ctc_1", "await tools.exec_command()", "exec_command")
	var m map[string]any
	if err := json.Unmarshal([]byte(item), &m); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	for _, k := range []string{"id", "type", "status", "call_id", "name", "input"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少必填字段 %s: %s", k, item)
		}
	}
	// name 必须与传入一致（动态工具名：CLI 版本间 exec -> exec_command）
	if m["type"] != "custom_tool_call" || m["name"] != "exec_command" {
		t.Errorf("type/name 错误: %s", item)
	}
	if m["input"] != "await tools.exec_command()" {
		t.Errorf("input 应为 JS 源码字符串: %s", item)
	}
}

// TestBridgeInputFoldsIntoTwoItems 验证上游只认"首条 system + 最后一条 user"后的折叠形状。
func TestBridgeInputFoldsIntoTwoItems(t *testing.T) {
	raw := []byte(`[
	{"type":"message","role":"developer","content":"be helpful"},
	{"type":"message","role":"user","content":[{"type":"input_text","text":"make dir x"}]},
	{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"mkdir x\"}"},
	{"type":"function_call_output","call_id":"c1","output":""}]`)
	items := bridgeInputItems(raw, "sys", bridgeDefaultBudget)
	if len(items) != 2 || items[0].Role != "system" || items[1].Role != "user" {
		t.Fatalf("应折叠为 [system, user]，得到 %+v", items)
	}
	user := items[1].Content[0].Text
	for _, want := range []string{"make dir x", "mkdir x", "[CLIENT RESULT call_id=c1]", "无输出", "[/CLIENT RESULT]"} {
		if !strings.Contains(user, want) {
			t.Errorf("折叠后的 user 消息缺少 %q", want)
		}
	}
	if !strings.HasSuffix(user, bridgeTailReminder()) {
		t.Error("收尾强化指令必须在最后")
	}
}

func TestFoldTranscript(t *testing.T) {
	const limit = 10000
	seg := func(tag string, n int) string { return tag + strings.Repeat("x", n) }

	t.Run("未超限原样拼接", func(t *testing.T) {
		if got := foldTranscript([]string{"a", "b"}, limit); got != "a\n\nb" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("超限时尾部占大头且不超预算", func(t *testing.T) {
		var segs []string
		for i := 0; i < 100; i++ {
			segs = append(segs, seg("S", 300))
		}
		segs[0] = seg("TASK", 300)
		segs[99] = seg("LATEST", 300)
		got := foldTranscript(segs, limit)
		if len(got) > limit {
			t.Fatalf("超出上限: %d > %d", len(got), limit)
		}
		i := strings.Index(got, "中段历史")
		if i < 0 || !strings.HasPrefix(got, "TASK") || !strings.Contains(got[i:], "LATEST") {
			t.Fatal("应保留开头任务与最近状态，并标注省略")
		}
		if head, tail := i, len(got)-i; tail < 2*head-1000 {
			t.Errorf("尾部应约占 2/3：head=%d tail=%d", head, tail)
		}
	})

	t.Run("最近一段超大时截断保留而不是丢掉", func(t *testing.T) {
		got := foldTranscript([]string{"TASK", seg("LATEST", 3*limit)}, limit)
		if len(got) > limit || !strings.Contains(got, "LATEST") || !strings.Contains(got, "超长已截断") {
			t.Errorf("len=%d, 应截断保留最近一段", len(got))
		}
	})

	t.Run("截断不切断 UTF-8", func(t *testing.T) {
		got := foldTranscript([]string{"任务", strings.Repeat("中文", limit)}, limit)
		if !utf8.ValidString(got) {
			t.Error("截断切断了多字节字符")
		}
	})
}

func TestBridgeClaimsAction(t *testing.T) {
	for _, s := range []string{"I created the file a.txt", "已创建 hello.py", "我已经创建了文件", "Ran the command successfully"} {
		if !bridgeClaimsAction(s) {
			t.Errorf("应判定为声称执行过: %q", s)
		}
	}
	for _, s := range []string{"42.", "CODEX-OK", "1+1=2", ""} {
		if bridgeClaimsAction(s) {
			t.Errorf("纯回答不应触发纠错: %q", s)
		}
	}
}

// TestBridgeInputRespectsBudget 验证 developer 不折入、整条 user 消息不超预算、收尾指令仍在最后。
func TestBridgeInputRespectsBudget(t *testing.T) {
	big := strings.Repeat("persona ", 5000) // 约 40 KB，模拟 Codex 人设
	var hist []string
	for i := 0; i < 60; i++ {
		hist = append(hist, `{"type":"message","role":"user","content":"`+strings.Repeat("u", 1000)+`"}`)
	}
	raw := []byte(`[{"type":"message","role":"developer","content":"` + big + `"},` +
		`{"type":"message","role":"user","content":"TASK: build it"},` + strings.Join(hist, ",") +
		`,{"type":"message","role":"user","content":"LATEST"}]`)

	items := bridgeInputItems(raw, "", bridgeMinBudget)
	user := items[len(items)-1].Content[0].Text
	if len(user) > bridgeMinBudget {
		t.Errorf("user 消息 %d 字节，超出预算 %d", len(user), bridgeMinBudget)
	}
	if strings.Contains(user, "persona") {
		t.Error("developer 消息被折了进来")
	}
	if !strings.Contains(user, "TASK: build it") || !strings.Contains(user, "LATEST") {
		t.Error("应保留开头的任务与最近一条消息")
	}
	if !strings.HasSuffix(user, bridgeTailReminder()) {
		t.Error("收尾强化指令必须在最后")
	}
}

// TestWithBridgeNudge 验证纠错并入最后一条 user，而不是另起一条（上游只认最后一条 user）。
func TestWithBridgeNudge(t *testing.T) {
	items := []prism.InputItem{prism.NewSystemItem("sys"), prism.NewUserItem("TASK + history")}
	got := withBridgeNudge(items, "NUDGE")
	if len(got) != 2 {
		t.Fatalf("不应新增条目，得到 %d 条", len(got))
	}
	if txt := got[1].Content[0].Text; !strings.HasPrefix(txt, "TASK + history") || !strings.HasSuffix(txt, "NUDGE") {
		t.Errorf("纠错应追加在原 user 消息末尾: %q", txt)
	}
	if items[1].Content[0].Text != "TASK + history" {
		t.Error("不应修改原切片（重试失败时还要用原输入）")
	}
}
