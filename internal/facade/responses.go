package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

// handleResponses 实现 POST /v1/responses（OpenAI 新一代 Responses API）。
//
// 这是目前 Codex CLI / 新版官方 SDK 的首选端点，
// 不实现它会导致"用官方工具链连不上"，因此必须支持。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, rawFields, err := decodeJSON[ResponsesRequest](body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}

	// Codex 桌面版的"自动生成会话标题"请求：本地直接应答，见 config.FacadeConfig.LocalTitles。
	if h.cfg.Facade.LocalTitles {
		if prompt, ok := codexTitleRequest(messagesFromResponsesInput(req.Input, "")); ok {
			h.serveLocalTitle(w, req.Stream, req.Model, titleReply(rawFields["text"], prompt))
			return
		}
	}

	// effort 三级回落：reasoning.effort > metadata.reasoning_effort > 模型映射表。
	effort := ""
	if req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	if strings.TrimSpace(effort) == "" {
		effort = metadataEffort(rawFields)
	}
	model, resolvedEffort := h.resolveModel(req.Model, effort)
	effort = resolvedEffort
	accountID, projectID := applyHeaderOverrides(r, &model, &effort)

	// 工具桥：Codex CLI 把工具声明放在 input 的 additional_tools 条目里
	// （顶层 tools 为 null）。检测到它就切换到桥模式 —— 上游当大脑，
	// 本地 CLI 当手脚，见 toolbridge.go 顶部注释。
	bridge := BridgeEnabled(rawFields)
	// 桥判定诊断：CLI 有两条工具声明路径（use_responses_lite 决定）——
	// true 走 input 里的 additional_tools 条目（我们认得），
	// false 走顶层 tools 字段（旧判据认不出，桥会静默失效，
	// 表现为模型在上游沙箱里干活、用户本地拿不到文件）。
	// 这行日志用于抓真实请求形状，排查后可按需降级为 Debug。
	toolsStr := string(rawFields["tools"])
	execToolName := ExecToolName(rawFields)
	execKind := ExecToolKind(rawFields)
	// 临时诊断：只要带 tools 就 dump（分析 CLI 实际注册的工具名）。
	if len(toolsStr) > 200 {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "oaiprism_tools_dump.json"), []byte(toolsStr), 0o600)
	}
	h.log.Info("桥判定",
		"bridge", bridge,
		"path", func() string {
			if strings.Contains(string(rawFields["input"]), `"additional_tools"`) {
				return "A/additional_tools"
			}
			if strings.Contains(string(rawFields["input"]), `"custom_tool_call"`) {
				return "A/custom_tool_call"
			}
			if toolsStr != "" && toolsStr != "null" && toolsStr != "[]" {
				return "B/tools_field"
			}
			return "none"
		}(),
		"tools_bytes", len(toolsStr),
		"input_bytes", len(rawFields["input"]),
		"exec_tool_name", execToolName,
	)
	input := messagesFromResponsesInput(req.Input, "")
	// 会话指纹要用未折叠的条目：桥模式把全量历史折进一条 user 消息，
	// 它每轮都变，拿它做指纹会让每一轮都新建项目与沙箱。
	keyItems := input
	if bridge {
		input = bridgeInputItems(req.Input, "", h.bridgeBudget())
	}
	if !bridge {
		if req.Instructions != "" {
			// instructions 就是 Responses API 的 system，保持 system 角色。
			input = append([]prism.InputItem{prism.NewSystemItem(req.Instructions)}, input...)
		} else if h.cfg.Facade.DefaultSystemPrompt != "" {
			input = append([]prism.InputItem{prism.NewSystemItem(h.cfg.Facade.DefaultSystemPrompt)}, input...)
		}
	}
	if len(input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input 不能为空")
		return
	}

	runReq := &RunRequest{
		Model:        model,
		Effort:       effort,
		UserID:       req.User,
		Input:        input,
		Metadata:     mergeMetadata(clientMetadata(rawFields), metadataWith("tools", toolsMetadata(req.Tools))),
		StickyKey:    responsesConversationKey(r, rawFields, keyItems),
		AccountID:    accountID,
		ProjectID:    projectID,
		API:          "responses",
		ExtraHeaders: extractSentinelToken(r),
	}
	// Responses API 原生就有 previous_response_id，直接映射到上游的
	// previousResponseId —— 这是最"应该"用上会话延续的一条路径。
	runReq.PreviousResponseID = req.PreviousResponseID
	runReq.ConversationID = conversationIDFrom(r, rawFields)
	runReq.Extra = passthroughFields(rawFields, responsesKnownFields)
	if bridge {
		runReq.BridgeInput = req.Input
	}

	id := newID("resp_")
	created := time.Now().Unix()

	if req.Stream {
		h.streamResponses(w, r, runReq, id, created, req.Model, bridge, execToolName, execKind, !hasPriorToolResult(rawFields))
		return
	}
	h.syncResponses(w, r, runReq, id, created, req.Model, bridge, execToolName, execKind)
}

