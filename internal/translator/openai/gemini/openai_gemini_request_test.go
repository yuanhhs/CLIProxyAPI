package gemini

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToOpenAIToolIDsAreDeterministicAndMatchedByName(t *testing.T) {
	input := []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"first","args":{"value":1}}},
				{"functionCall":{"name":"second","args":{"value":2}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"second","response":{"content":"two"}}},
				{"functionResponse":{"name":"first","response":{"content":"one"}}}
			]}
		]
	}`)

	first := ConvertGeminiRequestToOpenAI("test-model", input, false)
	second := ConvertGeminiRequestToOpenAI("test-model", input, false)
	if !bytes.Equal(first, second) {
		t.Fatalf("translation is not deterministic:\n%s\n%s", first, second)
	}

	firstCallID := gjson.GetBytes(first, "messages.0.tool_calls.0.id").String()
	secondCallID := gjson.GetBytes(first, "messages.0.tool_calls.1.id").String()
	if firstCallID == "" || secondCallID == "" || firstCallID == secondCallID {
		t.Fatalf("invalid tool call IDs: first=%q second=%q", firstCallID, secondCallID)
	}
	if got := gjson.GetBytes(first, "messages.1.tool_call_id").String(); got != secondCallID {
		t.Fatalf("second function response ID = %q, want %q", got, secondCallID)
	}
	if got := gjson.GetBytes(first, "messages.2.tool_call_id").String(); got != firstCallID {
		t.Fatalf("first function response ID = %q, want %q", got, firstCallID)
	}
}

func TestExplicitGeminiToolIDSupportsCamelCase(t *testing.T) {
	node := gjson.Parse(`{"callId":"explicit-call"}`)
	if got := explicitGeminiToolID(node); got != "explicit-call" {
		t.Fatalf("explicitGeminiToolID = %q, want explicit-call", got)
	}
}
