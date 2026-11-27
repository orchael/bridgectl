package telemetry

import "testing"

func TestParseProviderUsage(t *testing.T) {
 c, err := ParseClaudeUsage([]byte(`{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":5,"output_tokens":3}`))
 if err != nil || *c.InputUncached != 10 || *c.InputCached != 20 || *c.CacheWrite != 5 || *c.Output != 3 || c.Reasoning != nil { t.Fatalf("Claude: %+v %v",c,err) }
 x, err := ParseCodexUsage([]byte(`{"inputTokens":30,"cachedInputTokens":20,"outputTokens":8,"reasoningOutputTokens":3}`))
 if err != nil || *x.InputUncached != 10 || *x.InputCached != 20 || *x.Output != 8 || *x.Reasoning != 3 || x.CacheWrite != nil { t.Fatalf("Codex: %+v %v",x,err) }
 for _, raw := range []string{`{}`,`{"inputTokens":1,"cachedInputTokens":2,"outputTokens":3}`,`{"inputTokens":1,"cachedInputTokens":0,"outputTokens":3,"reasoningOutputTokens":4}`} {
  if _,err := ParseCodexUsage([]byte(raw)); err == nil { t.Fatalf("accepted %s",raw) }
 }
 if _,err := ParseClaudeUsage([]byte(`{"input_tokens":-1,"output_tokens":2}`)); err == nil { t.Fatal("accepted negative usage") }
}
