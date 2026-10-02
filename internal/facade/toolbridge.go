package facade

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 本文件实现「Codex 工具桥」：上游当大脑，本地 Codex CLI 当手脚。
//
// 背景（2026-09-17 实测定论）：上游是 server-side tools 架构，模型的
// 终端/文件工具在云端沙箱执行并消化，客户端永远只拿到最终文本 ——
// 所以本地 CLI 的工具链（exec_command 等）一次也不会被触发，
// 模型"创建"的文件全部留在云端容器里，用户磁盘上什么都没有。
//
// 桥的思路：既然上游不理会客户端的工具定义，就反过来 ——
// 在 system 指令里明确"你没有任何执行环境"，要求它把所有操作
// 以 ```codex-exec 围栏（内含一段 JS，调用 exec_command）输出；
// 代理解析这段 JS，包装成 Responses 协议的 custom_tool_call 返回；
// Codex CLI 在本地 V8 isolate 里执行它（exec_command 跑真命令，
// 文件就落在用户磁盘），再把结果回传，代理翻译成文本继续下一轮。
//
// 注意：桥 prompt 必须显式抑制上游自带的沙箱工具，否则模型仍会
// 在云端执行然后"汇报成功" —— 用户看到一切正常，本地空空如也。

// BridgeEnabled 判断请求是否启用工具桥。
//
// Codex CLI 有**两条工具声明路径**（由模型的 use_responses_lite 元数据决定，
// 见 codex-rs/core/src/client.rs:908）：
//
//	路径 A（lite）  ：工具是 input 里的一个 additional_tools 条目，顶层 tools 为 null
//	路径 B（标准）  ：工具走顶层 tools 字段（标准 Responses API 形状）
//
// 早期只认路径 A —— 走路径 B 的客户端（不同模型/不同 CLI 版本/交互式 TUI）
// 会让桥静默失效：模型看不到桥指令，就退回**上游沙箱工具**执行，
// 然后汇报"已创建 xxx" —— 用户本地找不到文件。这是典型的"看起来成功"故障。
//
// 路径 B 的识别必须保守：普通 API 调用方也可能带 tools（自定义函数），
// 误判会把它们拖进桥模式、破坏正常 function calling。所以只在工具集里
// 出现 **Codex 独有特征**（exec/shell/apply_patch 这类 custom 工具）时才认。
func BridgeEnabled(raw map[string]json.RawMessage) bool {
	rawInput, ok := raw["input"]
	if !ok || len(rawInput) == 0 {
		return false
	}
	inputStr := string(rawInput)

	// 路径 A：CLI lite 形状。
	if strings.Contains(inputStr, `"additional_tools"`) {
		return true
	}
	// 已有工具调用往返（custom_tool_call 是 Codex 独有形状）——
	// 说明会话已经在走桥，后续轮次必须继续走桥。
	if strings.Contains(inputStr, `"custom_tool_call"`) {
		return true
	}

	// 路径 B：标准 tools 字段 + Codex 工具特征。
	//
	// 用**前缀**匹配而非全名：Codex 的工具名会随版本演进
	//（旧版 shell、新版 exec_command / write_stdin），
	// 早期写成全名 "name":"exec" 导致 "exec_command" 漏判 ——
	// 桥静默失效，模型退回上游沙箱干活。
	// 前缀匹配 `"name":"exec` 能同时覆盖 exec / exec_command / exec_*。
	toolsRaw, ok := raw["tools"]
	if !ok || len(toolsRaw) == 0 {
		return false
	}
	toolsStr := string(toolsRaw)
	if toolsStr == "null" || toolsStr == "[]" {
		return false
	}
	for _, sig := range []string{
		`"name":"exec`, `"name": "exec`,
		`"name":"shell`, `"name": "shell`,
		`"name":"write_stdin`, `"name": "write_stdin`,
		`"apply_patch`,
	} {
		if strings.Contains(toolsStr, sig) {
			return true
		}
	}
	return false
}

// bridgePrompt 是注入给上游的桥接指令。
//
// exec_command 的签名摘要来自真实 CLI 抓包（cmd 是单字符串，PTY 执行，
// Windows 走 PowerShell 语义），模型必须按它生成 JS，否则本地执行会失败。
// hasPriorToolResult 判断本次请求的历史里是否已有客户端的执行结果。
//
// 有结果 = 模型已经走过一遍桥（任务在推进或已收尾）。此时它输出纯文本
// 通常是正常总结或追问；再注入"你什么都没执行"的纠错只会把它搞懵 ——
// 实测它会转而去要求用户把原始内容再发一遍，多绕好几轮。
// 纠错只在首轮（历史里没有任何结果）才有意义：那时"什么都没执行"是事实。
func hasPriorToolResult(raw map[string]json.RawMessage) bool {
	s := string(raw["input"])
	return strings.Contains(s, `"custom_tool_call_output"`) ||
		strings.Contains(s, `"function_call_output"`)
}