var responsesKnownFields = map[string]struct{}{
	"model": {}, "input": {}, "instructions": {}, "stream": {},
	"max_output_tokens": {}, "temperature": {}, "top_p": {},
	"tools": {}, "tool_choice": {}, "reasoning": {}, "metadata": {},
	"previous_response_id": {}, "previousResponseId": {}, "store": {}, "user": {},
	// conversation_id 已由 conversationIDFrom 消费，不再透传（见 chatKnownFields）。
	"conversation_id": {}, "conversationId": {},
	// Codex CLI（0.15x）专有字段。这些若被当"未知字段"直通上游请求体顶层，
	// 会触发上游 400（实测：client_metadata / include / prompt_cache_key /
	// service_tier / text / parallel_tool_calls 都是 CLI 新增字段，上游不认识）。
	"client_metadata": {}, "include": {}, "prompt_cache_key": {},
	"service_tier": {}, "text": {}, "parallel_tool_calls": {},
}

func responsesConversationKey(r *http.Request, body map[string]json.RawMessage, items []prism.InputItem) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderSession)); v != "" {
		return "h:" + v
	}
	// Codex CLI 每个会话有固定 ID：body.prompt_cache_key 与 session-id 头
	// （0.159 实测两者相同；旧版本用 session_id 头）。比指纹稳定，优先使用。
	if raw, ok := body["prompt_cache_key"]; ok {
		var k string
		if json.Unmarshal(raw, &k) == nil && strings.TrimSpace(k) != "" {
			return "c:" + strings.TrimSpace(k)
		}
	}
	for _, name := range []string{"session-id", "session_id"} {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return "c:" + v
		}
	}
	conv := make([]ChatMessage, 0, len(items))
	for _, it := range items {
		var sb strings.Builder
		for _, c := range it.Content {
			sb.WriteString(c.Text)
		}
		conv = append(conv, ChatMessage{Role: it.Role, Content: stringContent(sb.String())})
	}
	return conversationKey(r, body, conv)
}

// runBridge 执行一次桥请求，正文写进 out。
//
// 上游对 user 消息有未公开的体积上限。被以"请求过大"拒绝时，把折叠历史的
// 预算减半后重试（最多减到 bridgeMinBudget），并记住成功的预算，
// 之后的请求直接按它构建，不再白白多跑一轮。拒绝发生在生成之前，
// 不会有正文写出，重试是安全的；它也不是账号级错误，不会触发账号冷却。
func (h *Handler) runBridge(ctx context.Context, req *RunRequest, out *strings.Builder) (*RunResult, error) {
	budget := h.bridgeBudget()
	emit := func(d Delta) error {
		out.WriteString(d.Text)
		return nil
	}
	for {
		out.Reset()
		res, err := h.runner.Run(ctx, req, emit)
		if !isRequestTooLarge(err) || len(req.BridgeInput) == 0 || budget <= bridgeMinBudget {
			return res, err
		}
		next := max(budget/2, bridgeMinBudget)
		smaller := bridgeInputItems(req.BridgeInput, "", next)
		// 消息本来就比预算小时，缩预算也缩不小它：上游嫌大的不是历史长度，
		// 重试只是白跑一轮，也不该把这个预算记成"学到的上限"。
		if lastUserTextLen(smaller) >= lastUserTextLen(req.Input) {
			return res, err
		}
		budget = next
		h.lowerBridgeBudget(budget)
		h.log.Warn("上游判定请求过大，缩小桥历史预算后重试", "budget", budget)
		req.Input = smaller
	}
}

// lastUserTextLen 是最后一条 user 消息的正文字节数（上游只认这一条）。
func lastUserTextLen(items []prism.InputItem) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role != "user" {
			continue
		}
		n := 0
		for _, c := range items[i].Content {
			n += len(c.Text)
		}
		return n
	}
	return 0
}

