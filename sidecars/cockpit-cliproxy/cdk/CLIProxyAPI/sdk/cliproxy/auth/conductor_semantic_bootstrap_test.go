package auth

import (
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSummarizeOpenAIResponsesBootstrapCountsOnlyEventTypes(t *testing.T) {
	t.Parallel()
	chunks := []cliproxyexecutor.StreamChunk{
		{Payload: []byte(`data: {"type":"response.created","response":{"instructions":"private prompt"}}`)},
		{Payload: []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"private reasoning"}`)},
		{Payload: []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"more private reasoning"}`)},
		{Payload: []byte(`data: {"type":"response.completed","response":{"output":[]}}`)},
	}

	got := summarizeOpenAIResponsesBootstrap(chunks)
	want := "response.created=1,response.reasoning_summary_text.delta=2,response.completed=1"
	if got != want {
		t.Fatalf("bootstrap summary = %q, want %q", got, want)
	}
	for _, privateValue := range []string{"private prompt", "private reasoning", "more private reasoning"} {
		if strings.Contains(got, privateValue) {
			t.Fatalf("bootstrap summary leaked payload %q: %q", privateValue, got)
		}
	}
}

func TestSummarizeOpenAIResponsesBootstrapSanitizesEventLabels(t *testing.T) {
	t.Parallel()
	chunks := []cliproxyexecutor.StreamChunk{
		{Payload: []byte("data: {\"type\":\"response.created\\nsecret\"}")},
	}

	got := summarizeOpenAIResponsesBootstrap(chunks)
	if got != "response.created_secret=1" {
		t.Fatalf("sanitized bootstrap summary = %q", got)
	}
}
