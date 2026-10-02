package facade

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

const titleTemplate = "You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task that will be created from that prompt.\nGenerate a concise UI title (up to 36 characters) for this task.\n\nUser prompt:\n"

func TestCodexTitleRequest(t *testing.T) {
	items := []prism.InputItem{prism.NewUserItem(titleTemplate + "Fix the login bug on Windows\nmore detail")}
	prompt, ok := codexTitleRequest(items)
	if !ok || prompt != "Fix the login bug on Windows\nmore detail" {
		t.Fatalf("应识别标题请求并取出提示词，得到 ok=%v prompt=%q", ok, prompt)
	}
	if _, ok := codexTitleRequest([]prism.InputItem{prism.NewUserItem("please give this a short title")}); ok {
		t.Error("普通消息不应被当成标题请求")
	}
}

func TestTitleReply(t *testing.T) {
	schema := json.RawMessage(`{"format":{"type":"json_schema","name":"title","schema":{"type":"object",` +
		`"properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}}}`)
	prompt := "A previously isolated, newly discovered, randomly mating mammalian species raises its young for 18 years. " +
		strings.Repeat("More context. ", 20)

	var got map[string]string
	if err := json.Unmarshal([]byte(titleReply(schema, prompt)), &got); err != nil {
		t.Fatalf("带 schema 时应回 JSON: %v", err)
	}
	if n := utf8.RuneCountInString(got["title"]); n == 0 || n > titleMaxRunes {
		t.Errorf("title 长度 %d 不在 1..%d: %q", n, titleMaxRunes, got["title"])
	}
	if n := utf8.RuneCountInString(got["description"]); n == 0 || n > descriptionMaxRunes {
		t.Errorf("description 长度 %d 不在 1..%d", n, descriptionMaxRunes)
	}
	if strings.HasSuffix(got["title"], ",") || strings.HasSuffix(got["title"], " ") {
		t.Errorf("title 不应以标点或空格结尾: %q", got["title"])
	}

	if plain := titleReply(nil, "Locate foo_bar"); plain != "Locate foo_bar" {
		t.Errorf("无 schema 时应回纯文本，得到 %q", plain)
	}
}

func TestClipText(t *testing.T) {
	cases := map[string]string{
		"Fix foo()":                                        "Fix foo()",
		"## **Add** dark-mode support.":                    "Add dark-mode support",
		"修复登录时出现 500 错误的问题。":                               "修复登录时出现 500 错误的问题",
		"one two three four five six seven eight nine ten": "one two three four five six seven",
	}
	for in, want := range cases {
		if got := clipText(in, titleMaxRunes); got != want {
			t.Errorf("clipText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("长", 50)
	if n := utf8.RuneCountInString(clipText(long, titleMaxRunes)); n != titleMaxRunes {
		t.Errorf("无空格文本应按字符截到 %d，得到 %d", titleMaxRunes, n)
	}
}
