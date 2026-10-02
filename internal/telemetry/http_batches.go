package telemetry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const bridgeMaxRequestBytes = 1 << 20
const bridgeMaxEvents = 1000
const bridgeFragmentBytes = 192 << 10

type bridgeBatch struct {
	ID   string
	Body []byte
}

type bridgeWireSegment struct {
	CreatedAt string            `json:"created_at"`
	Collector json.RawMessage   `json:"collector"`
	Events    []json.RawMessage `json:"events"`
}

// bridgeBatches uses the encoded HTTP size, not the local JSONL size. Child
// IDs are content addressed; retries/restarts cannot reuse an ID for new bytes.
// Event IDs remain anchored to the original spool segment for logical dedup.
func bridgeBatches(id string, jsonl []byte, version string) ([]bridgeBatch, error) {
	envelope, err := bridgeEnvelope(id, jsonl, version)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Segment bridgeWireSegment `json:"segment"`
	}
	if err := json.Unmarshal(envelope, &wire); err != nil {
		return nil, err
	}
	segment := wire.Segment
	events := make([]json.RawMessage, 0, len(segment.Events))
	for _, raw := range segment.Events {
		if len(raw) < bridgeMaxRequestBytes-4096 {
			events = append(events, raw)
			continue
		}
		fragments, err := fragmentBridgeEvent(raw)
		if err != nil {
			return nil, err
		}
		events = append(events, fragments...)
	}
	segment.Events = []json.RawMessage{}
	empty, err := encodeBridgeBatch(segment)
	if err != nil {
		return nil, err
	}
	overhead := len(empty.Body)
	var batches []bridgeBatch
	size := overhead
	flush := func() error {
		if len(segment.Events) == 0 {
			return nil
		}
		batch, err := encodeBridgeBatch(segment)
		if err != nil {
			return err
		}
		if len(batch.Body) > bridgeMaxRequestBytes {
			return fmt.Errorf("telemetry batch exceeds request limit")
		}
		batches = append(batches, batch)
		segment.Events = []json.RawMessage{}
		size = overhead
		return nil
	}
	for _, raw := range events {
		extra := len(raw)
		if len(segment.Events) > 0 {
			extra++
		}
		if len(segment.Events) == bridgeMaxEvents || size+extra > bridgeMaxRequestBytes {
			if err := flush(); err != nil {
				return nil, err
			}
			extra = len(raw)
		}
		segment.Events = append(segment.Events, raw)
		size += extra
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return batches, nil
}

func encodeBridgeBatch(segment bridgeWireSegment) (bridgeBatch, error) {
	raw, err := json.Marshal(segment)
	if err != nil {
		return bridgeBatch{}, err
	}
	sum := sha256.Sum256(raw)
	checksum := hex.EncodeToString(sum[:])
	id := "batch-" + checksum
	body, err := json.Marshal(map[string]any{"schema_version": 1, "segment_id": id, "segment_checksum": "sha256:" + checksum, "segment": json.RawMessage(raw)})
	return bridgeBatch{ID: id, Body: body}, err
}

// Transport fragments carry the exact normalized Bridge event, not raw provider
// data. A reader must verify the hash and all fragment indices before counting
// the reconstructed event toward completeness.
func fragmentBridgeEvent(raw json.RawMessage) ([]json.RawMessage, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	count := (len(raw) + bridgeFragmentBytes - 1) / bridgeFragmentBytes
	result := make([]json.RawMessage, 0, count)
	for i := 0; i < count; i++ {
		fragment := make(map[string]json.RawMessage, len(event))
		for key, value := range event {
			fragment[key] = value
		}
		fragment["event_id"], _ = json.Marshal(fmt.Sprintf("fragment-%s-%d", hash, i))
		fragment["event_type"] = json.RawMessage(`"telemetry_fragment"`)
		payload, err := json.Marshal(map[string]any{"fragment": map[string]any{
			"encoding": "base64-json", "sha256": hash, "index": i, "count": count,
			"data": base64.StdEncoding.EncodeToString(raw[i*bridgeFragmentBytes : min((i+1)*bridgeFragmentBytes, len(raw))]),
		}})
		if err != nil {
			return nil, err
		}
		fragment["payload"] = payload
		encoded, err := json.Marshal(fragment)
		if err != nil {
			return nil, err
		}
		result = append(result, encoded)
	}
	return result, nil
}
