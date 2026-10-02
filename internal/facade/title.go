package facade

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// Codex 桌面版每发一条消息，都会另开一个隐藏会话请模型起标题。
// 请求里的固定模板以这段话开头，用户提示词跟在最后的 "User prompt:" 之后。
const (
	codexTitleMarker = "your job is to provide a short title for a task"
	codexTitlePrompt = "User prompt:"

	titleMaxRunes       = 36  // 模板要求：标题不超过 36 个字符
	descriptionMaxRunes = 100 // 模板要求：描述不超过 100 个字符
)

// codexTitleRequest 识别 Codex 的标题生成请求，返回其中的用户提示词。
func codexTitleRequest(items []prism.InputItem) (string, bool) {
	for _, it := range items {
		for _, c := range it.Content {
			if !strings.Contains(c.Text, codexTitleMarker) {
				continue
			}
			if i := strings.LastIndex(c.Text, codexTitlePrompt); i >= 0 {
				return strings.TrimSpace(c.Text[i+len(codexTitlePrompt):]), true
			}
			return "", true
		}
	}
	return "", false
}

// titleReply 生成标题请求的回复正文。
//
// 请求带 json_schema 结构化输出时（Codex 要 title / description 两个字段），
// 按 schema 声明的字段名填 JSON；字段名含 desc / summary 的填描述，其余填标题。
// 不带 schema 时回纯文本标题。
func titleReply(textField json.RawMessage, prompt string) string {
	title := clipText(firstLine(prompt), titleMaxRunes)
	if title == "" {
		title = "New task"
	}

	var spec struct {
		Format struct {
			Type   string `json:"type"`
			Schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schema"`
		} `json:"format"`
	}
	if len(textField) == 0 || json.Unmarshal(textField, &spec) != nil ||
		spec.Format.Type != "json_schema" || len(spec.Format.Schema.Properties) == 0 {
		return title
	}

	description := clipText(strings.Join(strings.Fields(prompt), " "), descriptionMaxRunes)
	out := make(map[string]string, len(spec.Format.Schema.Properties))
	for name := range spec.Format.Schema.Properties {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "desc") || strings.Contains(lower, "summary") {
			out[name] = description
		} else {
			out[name] = title
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return title
	}
	return string(b)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// clipText 规整空白、去掉 Markdown 符号，截到 max 个字符以内并去掉结尾标点。
// 含空格的文本（拉丁文）尽量在词边界截断。
func clipText(s string, max int) string {
	s = strings.NewReplacer("`", "", "*", "", "\"", "", "“", "", "”", "").Replace(s)
	s = strings.TrimLeft(strings.Join(strings.Fields(s), " "), "#>-+ ")
	r := []rune(s)
	if len(r) > max {
		cut := max
		for i := max; i > max/2; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		r = r[:cut]
	}
	// 只去句末标点，保留 ")" 之类，免得 "Fix foo()" 变成 "Fix foo("。
	return strings.TrimRightFunc(string(r), func(c rune) bool {
		return unicode.IsSpace(c) || strings.ContainsRune(".,;:!?。，；：！？、…—-", c)
	})
}