func bridgePrompt() string {
	return strings.Join([]string{
		"<local_tool_bridge>",
		`You are the reasoning engine for a LOCAL coding agent (Codex CLI). The client executes ALL tools locally on the user's machine.`,
		``,
		`CRITICAL: You have NO terminal, NO file system, and NO sandbox tools in this conversation. Any built-in shell/codex/terminal tools in your runtime operate in a REMOTE SANDBOX the user cannot see. NEVER use them. NEVER claim you created, ran, or modified anything unless the client's tool result (marked [CLIENT RESULT]) confirms it.`,
		``,
		`To run any command or create/edit/delete files on the user's machine, output EXACTLY ONE fenced block:`,
		"```codex-exec",
		`const out = await tools.exec_command({ cmd: "..." });`,
		"text(out);",
		"```",
		``,
		`The block content is raw JavaScript executed by the client in a V8 isolate:`,
		`- ` + "`tools.exec_command({ cmd: string, max_output_tokens?: number })`" + ` runs one shell command in a PTY and returns its output (string).`,
		`- The client shell on Windows is PowerShell; on macOS/Linux it is bash. Write commands for the user's OS (cwd is the user's workspace).`,
		`- ` + "`text(value)`" + ` appends a result for the model to read; ` + "`exit()`" + ` ends the script.`,
		`- You may await multiple exec_command calls in one block; keep the script small and focused.`,
		``,
		`Command recipes (the exec_command cmd runs in the CLIENT's native shell — determine the user's OS from the conversation context; Windows uses PowerShell 7 (pwsh), macOS/Linux use bash):`,
		`- PREFERRED for creating/editing files: the client's built-in apply_patch. It is intercepted by the CLIENT, so its heredoc is parsed by the client — not by the shell — and behaves identically on every OS. Prefer it over shell redirection:`,
		"  apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: <path>\n+<line 1>\n+<line 2>\n*** End Patch\nPATCH",
		`  (every content line must begin with '+'; use '*** Update File: <path>' with @@ hunks to edit an existing file)`,
		`- Create/overwrite a file, Windows/PowerShell (single cmd string, newlines allowed):`,
		"  $c = @'\n<FULL FILE CONTENT>\n'@; Set-Content -LiteralPath '<path>' -Value $c -NoNewline",
		`  (single-quoted here-string @'...'@ does NOT interpolate; always include the FULL file content)`,
		`- Create/overwrite a file, macOS/Linux/bash:`,
		"  cat > '<path>' <<'EOF'\n<FULL FILE CONTENT>\nEOF",
		`- Read back: Windows "Get-Content -LiteralPath '<path>' -Raw" ; bash "cat '<path>'"`,
		`- List directory: Windows "Get-ChildItem" ; bash "ls -la"`,
		`- NEVER use bash-only syntax (printf/cat redirection/heredoc) when the client is Windows — it fails silently and wastes a turn. If the OS cannot be determined, prefer the PowerShell recipe.`,
		``,
		`Output rules: outside the block write at most one short sentence of prose. If no tool is needed, reply normally with no block. Always emit the FULL file content in the command — never abbreviate.`,
		`Do NOT emit a block for greetings, questions, or small talk, and do NOT run environment checks or "test" commands (like true/echo/ls) to probe the client — emit a block ONLY when the task itself requires an operation on the user's machine.`,
		"</local_tool_bridge>",
	}, "\n")
}

