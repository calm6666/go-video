package eventenvelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewValid(t *testing.T) {
	payload := json.RawMessage(`{"submission_id":"sub_123"}`)
	e, err := New("video", "content.published", "submission", "sub_123", 1, payload, "trace-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.EventID) != 26 {
		t.Errorf("event_id length = %d, want 26", len(e.EventID))
	}
	if e.OccurredAt == "" {
		t.Error("occurred_at should be set")
	}
	if !strings.Contains(e.OccurredAt, "T") || !strings.HasSuffix(e.OccurredAt, "Z") {
		t.Errorf("occurred_at %q is not RFC3339 UTC", e.OccurredAt)
	}
	if string(e.Payload) != `{"submission_id":"sub_123"}` {
		t.Errorf("payload mismatch: %s", e.Payload)
	}
}

func TestNewNilPayloadDefaultsToEmptyObject(t *testing.T) {
	e, err := New("video", "content.published", "submission", "sub_1", 1, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(e.Payload) != `{}` {
		t.Errorf("nil payload should default to {}, got %s", e.Payload)
	}
}

func TestNewRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name          string
		producer      string
		eventType     string
		aggregateType string
		aggregateID   string
		schemaVersion int
	}{
		{"empty producer", "", "content.published", "submission", "id", 1},
		{"empty event_type", "video", "", "submission", "id", 1},
		{"invalid event_type", "video", "Content.PUBLISHED", "submission", "id", 1},
		{"trailing dot event_type", "video", "content.", "submission", "id", 1},
		{"empty aggregate_type", "video", "content.published", "", "id", 1},
		{"empty aggregate_id", "video", "content.published", "submission", "", 1},
		{"zero schema_version", "video", "content.published", "submission", "id", 0},
		{"negative schema_version", "video", "content.published", "submission", "id", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.producer, c.eventType, c.aggregateType, c.aggregateID, c.schemaVersion, json.RawMessage(`{}`), "")
			if err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	original, err := New("engagement", "engagement.action", "user", "u_1", 2, json.RawMessage(`{"action":"like"}`), "trace-xyz")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := original.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	var decoded Envelope
	if err := decoded.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	if decoded.EventID != original.EventID {
		t.Errorf("event_id mismatch: %q vs %q", decoded.EventID, original.EventID)
	}
	if decoded.EventType != original.EventType {
		t.Errorf("event_type mismatch: %q vs %q", decoded.EventType, original.EventType)
	}
	if decoded.SchemaVersion != original.SchemaVersion {
		t.Errorf("schema_version mismatch: %d vs %d", decoded.SchemaVersion, original.SchemaVersion)
	}
	if decoded.Producer != original.Producer {
		t.Errorf("producer mismatch: %q vs %q", decoded.Producer, original.Producer)
	}
	if string(decoded.Payload) != string(original.Payload) {
		t.Errorf("payload mismatch: %q vs %q", decoded.Payload, original.Payload)
	}
}

func TestUnmarshalRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"missing event_id", `{"event_type":"content.published","schema_version":1,"occurred_at":"2026-08-24T10:00:00Z","producer":"video","aggregate_type":"submission","aggregate_id":"s1","payload":{}}`},
		{"missing payload", `{"event_id":"01J9HQA3KX7P2R0V1QTN8D5R4M","event_type":"content.published","schema_version":1,"occurred_at":"2026-08-24T10:00:00Z","producer":"video","aggregate_type":"submission","aggregate_id":"s1"}`},
		{"bad occurred_at", `{"event_id":"01J9HQA3KX7P2R0V1QTN8D5R4M","event_type":"content.published","schema_version":1,"occurred_at":"not-a-time","producer":"video","aggregate_type":"submission","aggregate_id":"s1","payload":{}}`},
		{"non-JSON payload", `{"event_id":"01J9HQA3KX7P2R0V1QTN8D5R4M","event_type":"content.published","schema_version":1,"occurred_at":"2026-08-24T10:00:00Z","producer":"video","aggregate_type":"submission","aggregate_id":"s1","payload":notjson}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var e Envelope
			if err := e.UnmarshalJSON([]byte(c.json)); err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestTopic(t *testing.T) {
	cases := []struct {
		eventType string
		version   int
		want      string
	}{
		{"content.published", 1, "content.published.v1"},
		{"engagement.action", 3, "engagement.action.v3"},
		{"", 1, ""},
		{"content.published", 0, ""},
		{"content.published", -1, ""},
	}
	for _, c := range cases {
		if got := Topic(c.eventType, c.version); got != c.want {
			t.Errorf("Topic(%q, %d) = %q, want %q", c.eventType, c.version, got, c.want)
		}
	}
}

func TestValidateNilReceiver(t *testing.T) {
	var e *Envelope
	if err := e.Validate(); err == nil {
		t.Error("nil receiver should fail validation")
	}
}

func TestMarshalRejectsInvalidEnvelope(t *testing.T) {
	e := &Envelope{EventType: "Content.Published"} // uppercase invalid
	if _, err := e.MarshalJSON(); err == nil {
		t.Error("MarshalJSON should reject invalid envelope")
	}
}
