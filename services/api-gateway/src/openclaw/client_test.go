package openclaw

import (
	"encoding/json"
	"errors"
	"testing"
)

// extractPayloadText: wrapper uji yang mereplikasi alur lama (parse envelope →
// ambil teks payload) di atas helper baru payloadText.
func extractPayloadText(out []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", err
	}
	return payloadText(&env, out)
}

func TestExtractPayloadTextNoReply(t *testing.T) {
	cases := []string{
		`{"result":{"payloads":[],"meta":{"finalAssistantVisibleText":"NO_REPLY","finalAssistantRawText":"NO_REPLY"}}}`,
		`{"payloads":[],"meta":{"finalAssistantVisibleText":"NO_REPLY"},"transport":"embedded"}`,
	}
	for _, raw := range cases {
		if _, err := extractPayloadText([]byte(raw)); !errors.Is(err, ErrNoReply) {
			t.Fatalf("EXPECT ErrNoReply untuk raw %q, dapat %v", raw, err)
		}
	}
}

func TestExtractPayloadText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "gateway transport (result.payloads)",
			raw:  `{"runId":"r1","status":"ok","result":{"payloads":[{"text":"halo"}]}}`,
			want: "halo",
		},
		{
			name: "embedded fallback (payloads di root)",
			raw:  `{"payloads":[{"text":"halo-embedded"}],"transport":"embedded"}`,
			want: "halo-embedded",
		},
		{
			name: "result kosong -> fallback ke root payloads",
			raw:  `{"result":{"payloads":[]},"payloads":[{"text":"fallback"}]}`,
			want: "fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractPayloadText([]byte(tc.raw))
			if err != nil {
				t.Fatalf("error tak terduga: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractPayloadTextErrors(t *testing.T) {
	for _, raw := range []string{
		`bukan json`,
		`{"result":{"payloads":[]},"payloads":[]}`,
		`{}`,
	} {
		if _, err := extractPayloadText([]byte(raw)); err == nil {
			t.Fatalf("harusnya error untuk raw %q", raw)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fenced ```json",
			in:   "```json\n{\"response\":\"hai\",\"requiresApproval\":false}\n```",
			want: `{"response":"hai","requiresApproval":false}`,
		},
		{
			name: "fence tanpa label bahasa",
			in:   "```\n{\"a\":1}\n```",
			want: `{"a":1}`,
		},
		{
			name: "ada preamble teks",
			in:   "Berikut hasilnya:\n{\"a\":1}",
			want: `{"a":1}`,
		},
		{
			name: "json polos",
			in:   `{"a":1}`,
			want: `{"a":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSONObject(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Validasi parsing penuh: dari output CLI mentah -> AgentReply.
func TestParseFullPipeline(t *testing.T) {
	raw := "{\"result\":{\"payloads\":[{\"text\":\"```json\\n{\\\"response\\\":\\\"Baik, akan saya teruskan ke beliau.\\\",\\\"newFacts\\\":[\\\"dari PT Marteux\\\"],\\\"requiresApproval\\\":true,\\\"approvalReason\\\":\\\"permintaan jadwal pertemuan\\\"}\\n```\"}]}}"

	text, err := extractPayloadText([]byte(raw))
	if err != nil {
		t.Fatalf("extractPayloadText: %v", err)
	}
	jsonStr := extractJSONObject(text)

	var reply AgentReply
	if err := json.Unmarshal([]byte(jsonStr), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Response == "" || !reply.RequiresApproval {
		t.Fatalf("reply tidak sesuai: %+v", reply)
	}
	if len(reply.NewFacts) != 1 || reply.NewFacts[0] != "dari PT Marteux" {
		t.Fatalf("newFacts salah: %+v", reply.NewFacts)
	}
	if reply.ApprovalReason == "" {
		t.Fatalf("approvalReason kosong")
	}
}