// bridgeInputItems 把 Codex CLI 的 input 数组翻译成上游 input。
//
// 上游对 input 数组的真实语义（2026-10-01 受控实验实测，见 docs/协议校准报告.md 第八节）：
//
//   - 第一条 system 条目 → 系统上下文（有效）；
//   - 最后一条 user 条目 → 当轮提示（有效）；
//   - **其余条目一律丢弃**（包括更早的 user / assistant / system）。
//
// 也就是说"在 input 里重放完整对话"对上游无效 —— 模型每轮只看到最后一条
// user 消息。这正是"模型收到文件读取结果却问'你要我干什么'"的根源。
// （chat 路径的 translateChatMessages 早已针对该缺陷把历史折进首条 system；
// 桥路径此前没有做同样的处理。）
//
// 因此这里把整个对话（环境/任务/上一轮调用/客户端执行结果）
// **全部折叠进唯一一条 user 消息**，桥指令保留为首条 system。
//
// CLI 的 developer 消息（Codex 人设、skills、multi-agent，合计约 35 KB）不折进来：
//   - 上游对这条 user 消息有未公开的体积上限，带上它们会触发
//     "This request is too large to send"；
//   - 它们是为 Codex 原生函数调用写的，与桥的 codex-exec 协议相冲突
//     （正是收尾强化指令要压制的东西）；
//   - 折叠之前它们作为非首条 system 发送，本来就被上游丢弃，桥照样能工作。
//
// AGENTS.md 与 environment_context 在 CLI 里是 user 消息，照常保留。
// budget 是这条 user 消息的字节上限（含收尾指令），见 runBridge。
func bridgeInputItems(raw json.RawMessage, defaultSystem string, budget int) []prism.InputItem {
	var blocks []struct {
		Type   string `json:"type"`
		Role   string `json:"role"`
		Name   string `json:"name"`
		CallID string `json:"call_id"`
		// 工具调用的参数：custom_tool_call 用 input，function_call 用 arguments。
		// 两者都要读 —— 只读 input 时，CLI v0.159（function 形状）的历史回放
		// 会变成**空块**，模型回看自己上一轮的命令什么都看不到，于是要求用户
		// "把原始命令/内容再发一遍"，表现得像上下文丢失。
		Input     json.RawMessage `json:"input"`
		Arguments json.RawMessage `json:"arguments"`
		Output    json.RawMessage `json:"output"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}

	textOf := func(r json.RawMessage) string {
		if len(r) == 0 {
			return ""
		}
		var s string
		if json.Unmarshal(r, &s) == nil {
			return s
		}
		// 也可能是 content 数组（[{"type":"input_text","text":"..."}]）——
		// CLI 的工具结果用这种形状。直接 string(r) 会让模型读到一坨转义
		// JSON，它读不懂就以为"没有输出"，转而要求用户重发内容。
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(r, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
		return string(r)
	}
	contentText := func(r json.RawMessage) string {
		// content 可能是字符串，也可能是 [{type,input_text/text}] 数组。
		var s string
		if json.Unmarshal(r, &s) == nil {
			return s
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(r, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
			return sb.String()
		}
		return ""
	}

	// 逐块渲染成带角色标注的文本段，稍后拼进唯一一条 user 消息。
	segments := make([]string, 0, len(blocks)+2)
	addSegment := func(header, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		segments = append(segments, "--- "+header+" ---\n"+text)
	}

	for _, b := range blocks {
		switch b.Type {
		case "message":
			role := strings.ToLower(strings.TrimSpace(b.Role))
			text := contentText(b.Content)
			switch role {
			case "developer", "system":
				// 不折叠，原因见函数注释。
			case "assistant":
				addSegment("ASSISTANT (your previous reply)", text)
			default:
				addSegment("USER", text)
			}
		case "custom_tool_call", "function_call":
			// 上游"上一轮"发出的调用：以它原始的样子回放，
			// 让上游维持自己已规划过这些操作的记忆。
			//
			// 参数同时看 input 与 arguments（见结构体注释）；渲染回桥约定的
			// JS 形态，让上下文里只存在一种调用写法，模型不易走偏。
			call := textOf(b.Input)
			if strings.TrimSpace(call) == "" {
				call = textOf(b.Arguments)
			}
			addSegment("ASSISTANT (your previous planned operation)",
				"```codex-exec\n"+replayCallText(call)+"\n```")
		case "custom_tool_call_output", "function_call_output":
			header := "[CLIENT RESULT]"
			if b.CallID != "" || b.Name != "" {
				header = "[CLIENT RESULT"
				if b.CallID != "" {
					header += " call_id=" + b.CallID
				}
				if b.Name != "" {
					header += " tool=" + b.Name
				}
				header += "]"
			}
			out := textOf(b.Output)

			// 客户端拒绝执行（工具名与它注册的不一致）。原样回放会让模型
			// 认定"我的工具不被支持"，于是反复要求用户重发任务 —— 表现得
			// 像上下文丢失，实际是它不知道该怎么办。翻译成可行动的指引。
			// （真实案例：CLI v0.159 把 exec 改名为 exec_command 后，
			//   旧会话历史里残留的 unsupported 记录会持续污染整轮对话。）
			if strings.Contains(out, "unsupported custom tool call") {
				addSegment("CLIENT RESULT", header+"\n客户端拒绝了上次调用（工具名不被支持）：`"+
					truncateRunes(out, 120)+"`。\n"+
					"这不代表你没有工具 —— 请立刻用客户端注册的工具名重新输出**完整的** "+
					"```codex-exec 块（包含全部命令与文件内容），客户端会执行它。"+
					"不要再要求用户重发任务。\n[/CLIENT RESULT]")
				continue
			}

			// 用户主动中断：既不是执行失败，也不是模型的错。明确标注，
			// 否则模型会困惑于"为什么没有结果"而反复追问。
			if strings.TrimSpace(out) == "aborted" {
				addSegment("CLIENT RESULT", header+"\n（用户主动中断了这次执行，并非工具失败。）\n[/CLIENT RESULT]")
				continue
			}

			// bash 语法用在 PowerShell 客户端上（`cat > f <<'EOF'` 等）会直接
			// 语法报错。它和"命令逻辑错"不同 —— 换个语法就能成功，所以必须
			// 把这一点告诉模型；否则它会以为内容丢了，转而去要求用户
			// "把原始内容再发一遍"（实测就是在这里绕圈的）。
			if isShellSyntaxError(out) {
				addSegment("CLIENT RESULT", header+"\n"+truncateRunes(out, 300)+"\n"+
					"CLIENT SHELL NOTE: 这个客户端的 shell 是 Windows PowerShell 7，不是 bash —— "+
					"`cat >`、`<<'EOF'` heredoc、`printf >` 这类 bash 专用语法在这里会直接语法报错。\n"+
					"请立刻改用 PowerShell 语法重发**完整的** ```codex-exec 块（内容必须完整，不要省略、不要再要求用户提供原始内容）：\n"+
					"  const out = await tools.exec_command({ cmd: \"$c = @'\n<完整文件内容>\n'@; Set-Content -LiteralPath '<路径>' -Value $c -NoNewline\" });\n"+
					"[/CLIENT RESULT]")
				continue
			}

			// 空输出也必须回放：mkdir、Set-Content 这类命令成功时就是没有输出，
			// 漏掉这一条，模型看不到"已执行"，会重做一遍或反问用户。
			if strings.TrimSpace(out) == "" {
				out = "（执行完毕，无输出）"
			}
			addSegment("CLIENT RESULT", header+"\n"+out+"\n[/CLIENT RESULT]")
		default:
			// additional_tools / reasoning / 其它非消息条目：跳过。
		}
	}

	// 收尾强化指令。LLM 对序列末尾的指令服从度最高 —— 桥指令除了放在
	// 首条 system 外，在唯一 user 消息的末尾再重申一次。
	// 放在折叠之外拼接：无论历史怎么裁剪，它都一定在最后。
	tail := transcriptSep + bridgeTailReminder()
	return []prism.InputItem{
		prism.NewSystemItem(defaultSystem + "\n\n" + bridgePrompt()),
		prism.NewUserItem(foldTranscript(segments, budget-len(tail)) + tail),
	}
}