// failureCode 选择 response.failed 里的 error.code，决定 Codex 是否重试。
//
// Codex CLI 0.159 实测：server_error / rate_limit_exceeded 会重连 5 次
// （共 6 次请求）；context_length_exceeded、invalid_prompt 只请求 1 次，
// 前者显示 Codex 自己的"上下文已满"提示，后者原样显示我们的消息。
// 一律回 server_error 的话，凭据失效这类永久错误也会被重试 5 次才报出来。
func failureCode(err error) string {
	switch {
	case isRequestTooLarge(err):
		return "context_length_exceeded"
	case creds.IsRateLimited(err),
		errors.Is(err, account.ErrAllBusy),
		errors.Is(err, account.ErrAllCooling):
		return "rate_limit_exceeded"
	case creds.IsAuthError(err),
		errors.Is(err, account.ErrAllAuthFailed),
		errors.Is(err, account.ErrNoAccount):
		return "invalid_prompt"
	}
	return "server_error"
}

func (h *Handler) bridgeBudget() int {
	if v := h.bridgeLimit.Load(); v > 0 {
		return int(v)
	}
	return bridgeDefaultBudget
}

// lowerBridgeBudget 只降不升：并发请求各自学到的值取最小者。
func (h *Handler) lowerBridgeBudget(v int) {
	for {
		cur := h.bridgeLimit.Load()
		if cur > 0 && cur <= int64(v) {
			return
		}
		if h.bridgeLimit.CompareAndSwap(cur, int64(v)) {
			return
		}
	}
}

func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, bridge bool, execToolName, execKind string, allowNudge bool) {
	// 流式头必须早于首帧，只能回显客户端带回来的会话 ID（见 streamChat 注释）。
	setConversationHeader(w, runReq.ConversationID)

	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()

	itemID := newID("msg_")
	buf := make([]byte, 0, 2048)

	// 序言事件：created -> output_item.added -> content_part.added。
	// 顺序是协议强制的，客户端状态机依赖它。
	for _, ev := range []string{"response.created", "response.in_progress"} {
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
			Type: ev, ResponseID: id, Model: publicModel, CreatedAt: created,
		})
		if err := sw.WriteRaw(buf); err != nil {
			return
		}
	}

	// 心跳：等待上游期间必须持续发事件保活。
	//
	// 为什么必需：start+poll 一轮可能要 1-5 分钟（上游沙箱重试、
	// xhigh 长推理），期间若一个字节都不发，中间链路（node sidecar、
	// 反代、Nginx 的 proxy_read_timeout）会按空闲把连接掐掉，
	// 客户端表现为 "stream closed before response.completed"。
	// 实测：Codex CLI 0.154/0.159 对长时间静默同样会判流断。
	heartbeatStop := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-t.C:
				hb := AppendResponsesEvent(nil, ResponsesEvent{
					Type: "response.in_progress", ResponseID: id,
					Model: publicModel, CreatedAt: created,
				})
				if err := sw.WriteRaw(hb); err != nil {
					return // 客户端已断开，主流程会经 ctx 感知
				}
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		heartbeatWG.Wait()
	}()

	if bridge {
		// 桥模式不能边收边发：必须先拿到完整回复才能判断它是
		// 工具调用（```codex-exec 块）还是纯文本。缓冲后统一输出。
		var sb strings.Builder
		var usage *prism.Usage
		res, runErr := h.runBridge(r.Context(), runReq, &sb)

		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.failed", ResponseID: id, Model: publicModel, CreatedAt: created, Text: runErr.Error(), Status: failureCode(runErr)})
			_ = sw.WriteRaw(buf)
			return
		}
		reasoning := ""
		if res != nil {
			usage = res.Usage
			reasoning = res.Reasoning
		}
		text := sb.String()

		var js string
		if js0, ok := extractExecBlock(text); ok {
			js = ensureExecJS(js0)
		}
		// 纠错只打向"声称执行过操作"的回复（云端沙箱干完活还口头汇报）。
		// 对无需工具的纯回答（"42."、"CODEX-OK"）纠错会把模型带偏成反问，
		// 实测反而毁掉正常回答 —— 见 bridgeClaimsAction 注释。
		if js == "" && allowNudge && bridgeClaimsAction(text) {
			// 模型没用桥格式（大概率在云端沙箱里执行后口头汇报）。
			// 自动纠正一轮：明确告诉它"你的动作没到用户机器上"，
			// 要求重新以 codex-exec 块输出。只重试一次，避免循环。
			//
			// 只在历史里还没有任何执行结果时纠错（首轮）：任务已经跑起来
			// 之后模型输出纯文本是正常的收尾/追问，再指控它"什么都没执行"
			// 会把它带偏，转而去要求用户重发原始内容。
			retry := *runReq
			retry.Input = withBridgeNudge(runReq.Input, bridgeRetryNudge(text))
			var sb2 strings.Builder
			emit2 := func(d Delta) error {
				sb2.WriteString(d.Text)
				return nil
			}
			res2, runErr2 := h.runner.Run(r.Context(), &retry, emit2)
			if runErr2 == nil && res2 != nil {
				if js2, ok2 := extractExecBlock(sb2.String()); ok2 {
					js = ensureExecJS(js2)
					text = sb2.String()
				} else if strings.TrimSpace(sb2.String()) != "" {
					text = sb2.String()
				}
				if text == sb2.String() {
					usage, reasoning = res2.Usage, res2.Reasoning
				}
			}
		}

		_ = emitBridgeItems(sw, &buf, id, publicModel, created,
			bridgeOutputItems(text, reasoning, js, execKind, execToolName), usage)
		return
	}

	buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
		Type: "response.output_item.added", ItemID: itemID,
	})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}
	buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
		Type: "response.content_part.added", ItemID: itemID,
	})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}

	emit := func(d Delta) error {
		if d.Text == "" {
			return nil
		}
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
			Type: "response.output_text.delta", ItemID: itemID, Text: d.Text,
		})
		return sw.WriteRaw(buf)
	}

	res, runErr := h.runner.Run(r.Context(), runReq, emit)
	text := ""
	var usage *prism.Usage
	if res != nil {
		text = res.Text
		usage = res.Usage
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.failed", ResponseID: id, Model: publicModel, CreatedAt: created, Text: runErr.Error(), Status: failureCode(runErr)})
		_ = sw.WriteRaw(buf)
		return
	}

	// 收尾事件必须逐个发全，否则 SDK 会一直等 response.completed。
	_ = emitTextResponseEvents(sw, &buf, id, publicModel, created, itemID, text, usage)
}

