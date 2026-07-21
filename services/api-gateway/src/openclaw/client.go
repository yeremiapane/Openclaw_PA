// Package openclaw adalah client untuk menjalankan agent OpenClaw via CLI.
package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pa-ai/api-gateway/src/model"
)

// ErrNoReply menandakan agent sengaja TIDAK membalas (mis. pesan duplikat,
// atau agent memutuskan diam).
var ErrNoReply = errors.New("agent tidak membalas (NO_REPLY)")

// errEmptyOutput menandai stdout CLI kosong (transient) — memicu satu retry.
var errEmptyOutput = errors.New("output CLI kosong")

// Client membungkus pemanggilan CLI `openclaw agent`.
type Client struct {
	bin     string        // path/nama binary openclaw
	node    string        // path node
	script  string        // path openclaw.mjs; jika != "" → pakai mode node+script
	agentID string        // agent target
	timeout time.Duration // batas waktu satu turn
}

// New membuat client OpenClaw. node & script opsional: bila script kosong dan
// platform Windows, New mencoba auto-deteksi openclaw.mjs dari lokasi shim.
func New(bin, node, script, agentID string, timeout time.Duration) *Client {
	if bin == "" {
		bin = "openclaw"
	}
	if node == "" {
		node = "node"
	}
	if agentID == "" {
		agentID = "pa_communicator"
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	if script == "" && runtime.GOOS == "windows" {
		script = detectScript(bin)
	}
	c := &Client{bin: bin, node: node, script: script, agentID: agentID, timeout: timeout}
	if script != "" {
		log.Printf("[openclaw] mode node+script: %s %s", node, script)
	} else {
		log.Printf("[openclaw] mode bin langsung: %s", bin)
	}
	return c
}

// detectScript mencoba menemukan openclaw.mjs di samping shim npm.
// Shim: <npm>\openclaw.cmd ; script: <npm>\node_modules\openclaw\openclaw.mjs.
func detectScript(bin string) string {
	shim, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(shim)
	cand := filepath.Join(dir, "node_modules", "openclaw", "openclaw.mjs")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return ""
}

// AgentReply adalah kontrak output PA Communicator (JSON dibungkus markdown fence).
type AgentReply struct {
	Response         string         `json:"response"`
	Actions          []model.Action `json:"actions"`
	NewFacts         []string       `json:"newFacts"`
	RequiresApproval bool           `json:"requiresApproval"`
	ApprovalReason   string         `json:"approvalReason"`
	Meeting          *MeetingInfo   `json:"meeting,omitempty"`
}

// MeetingInfo = detail jadwal terstruktur untuk Calendar + RSVP email.
// Semua field opsional; tanpa Datetime valid, meeting hanya disimpan.
type MeetingInfo struct {
	Title           string `json:"title,omitempty"`
	Datetime        string `json:"datetime,omitempty"` // RFC3339, mis. 2026-06-26T10:00:00+07:00
	DurationMinutes int    `json:"durationMinutes,omitempty"`
	Venue           string `json:"venue,omitempty"` // kosong → online (Teams via Calendar)
	AttendeeEmail   string `json:"attendeeEmail,omitempty"`
	AttendeeName    string `json:"attendeeName,omitempty"`
}

type payload struct {
	Text string `json:"text"`
}

// Usage = hitungan token satu giliran (dari result.meta.agentMeta.usage).
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// Total menjumlahkan semua komponen token.
func (u Usage) Total() int { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// RunMeta menyimpan metadata observability satu giliran agent.
type RunMeta struct {
	RunID              string
	Status             string
	OCSessionID        string
	SessionKey         string
	Provider           string
	Model              string
	Usage              Usage
	FinishReason       string
	StopReason         string
	Refusal            bool
	DurationMs         int
	SystemPromptChars  int
	PromptChars        int
	FallbackUsed       bool
	Runner             string
	ExecutionTrace     json.RawMessage
	TruncatedBootstrap []string
	ToolsAvailable     []string
}

// HasTool melaporkan apakah tool bernama name tersedia untuk giliran ini.
func (m *RunMeta) HasTool(name string) bool {
	for _, t := range m.ToolsAvailable {
		if t == name {
			return true
		}
	}
	return false
}

type meta struct {
	DurationMs                int    `json:"durationMs"`
	FinalAssistantVisibleText string `json:"finalAssistantVisibleText"`
	FinalAssistantRawText     string `json:"finalAssistantRawText"`
	Completion                struct {
		FinishReason string `json:"finishReason"`
		StopReason   string `json:"stopReason"`
		Refusal      bool   `json:"refusal"`
	} `json:"completion"`
	AgentMeta struct {
		SessionID string `json:"sessionId"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Usage     Usage  `json:"usage"`
	} `json:"agentMeta"`
	SystemPromptReport struct {
		SessionID    string `json:"sessionId"`
		SessionKey   string `json:"sessionKey"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		SystemPrompt struct {
			Chars int `json:"chars"`
		} `json:"systemPrompt"`
		CurrentTurn struct {
			PromptChars int `json:"promptChars"`
		} `json:"currentTurn"`
		// bootstrapTruncation + injectedWorkspaceFiles: metadata pemangkasan bootstrap
		// dan file terinjeksi selalu dikirim.
		BootstrapTruncation struct {
			TruncatedFiles int `json:"truncatedFiles"`
		} `json:"bootstrapTruncation"`
		InjectedWorkspaceFiles []struct {
			Name          string `json:"name"`
			RawChars      int    `json:"rawChars"`
			InjectedChars int    `json:"injectedChars"`
			Truncated     bool   `json:"truncated"`
		} `json:"injectedWorkspaceFiles"`
		Tools struct {
			Entries []struct {
				Name string `json:"name"`
			} `json:"entries"`
		} `json:"tools"`
	} `json:"systemPromptReport"`
	ExecutionTrace struct {
		WinnerProvider string `json:"winnerProvider"`
		WinnerModel    string `json:"winnerModel"`
		FallbackUsed   bool   `json:"fallbackUsed"`
		Runner         string `json:"runner"`
	} `json:"executionTrace"`
}

// envelope menangani DUA bentuk output CLI:
//   - transport gateway  -> result.payloads[].text + result.meta
//   - fallback embedded  -> payloads[].text + meta (di root)
type envelope struct {
	RunID  string `json:"runId"`
	Status string `json:"status"`
	Result struct {
		Payloads []payload `json:"payloads"`
		Meta     meta      `json:"meta"`
	} `json:"result"`
	Payloads []payload `json:"payloads"`
	Meta     meta      `json:"meta"`
}

// activeMeta memilih meta dari result (bentuk gateway) atau root (embedded).
// Result.Meta dipakai bila punya sinyal apa pun (model/durasi/payload/teks final).
func (e *envelope) activeMeta() meta {
	rm := e.Result.Meta
	if rm.AgentMeta.Model != "" || rm.DurationMs != 0 || len(e.Result.Payloads) > 0 ||
		rm.FinalAssistantVisibleText != "" || rm.FinalAssistantRawText != "" {
		return rm
	}
	return e.Meta
}

// runMeta merakit RunMeta dari envelope, menggabungkan agentMeta &
// systemPromptReport (saling melengkapi bila salah satu kosong).
func (e *envelope) runMeta() *RunMeta {
	m := e.activeMeta()
	spr := m.SystemPromptReport
	rm := &RunMeta{
		RunID:             e.RunID,
		Status:            e.Status,
		OCSessionID:       firstNonEmpty(m.AgentMeta.SessionID, spr.SessionID),
		SessionKey:        spr.SessionKey,
		Provider:          firstNonEmpty(m.AgentMeta.Provider, spr.Provider),
		Model:             firstNonEmpty(m.AgentMeta.Model, spr.Model, m.ExecutionTrace.WinnerModel),
		Usage:             m.AgentMeta.Usage,
		FinishReason:      m.Completion.FinishReason,
		StopReason:        m.Completion.StopReason,
		Refusal:           m.Completion.Refusal,
		DurationMs:        m.DurationMs,
		SystemPromptChars: spr.SystemPrompt.Chars,
		PromptChars:       spr.CurrentTurn.PromptChars,
		FallbackUsed:      m.ExecutionTrace.FallbackUsed,
		Runner:            m.ExecutionTrace.Runner,
	}
	rm.ExecutionTrace, _ = json.Marshal(map[string]any{
		"winnerProvider":    m.ExecutionTrace.WinnerProvider,
		"winnerModel":       m.ExecutionTrace.WinnerModel,
		"fallbackUsed":      m.ExecutionTrace.FallbackUsed,
		"runner":            m.ExecutionTrace.Runner,
		"systemPromptChars": spr.SystemPrompt.Chars,
		"promptChars":       spr.CurrentTurn.PromptChars,
		"sessionKey":        spr.SessionKey,
	})

	// Rekam file bootstrap yang terpotong — sumber diam parse_error & riset gagal.
	for _, f := range spr.InjectedWorkspaceFiles {
		if f.Truncated {
			rm.TruncatedBootstrap = append(rm.TruncatedBootstrap,
				fmt.Sprintf("%s %d→%d", f.Name, f.RawChars, f.InjectedChars))
		}
	}
	for _, e := range spr.Tools.Entries {
		if e.Name != "" {
			rm.ToolsAvailable = append(rm.ToolsAvailable, e.Name)
		}
	}
	return rm
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// WithTimeout mengembalikan salinan client dengan timeout berbeda untuk satu turn.
// Dipakai untuk jalur kerja berat yang butuh waktu lebih lama.
func (c *Client) WithTimeout(d time.Duration) *Client {
	if d <= 0 {
		return c
	}
	cp := *c
	cp.timeout = d
	return &cp
}

// Inject menjalankan satu turn untuk sessionKey tertentu.
func (c *Client) Inject(ctx context.Context, sessionKey, message string) (*AgentReply, *RunMeta, error) {
	return c.InjectAgent(ctx, c.agentID, sessionKey, message)
}

// InjectAgent menjalankan satu turn untuk agent tertentu. agentID kosong memakai default client.
// Mengembalikan *RunMeta yang tetap terisi meski parse gagal atau output kosong.
func (c *Client) InjectAgent(ctx context.Context, agentID, sessionKey, message string) (*AgentReply, *RunMeta, error) {
	if agentID == "" {
		agentID = c.agentID
	}
	// CLI/gateway sesekali mengembalikan stdout kosong (exit 0) — transient.
	out, err := c.runOnce(ctx, agentID, sessionKey, message)
	if err == nil && len(strings.TrimSpace(string(out))) == 0 {
		err = errEmptyOutput
	}
	if errors.Is(err, errEmptyOutput) {
		out, err = c.runOnce(ctx, agentID, sessionKey, message)
		if err == nil && len(strings.TrimSpace(string(out))) == 0 {
			return nil, &RunMeta{}, fmt.Errorf("openclaw agent mengembalikan output kosong (2x percobaan)")
		}
	}
	if err != nil {
		return nil, &RunMeta{}, err
	}

	var env envelope
	if perr := json.Unmarshal(out, &env); perr != nil {
		return nil, &RunMeta{}, fmt.Errorf("parse envelope CLI gagal: %w (raw: %s)", perr, truncate(string(out), 600))
	}
	rm := env.runMeta()

	// Peringatkan jika bootstrap terpotong; naikkan bootstrapMaxChars atau pangkas/pisah SOUL.md.
	if len(rm.TruncatedBootstrap) > 0 {
		log.Printf("[OPENCLAW][WARN] bootstrap TERPOTONG sk=%s: %s — kontrak JSON/aturan di ekor file bisa hilang; naikkan bootstrapMaxChars",
			sessionKey, strings.Join(rm.TruncatedBootstrap, ", "))
	}

	text, err := payloadText(&env, out)
	if err != nil {
		return nil, rm, err
	}

	jsonStr := extractJSONObject(text)
	var reply AgentReply
	if err := json.Unmarshal([]byte(jsonStr), &reply); err != nil {
		return nil, rm, fmt.Errorf("parse balasan agent gagal: %w (text: %s)", err, truncate(text, 600))
	}
	if strings.TrimSpace(reply.Response) == "" {
		return nil, rm, fmt.Errorf("balasan agent kosong (text: %s)", truncate(text, 600))
	}
	return &reply, rm, nil
}

// runOnce menjalankan satu pemanggilan CLI `openclaw agent` dan mengembalikan stdout.
func (c *Client) runOnce(ctx context.Context, agentID, sessionKey, message string) ([]byte, error) {
	cliTimeout := int(c.timeout.Seconds())

	// Beri ruang ekstra agar CLI yang melaporkan timeout-nya sendiri, bukan
	// context yang membunuh proses lebih dulu.
	runCtx, cancel := context.WithTimeout(ctx, c.timeout+15*time.Second)
	defer cancel()

	args := []string{
		"agent",
		"--agent", agentID,
		"--session-key", sessionKey,
		"--message", message,
		"--json",
		"--timeout", fmt.Sprintf("%d", cliTimeout),
	}

	// Mode node+script (Windows) menghindari cmd.exe yang merusak arg multiline.
	var cmd *exec.Cmd
	if c.script != "" {
		cmd = exec.CommandContext(runCtx, c.node, append([]string{c.script}, args...)...)
	} else {
		cmd = exec.CommandContext(runCtx, c.bin, args...)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("openclaw agent gagal: %w (stderr: %s)", err, truncate(stderr.String(), 600))
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		log.Printf("[OPENCLAW] stdout kosong; stderr: %s", truncate(stderr.String(), 800))
	}
	return out, nil
}

// payloadText mengambil teks payload pertama dari envelope yang sudah di-parse,
// menangani kedua bentuk (gateway result.payloads vs embedded payloads).
func payloadText(env *envelope, out []byte) (string, error) {
	if len(env.Result.Payloads) > 0 && strings.TrimSpace(env.Result.Payloads[0].Text) != "" {
		return env.Result.Payloads[0].Text, nil
	}
	if len(env.Payloads) > 0 && strings.TrimSpace(env.Payloads[0].Text) != "" {
		return env.Payloads[0].Text, nil
	}

	// Tak ada payload teks. Bedakan "agent sengaja diam" (NO_REPLY) dari error nyata.
	m := env.activeMeta()
	if isNoReply(m.FinalAssistantVisibleText) || isNoReply(m.FinalAssistantRawText) {
		return "", ErrNoReply
	}
	return "", fmt.Errorf("tidak ada payload teks di output CLI (raw: %s)", truncate(string(out), 600))
}

// extractJSONObject mengambil objek JSON dari teks yang mungkin dibungkus
// markdown fence ```json ... ``` atau diiringi teks lain. Strategi: ambil
// dari '{' pertama hingga '}' terakhir.
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return strings.TrimSpace(text)
}

// isNoReply true bila teks final agent menandakan keputusan untuk tidak membalas.
func isNoReply(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "NO_REPLY")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