// 桥 user 消息的字节预算。上游限额未公开（实测：约 4 KB 通过、约 40 KB 被拒），
// 所以从 bridgeDefaultBudget 起步，被拒时减半重试并记住结果，见 runBridge。
// CLI 每轮重放全量历史，长会话下折叠文本会持续增长，预算决定保留多少。
const (
	bridgeDefaultBudget = 32 << 10
	bridgeMinBudget     = 8 << 10
)

// isRequestTooLarge 判断上游是否以"请求过大"拒绝了这次生成。
// 上游原文："This request is too large to send. Please shorten your message
// or selected text and try again."，经 runner 包装为"上游生成失败 [unknown]: …"。
func isRequestTooLarge(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "too large")
}

// withBridgeNudge 把纠错消息追加到最后一条 user 消息里，而不是另起一条。
// 上游只认最后一条 user：另起一条会让模型只看到纠错、看不到任务，转而反问用户。
func withBridgeNudge(items []prism.InputItem, nudge string) []prism.InputItem {
	out := append([]prism.InputItem(nil), items...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "user" || len(out[i].Content) == 0 {
			continue
		}
		content := append([]prism.InputContent(nil), out[i].Content...)
		last := len(content) - 1
		content[last].Text += transcriptSep + nudge
		out[i].Content = content
		return out
	}
	return append(out, prism.NewUserItem(nudge))
}

const (
	transcriptSep     = "\n\n"
	transcriptOmitted = "\n\n--- [... 中段历史因长度限制被省略；任务目标见开头，最近状态见下 ...] ---\n\n"
)