// emitTextResponseEvents 发文本型回复的收尾事件序列：
// output_text.done -> content_part.done -> output_item.done -> completed。
// 返回第一个写错误（如有）。
// serveLocalTitle 直接应答 Codex 的标题请求，不建项目、不占沙箱、不经过上游。
func (h *Handler) serveLocalTitle(w http.ResponseWriter, stream bool, model, text string) {
	id, msgID, created := newID("resp_"), newID("msg_"), time.Now().Unix()
	h.log.Info("本地应答 Codex 会话标题请求")
	if !stream {
		writeJSON(w, http.StatusOK, ResponsesResponse{
			ID: id, Object: "response", CreatedAt: created, Status: "completed", Model: model,
			Output: []ResponsesItem{{
				Type: "message", ID: msgID, Role: "assistant", Status: "completed",
				Content: []ResponsesContent{{Type: "output_text", Text: text}},
			}},
		})
		return
	}
	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()
	buf := AppendResponsesEvent(nil, ResponsesEvent{
		Type: "response.created", ResponseID: id, Model: model, CreatedAt: created,
	})
	if sw.WriteRaw(buf) != nil {
		return
	}
	_ = emitBridgeItems(sw, &buf, id, model, created, []bridgeItem{
		{kind: "message", id: msgID, json: strippedTextItemJSON(msgID, text), text: text},
	}, nil)
}

// emitBridgeItems 按原生顺序输出桥模式的条目并以 response.completed 收尾（带 usage）。
//
// 每类条目沿用已在 Codex 上验证过的事件形状：reasoning 只发 output_item.done；
// message 发 output_text.done / content_part.done / output_item.done；
// 工具调用发 added / [custom_tool_call_input.done] / done。
func emitBridgeItems(sw *sse.Writer, buf *[]byte, id, publicModel string, created int64, items []bridgeItem, usage *prism.Usage) error {
	all := make([]string, 0, len(items))
	for _, it := range items {
		var evs []ResponsesEvent
		switch it.kind {
		case "message":
			evs = []ResponsesEvent{
				{Type: "response.output_text.done", ItemID: it.id, Text: it.text},
				{Type: "response.content_part.done", ItemID: it.id, Text: it.text},
				{Type: "response.output_item.done", ItemID: it.id, Text: it.text},
			}
		case "tool":
			evs = []ResponsesEvent{{Type: "response.output_item.added", ItemJSON: it.json}}
			if it.text != "" {
				// custom_tool_call 专用事件；function_call 没有这一段。
				evs = append(evs, ResponsesEvent{Type: "response.custom_tool_call_input.done", ItemID: it.id, Text: it.text})
			}
			evs = append(evs, ResponsesEvent{Type: "response.output_item.done", ItemJSON: it.json})
		default:
			evs = []ResponsesEvent{{Type: "response.output_item.done", ItemJSON: it.json}}
		}
		for _, ev := range evs {
			*buf = AppendResponsesEvent((*buf)[:0], ev)
			if err := sw.WriteRaw(*buf); err != nil {
				return err
			}
		}
		all = append(all, it.json)
	}
	*buf = AppendResponsesEvent((*buf)[:0], ResponsesEvent{
		Type: "response.completed", ResponseID: id, Model: publicModel, CreatedAt: created,
		OutputJSON: "[" + strings.Join(all, ",") + "]", Usage: usage,
	})
	return sw.WriteRaw(*buf)
}

