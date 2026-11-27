package telemetry

import (
 "encoding/json"
 "fmt"
)

// ProviderUsage is a numeric-only snapshot. Nil counters mean unavailable.
// Reasoning is a subset of output for Codex, not an additional billable count.
type ProviderUsage struct {
 InputUncached *int64
 InputCached *int64
 CacheWrite *int64
 Output *int64
 Reasoning *int64
}

func validUsage(values ...*int64) bool {
 for _, v := range values { if v != nil && *v < 0 { return false } }
 return true
}

// ParseClaudeUsage accepts a single assistant message usage object, not a
// cumulative result summary. Callers must deduplicate native message IDs.
func ParseClaudeUsage(raw []byte) (ProviderUsage, error) {
 var v struct {
  Input *int64 `json:"input_tokens"`
  Read *int64 `json:"cache_read_input_tokens"`
  Write *int64 `json:"cache_creation_input_tokens"`
  Output *int64 `json:"output_tokens"`
 }
 if err := json.Unmarshal(raw, &v); err != nil { return ProviderUsage{}, err }
 if v.Input == nil || v.Output == nil || !validUsage(v.Input,v.Read,v.Write,v.Output) { return ProviderUsage{}, fmt.Errorf("invalid or incomplete Claude usage") }
 return ProviderUsage{InputUncached:v.Input,InputCached:v.Read,CacheWrite:v.Write,Output:v.Output},nil
}

// ParseCodexUsage accepts tokenUsage.total from thread/tokenUsage/updated.
// It is cumulative for the native thread; never sum successive snapshots.
func ParseCodexUsage(raw []byte) (ProviderUsage, error) {
 var v struct {
  Input *int64 `json:"inputTokens"`
  Cached *int64 `json:"cachedInputTokens"`
  Output *int64 `json:"outputTokens"`
  Reasoning *int64 `json:"reasoningOutputTokens"`
 }
 if err := json.Unmarshal(raw,&v); err != nil { return ProviderUsage{},err }
 if v.Input == nil || v.Cached == nil || v.Output == nil || !validUsage(v.Input,v.Cached,v.Output,v.Reasoning) || *v.Cached > *v.Input || (v.Reasoning != nil && *v.Reasoning > *v.Output) { return ProviderUsage{},fmt.Errorf("invalid or incomplete Codex usage") }
 uncached := *v.Input - *v.Cached
 return ProviderUsage{InputUncached:&uncached,InputCached:v.Cached,Output:v.Output,Reasoning:v.Reasoning},nil
}