// foldTranscript 把文本段拼成一段历史，超过 limit 时丢弃中段。
//
// 先从末尾装最近的段（当前操作状态，最多 2/3 预算），剩余预算再从开头装
// 最早的段（人设/任务目标）。单段本身超出预算时截断保留其开头，
// 而不是整段丢掉 —— 最近一次读文件的结果往往正是最大、也最要紧的那段。
func foldTranscript(segments []string, limit int) string {
	total := 0
	for i, s := range segments {
		if i > 0 {
			total += len(transcriptSep)
		}
		total += len(s)
	}
	if total <= limit {
		return strings.Join(segments, transcriptSep)
	}

	budget := limit - len(transcriptOmitted)
	// fill 依次装入 at(0), at(1), … 直到 room 用尽；第一段就装不下时截断装入。
	fill := func(room int, n int, at func(int) string) ([]string, int) {
		var got []string
		for k := 0; k < n; k++ {
			s := at(k)
			if len(s)+len(transcriptSep) > room {
				if len(got) == 0 {
					got = append(got, truncateBytes(s, room-len(transcriptSep)))
				}
				break
			}
			got = append(got, s)
			room -= len(s) + len(transcriptSep)
		}
		return got, room
	}

	// 尾部：最近的段优先，最多 2/3 预算。
	tail, tailLeft := fill(budget*2/3, len(segments), func(k int) string {
		return segments[len(segments)-1-k]
	})
	for a, b := 0, len(tail)-1; a < b; a, b = a+1, b-1 {
		tail[a], tail[b] = tail[b], tail[a]
	}
	// 头部：剩余预算（含尾部没用完的部分）全给开头。
	rest := len(segments) - len(tail)
	head, _ := fill(budget-budget*2/3+tailLeft, rest, func(k int) string {
		return segments[k]
	})

	if len(head) < rest {
		return strings.Join(head, transcriptSep) + transcriptOmitted + strings.Join(tail, transcriptSep)
	}
	return strings.Join(append(head, tail...), transcriptSep)
}