func emitTextResponseEvents(sw *sse.Writer, buf *[]byte, id, publicModel string, created int64, itemID, text string, usage *prism.Usage) error {
	events := []ResponsesEvent{
		{Type: "response.output_text.done", ItemID: itemID, Text: text},
		{Type: "response.content_part.done", ItemID: itemID, Text: text},
		{Type: "response.output_item.done", ItemID: itemID, Text: text},
		{Type: "response.completed", ResponseID: id, Model: publicModel,
			CreatedAt: created, ItemID: itemID, Text: text, Usage: usage},
	}
	for _, ev := range events {
		*buf = AppendResponsesEvent((*buf)[:0], ev)
		if err := sw.WriteRaw(*buf); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) syncResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, bridge bool, execToolName, execKind string) {
	var res *RunResult
	var err error
	if bridge {
		var sb strings.Builder
		res, err = h.runBridge(r.Context(), runReq, &sb)
	} else {
		res, err = h.runner.Run(r.Context(), runReq, nil)
	}
	if err != nil {
		status, typ, msg := mapError(err)
		writeError(w, status, typ, msg)
		return
	}

	text := ""
	var usage *ResponsesUsage
	var conversationID string
	if res != nil {
		text = res.Text
		conversationID = res.ConversationID
		if res.Usage != nil {
			usage = &ResponsesUsage{
				InputTokens:  res.Usage.InputTokens,
				OutputTokens: res.Usage.OutputTokens,
				TotalTokens:  res.Usage.TotalTokens,
			}
		}
	}

	if bridge {
		// 桥模式：有 exec 块就回 custom_tool_call（CLI 认的形状），
		// 没有就回普通文本 message。
		if js0, ok := extractExecBlock(text); ok {
			reasoning := ""
			if res != nil {
				reasoning = res.Reasoning
			}
			items := bridgeOutputItems(text, reasoning, ensureExecJS(js0), execKind, execToolName)
			out := make([]json.RawMessage, 0, len(items))
			for _, it := range items {
				out = append(out, json.RawMessage(it.json))
			}
			body := map[string]any{
				"id": id, "object": "response", "created_at": created,
				"status": "completed", "model": publicModel,
				"output": out,
			}
			if usage != nil {
				body["usage"] = usage
			}
			setConversationHeader(w, conversationID)
			writeJSON(w, http.StatusOK, body)
			return
		}
		text = stripExecFence(text)
	}
	resp := ResponsesResponse{
		ID:        id,
		Object:    "response",
		CreatedAt: created,
		Status:    "completed",
		Model:     publicModel,
		Output: []ResponsesItem{{
			Type:   "message",
			ID:     newID("msg_"),
			Role:   "assistant",
			Status: "completed",
			Content: []ResponsesContent{{
				Type: "output_text",
				Text: text,
			}},
		}},
	}
	if usage != nil {
		resp.Usage = usage
	}
	if conversationID != "" {
		resp.ConversationID = conversationID
		setConversationHeader(w, conversationID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// stripExecFence 去掉 codex-exec 围栏（桥模式纯文本路径不再展示它）。
func stripExecFence(text string) string {
	const fence = "```codex-exec"
	idx := strings.Index(text, fence)
	if idx < 0 {
		return text
	}
	end := strings.Index(text[idx:], "```")
	if end < 0 {
		return strings.TrimSpace(text[:idx])
	}
	return strings.TrimSpace(text[:idx] + text[idx+end+3:])
}

// extractSentinelToken 从下游请求头中提取 openai-sentinel-token 或 x-openai-sentinel-token。
func extractSentinelToken(r *http.Request) map[string]string {
	extra := make(map[string]string)
	if tok := strings.TrimSpace(r.Header.Get("openai-sentinel-token")); tok != "" {
		extra["openai-sentinel-token"] = tok
	} else if tok := strings.TrimSpace(r.Header.Get("x-openai-sentinel-token")); tok != "" {
		extra["openai-sentinel-token"] = tok
	}
	return extra
}

// heartbeatInterval 是流式等待期间的心跳间隔。
//
// 15 秒：远小于常见反代的 proxy_read_timeout（默认 60s）
// 与 node http 的默认超时，又不会显著增加事件量
// （一轮 5 分钟的请求约多 20 个事件，可忽略）。
const heartbeatInterval = 15 * time.Second