// truncateBytes 把 s 截到不超过 n 字节（含截断标记），不切断 UTF-8 字符。
func truncateBytes(s string, n int) string {
	const mark = "\n…[超长已截断]"
	if len(s) <= n {
		return s
	}
	cut := n - len(mark)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// bridgeRetryNudge 是"模型没用桥格式"时的自动纠正消息。
//
// 关键是要打破模型的错觉：它在云端沙箱里真的执行成功了，
// 所以它坚信任务已完成 —— 必须明确告知那个执行对用户不可见。
func bridgeRetryNudge(prevText string) string {
	return strings.Join([]string{
		`[SYSTEM CORRECTION] Your previous reply did NOT contain a ` + "```codex-exec" + ` ` + "```" + ` block, so NOTHING was executed on the user's machine. Whatever you did with your built-in tools ran inside a remote container that the user cannot see or access.`,
		`Emit the ` + "```codex-exec" + ` ` + "```" + ` block NOW with the full command (including the complete file content) so the client can execute it locally. Do not re-describe the task — output the block.`,
		`Your previous reply was: "` + truncateRunes(prevText, 300) + `"`,
	}, "\n")
}

// bridgeClaimsAction 判断模型的纯文本回复是否"声称执行过操作"。
//
// 桥的自动纠错（bridgeRetryNudge）只该打向"在云端沙箱里干完活还口头汇报"
// 的回复；对"1+1=2"这类本来就无需工具的纯回答，纠错会把模型带偏成
// 反问用户"你要我执行什么"（实测如此），反而毁掉正常回答。
// 用动作声明关键词做启发式：声称创建/写入/执行过才需要纠正。
func bridgeClaimsAction(text string) bool {
	l := strings.ToLower(text)
	for _, sig := range claimSignals {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

// claimSignals 是"声称执行过操作"的关键词（已小写）。
// 子串匹配：短词已覆盖长短语（"created" 覆盖 "created the file"）。
var claimSignals = func() []string {
	out := []string{
		"created", "wrote", "written", "saved", "modified", "deleted",
		"removed the file", "updated the file", "executed",
		"ran the command", "ran the script", "applied the patch",
	}
	// "已创建" 不是 "已经创建" 的子串，两种说法都要列。
	for _, v := range []string{"创建", "写入", "保存", "修改", "更新", "删除", "执行", "运行", "应用"} {
		out = append(out, "已"+v, "已经"+v)
	}
	return out
}()

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// replayCallText 把上一轮的工具调用参数渲染成桥约定的 JS 片段。
//
// function_call 的 arguments 是 JSON（{"cmd":"..."}），custom_tool_call 的
// input 本来就是 JS 源码。统一渲染回 JS，上下文里只存在一种调用写法，
// 上游更容易维持"我已经规划过这些操作"的记忆，也不会被 JSON 形态带偏。
func replayCallText(call string) string {
	call = strings.TrimSpace(call)
	if call == "" {
		return call
	}
	var m map[string]any
	if json.Unmarshal([]byte(call), &m) != nil {
		return call // 本来就是 JS 源码
	}
	cmd, ok := m["cmd"].(string)
	if !ok {
		return call
	}
	var sb strings.Builder
	sb.WriteString("const out = await tools.exec_command({ cmd: ")
	writeJSONString(&sb, cmd)
	sb.WriteString(" });")
	return sb.String()
}

// isShellSyntaxError 判断客户端输出是否为 shell 语法错误。
//
// 与"命令逻辑失败"（文件不存在、权限不足）区别对待：语法错只要换个写法
// 就能成功，所以翻译时要给出正确语法的示范，而不是让模型回头找内容。
// 特征串取自 PowerShell 与 POSIX shell 的真实报错文本。
func isShellSyntaxError(out string) bool {
	l := strings.ToLower(out)
	for _, sig := range []string{
		"重定向运算符后缺少文件规范",                // PowerShell 遇 `cat > f <<'EOF'` 的中文报错
		"the '<' operator is reserved", // 同场景的英文报错
		"parsererror",                  // PowerShell 语法错误通用标记
		"syntax error near unexpected token",
		"here-document", // bash heredoc 解析失败
		"unexpected eof while looking for matching",
	} {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

// bridgeTailReminder 是追加在消息序列末尾的强化指令。
func bridgeTailReminder() string {
	return strings.Join([]string{
		"<local_tool_bridge_reminder>",
		`OVERRIDE: In THIS session your built-in sandbox tools are DISABLED and DISCONNECTED. Any command you "ran" via built-in tools executed in a throwaway remote container — the user's machine was NOT touched, and the user CANNOT see those files.`,
		`The ONLY way your actions reach the user's machine is a ` + "```codex-exec" + ` ` + "```" + ` block (executed locally by the client).`,
		`If the task requires running anything or creating/editing/deleting files, you MUST emit that block in THIS reply — with the FULL command and FULL file content. Do not describe, summarize, or claim completion without it.`,
		`SHELL SYNTAX: exec_command runs in the client's native PTY — PowerShell on Windows, bash elsewhere. NEVER emit bash-only syntax (` + "`cat >`" + `, ` + "`<<'EOF'`" + ` heredocs, ` + "`printf >`" + `) unless you know the client is macOS/Linux: it fails instantly with a parser error and burns a round trip. For writing files on Windows use the single-quoted here-string recipe (` + "`$c = @'...'@; Set-Content -LiteralPath <path> -Value $c -NoNewline`" + `). If a previous [CLIENT RESULT] shows any shell parser error, switch syntax instead of re-asking the user for content.`,
		"</local_tool_bridge_reminder>",
	}, "\n")
}

// extractExecBlock 从上游回复里提取 ```codex-exec 围栏内的 JS 源码。
//
// 只认我们约定的围栏名，避免把普通代码块误当工具调用。
// 返回 ok=false 表示这条回复不含工具调用（纯文本回答）。
func extractExecBlock(text string) (string, bool) {
	const fence = "```codex-exec"
	idx := strings.Index(text, fence)
	if idx < 0 {
		return "", false
	}
	rest := text[idx+len(fence):]
	// 跳过围栏后紧跟着的换行。
	rest = strings.TrimLeft(rest, "\r\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		// 未闭合：把剩余部分整体当作块内容（流式截断时可能发生）。
		rest = strings.TrimRight(rest, "`")
	} else {
		rest = rest[:end]
	}
	js := strings.TrimSpace(rest)
	if js == "" {
		return "", false
	}
	return js, true
}

// ensureExecJS 把提取的围栏内容规范化为可执行的 JS。
//
// 实测模型经常无视"输出 JS"的要求、直接把 shell 命令写进围栏 ——
// 那样的内容进了 V8 就是 SyntaxError，然后进入"语法错误→模型困惑→
// 换个姿势再错"的死循环。与其反复纠正模型，不如代理层兜底：
// 不含 JS 特征的内容就视为一条 shell 命令，自动包上 exec_command。
func ensureExecJS(candidate string) string {
	if strings.Contains(candidate, "tools.") || strings.Contains(candidate, "await") {
		return candidate // 已经是 JS
	}
	var sb strings.Builder
	sb.WriteString(`const __out = await tools.exec_command({ cmd: `)
	writeJSONString(&sb, candidate)
	sb.WriteString(` });
text(__out);`)
	return sb.String()
}

// ExecToolName 从请求里提取客户端实际注册的 custom 工具名。
//
// **必须动态提取**：CLI 的工具名随版本演进 ——
//
//	v0.154：exec
//	v0.159：exec_command（+ write_stdin）
//
// 名字用错时客户端不执行、直接拒绝，回一条
// "[CLIENT RESULT] unsupported custom tool call: exec"，
// 而模型只看到"没执行"，于是反复说"请把内容再发一遍"——
// 表现成上下文丢失，实为工具名不匹配。
//
// 优先级：exec_command > exec > shell；都不认识时退回 "exec"（旧版兜底）。
func ExecToolName(raw map[string]json.RawMessage) string {
	hay := string(raw["tools"]) + string(raw["input"])
	for _, name := range []string{"exec_command", "exec", "shell"} {
		if strings.Contains(hay, `"name":"`+name+`"`) || strings.Contains(hay, `"name": "`+name+`"`) {
			return name
		}
	}
	return "exec"
}

// ExecToolKind 判断客户端的 shell 工具是 custom 还是 function 类型。
//
// **这是 CLI 版本适配的关键分水岭**：
//
//	v0.154：exec 是 custom 工具（type=custom，input 为自由 JS 源码）
//	v0.159：exec_command 是 function 工具（type=function，arguments 为 JSON）
//
// 回错形状时客户端**不会报错**，它只是找不到匹配的 handler、不执行这条调用；
// 下一轮构造上下文时发现"有调用无结果"，自动补一条 output:"aborted" ——
// 于是模型看到"执行被中止"，反复重试，用户看到的是无限循环。
//
// 判定方式：在 tools 定义里看该工具的 type 字段。
func ExecToolKind(raw map[string]json.RawMessage) string {
	hay := string(raw["tools"])
	if hay == "" || hay == "null" {
		hay = string(raw["input"])
	}
	// function 类型：{"type":"function","name":"exec_command",...}
	for _, sig := range []string{
		`"type":"function","name":"exec_command"`, `"type": "function", "name": "exec_command"`,
		`"type":"function","name":"exec"`, `"type": "function", "name": "exec"`,
	} {
		if strings.Contains(hay, sig) {
			return "function"
		}
	}
	return "custom"
}

// toFunctionArguments 把模型输出的块内容转成 function 工具需要的 JSON arguments。
//
// function 工具（新版 CLI）要的是 {"cmd": "..."}；但模型常按旧习惯输出
// JS 源码（const out = await tools.exec_command({cmd: "..."})）—— 这里做兜底
// 提取，两种形状都能转。解析不出 cmd 时退化成原样字符串放在 cmd 字段，
// 至少让客户端能执行一次（失败也有明确报错，而不是静默不执行）。
func toFunctionArguments(block string) string {
	trimmed := strings.TrimSpace(block)

	// 已经是 JSON 对象：{"cmd": "..."} 或 {"command": "..."}
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(trimmed), &m) == nil {
			if _, ok := m["cmd"]; !ok {
				if v, ok2 := m["command"]; ok2 {
					m["cmd"] = v
				}
			}
			if b, err := json.Marshal(m); err == nil {
				return string(b)
			}
		}
	}

	// JS 源码：提取 exec_command({ cmd: "..." }) 里的 cmd 字符串。
	if cmd, ok := extractJSCmd(trimmed); ok {
		if b, err := json.Marshal(map[string]string{"cmd": cmd}); err == nil {
			return string(b)
		}
	}

	// 兜底：整段当命令。
	if b, err := json.Marshal(map[string]string{"cmd": trimmed}); err == nil {
		return string(b)
	}
	return `{"cmd":""}`
}

// extractJSCmd 从 JS 源码里提取 cmd 参数（支持单/双引号、反引号与转义）。
func extractJSCmd(js string) (string, bool) {
	idx := strings.Index(js, "cmd:")
	if idx < 0 {
		idx = strings.Index(js, `"cmd"`)
		if idx < 0 {
			return "", false
		}
	}
	rest := js[idx:]
	// 跳到第一个引号
	q := -1
	for i, r := range rest {
		if r == '"' || r == '\'' || r == '`' {
			q = i
			break
		}
	}
	if q < 0 {
		return "", false
	}
	quote := rest[q]
	var sb strings.Builder
	escaped := false
	for i := q + 1; i < len(rest); i++ {
		c := rest[i]
		if escaped {
			switch c {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			default:
				sb.WriteByte(c)
			}
			escaped = false
			continue
		}
		if c == '\\' && quote != '`' {
			escaped = true
			continue
		}
		if c == quote {
			return sb.String(), sb.Len() > 0
		}
		sb.WriteByte(c)
	}
	return "", false
}

// customToolCallItemJSON 构造 Responses 协议的 custom_tool_call 条目。
//
// Codex 的工具是 type=custom（input 为自由 JS 源码），不是 function ——
// input 直接是源码字符串，不带 arguments 包装。
// name 来自 ExecToolName（随 CLI 版本变化，不能写死）。
func customToolCallItemJSON(id, js, toolName string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"type":"custom_tool_call","status":"completed","call_id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"name":`)
	writeJSONString(&sb, toolName)
	sb.WriteString(`,"input":`)
	writeJSONString(&sb, js)
	sb.WriteString(`}`)
	return sb.String()
}

// functionCallItemJSON 构造 Responses 协议的 function_call 条目。
//
// 新版 CLI（v0.159）把 shell 工具注册为 type=function（exec_command），
// 回传形状必须是 function_call + JSON arguments；回成 custom_tool_call
// 时客户端找不到 handler，静默不执行（下一轮被 normalize 补成 aborted）。
func functionCallItemJSON(id, name, args string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"type":"function_call","status":"completed","call_id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"name":`)
	writeJSONString(&sb, name)
	sb.WriteString(`,"arguments":`)
	writeJSONString(&sb, args)
	sb.WriteString(`}`)
	return sb.String()
}

// strippedTextItemJSON 构造去掉工具块后的纯文本 message 条目（completed 用）。
func strippedTextItemJSON(id, text string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":`)
	writeJSONString(&sb, text)
	sb.WriteString(`,"annotations":[]}]}`)
	return sb.String()
}

// reasoningItemJSON 构造 reasoning 条目，Codex 据此显示思考摘要。
func reasoningItemJSON(id, summary string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"type":"reasoning","summary":[{"type":"summary_text","text":`)
	writeJSONString(&sb, summary)
	sb.WriteString(`}]}`)
	return sb.String()
}

// bridgeItem 是桥模式一次回复里的一个输出条目。
type bridgeItem struct {
	kind string // reasoning | message | tool
	id   string
	json string
	text string // message 的正文；custom_tool_call 的 JS（需额外发 input.done 事件）
}

// bridgeOutputItems 把桥模式的一次回复拆成原生 Responses 的条目序列：
// [思考摘要] [说明文字] [工具调用]。
//
// 原生 Codex 执行命令前会先显示一句说明（"先看看目录结构"）和思考摘要；
// 只回工具调用的话，这两样在界面上都会消失，用户只看到命令一条条冒出来。
func bridgeOutputItems(text, reasoning, js, execKind, execToolName string) []bridgeItem {
	var items []bridgeItem
	if r := strings.TrimSpace(reasoning); r != "" {
		id := newID("rs_")
		items = append(items, bridgeItem{kind: "reasoning", id: id, json: reasoningItemJSON(id, r)})
	}
	msg := text
	if js != "" {
		msg = bridgePreamble(text)
	}
	// 纯文本回复即使为空也要有 message 条目，否则客户端拿不到任何输出。
	if js == "" || msg != "" {
		id := newID("msg_")
		items = append(items, bridgeItem{kind: "message", id: id, json: strippedTextItemJSON(id, msg), text: msg})
	}
	if js != "" {
		id := newID("ctc_")
		if execKind == "function" {
			items = append(items, bridgeItem{kind: "tool", id: id,
				json: functionCallItemJSON(id, execToolName, toFunctionArguments(js))})
		} else {
			items = append(items, bridgeItem{kind: "tool", id: id,
				json: customToolCallItemJSON(id, js, execToolName), text: js})
		}
	}
	return items
}

// bridgePreamble 取 codex-exec 块之前的说明文字。
func bridgePreamble(text string) string {
	if i := strings.Index(text, "```codex-exec"); i >= 0 {
		return strings.TrimSpace(text[:i])
	}
	return ""
}

func writeJSONString(sb *strings.Builder, s string) {
	// json.Marshal 默认把 < > & 转成 \u003e 等（HTML 安全模式）——
	// 对 CLI 功能无影响，但会让 exec JS 源码面目全非、难以排查。
	// 用 Encoder + SetEscapeHTML(false) 保持原字符。
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return
	}
	sb.WriteString(strings.TrimRight(buf.String(), "\n"))
}
