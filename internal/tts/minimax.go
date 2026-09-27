package tts

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Default MiniMax settings; all overridable via environment variables.
const (
	minimaxDefaultBaseURL    = "https://api.minimaxi.com"
	minimaxDefaultModel      = "speech-02-hd"
	minimaxDefaultVoice      = "female-shaonv"
	minimaxDefaultSampleRate = 32000
)

// MiniMaxProvider synthesizes speech via the MiniMax t2a_v2 API
// (https://api.minimaxi.com / https://api.minimax.cn).
//
// Environment variables:
//
//	MINIMAX_API_KEY      (required) API key
//	MINIMAX_BASE_URL     (optional) default https://api.minimaxi.com; use
//	                     https://api.minimax.cn for the China endpoint
//	MINIMAX_MODEL        (optional) default speech-02-hd
//	MINIMAX_VOICE        (optional) default voice id, default female-shaonv
//	MINIMAX_SAMPLE_RATE  (optional) default 32000
type MiniMaxProvider struct {
	apiKey     string
	baseURL    string
	model      string
	voice      string
	sampleRate int
	httpClient *http.Client
}

// NewMiniMaxProvider creates a provider configured from the environment.
func NewMiniMaxProvider() *MiniMaxProvider {
	baseURL := envOr("MINIMAX_BASE_URL", minimaxDefaultBaseURL)
	model := envOr("MINIMAX_MODEL", minimaxDefaultModel)
	voice := envOr("MINIMAX_VOICE", minimaxDefaultVoice)

	sampleRate := minimaxDefaultSampleRate
	if v := os.Getenv("MINIMAX_SAMPLE_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			sampleRate = n
		}
	}

	return &MiniMaxProvider{
		apiKey:     os.Getenv("MINIMAX_API_KEY"),
		baseURL:    baseURL,
		model:      model,
		voice:      voice,
		sampleRate: sampleRate,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Name implements Provider.
func (m *MiniMaxProvider) Name() string { return "minimax" }

// DefaultVoice implements Provider.
func (m *MiniMaxProvider) DefaultVoice() string { return m.voice }

// minimaxSuggestedVoices lists common system voice ids for discoverability.
// MiniMax also accepts voice cloning ids, so validation stays permissive and
// the API rejects unknown ids with a descriptive message.
var minimaxSuggestedVoices = []string{
	"female-shaonv",       // 少女
	"female-yujie",        // 御姐
	"female-chengshu",     // 成熟女声
	"female-tianmei",      // 甜美女声
	"male-qn-qingse",      // 青涩男声
	"male-qn-jingying",    // 精英男声
	"male-qn-badao",       // 霸道男声
	"male-qn-daxuesheng",  // 大学生男声
	"audiobook_female_1",  // 有声书女声
	"audiobook_male_1",    // 有声书男声
	"presenter_female",    // 女主持人
	"presenter_male",      // 男主持人
}

// Voices implements Provider.
func (m *MiniMaxProvider) Voices() []string { return minimaxSuggestedVoices }

// IsValidVoice implements Provider. Any non-empty id is accepted because
// MiniMax supports many system and cloned voices; invalid ids fail at the
// API call with a clear status message.
func (m *MiniMaxProvider) IsValidVoice(v string) bool { return v != "" }

type minimaxVoiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float64 `json:"speed"`
	Vol     float64 `json:"vol"`
	Pitch   int     `json:"pitch"`
}

type minimaxAudioSetting struct {
	SampleRate int    `json:"sample_rate"`
	Format     string `json:"format"`
	Channel    int    `json:"channel"`
}

type minimaxRequest struct {
	Model        string               `json:"model"`
	Text         string               `json:"text"`
	Stream       bool                 `json:"stream"`
	VoiceSetting minimaxVoiceSetting  `json:"voice_setting"`
	AudioSetting minimaxAudioSetting  `json:"audio_setting"`
}

type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

type minimaxResponse struct {
	BaseResp minimaxBaseResp `json:"base_resp"`
	Data     struct {
		// Audio is hex-encoded audio data.
		Audio string `json:"audio"`
	} `json:"data"`
	TraceID string `json:"trace_id"`
}

// SynthesizeAudio implements Provider. It requests raw PCM (16-bit signed
// little-endian mono) and returns it wrapped in a WAV container so that
// every platform player (including Windows Media.SoundPlayer) can play it.
func (m *MiniMaxProvider) SynthesizeAudio(text, voice string) (AudioResult, error) {
	if m.apiKey == "" {
		return AudioResult{}, fmt.Errorf("MINIMAX_API_KEY environment variable is required")
	}
	if voice == "" {
		voice = m.voice
	}

	reqBody := minimaxRequest{
		Model:  m.model,
		Text:   text,
		Stream: false,
		VoiceSetting: minimaxVoiceSetting{
			VoiceID: voice,
			Speed:   1.0,
			Vol:     1.0,
			Pitch:   0,
		},
		AudioSetting: minimaxAudioSetting{
			SampleRate: m.sampleRate,
			Format:     "pcm",
			Channel:    1,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return AudioResult{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", m.baseURL+"/v1/t2a_v2", bytes.NewReader(jsonData))
	if err != nil {
		return AudioResult{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return AudioResult{}, fmt.Errorf("minimax API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AudioResult{}, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return AudioResult{}, fmt.Errorf("minimax API error (status %d): %s", resp.StatusCode, string(body))
	}

	var out minimaxResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return AudioResult{}, fmt.Errorf("failed to decode minimax response: %w", err)
	}
	if out.BaseResp.StatusCode != 0 {
		return AudioResult{}, fmt.Errorf("minimax API error %d: %s (trace_id: %s)",
			out.BaseResp.StatusCode, out.BaseResp.StatusMsg, out.TraceID)
	}
	if out.Data.Audio == "" {
		return AudioResult{}, fmt.Errorf("minimax API returned no audio (trace_id: %s)", out.TraceID)
	}

	pcm, err := hex.DecodeString(out.Data.Audio)
	if err != nil {
		return AudioResult{}, fmt.Errorf("failed to decode hex audio payload: %w", err)
	}

	wav := wrapPCMInWAV(pcm, 1, 16, m.sampleRate)
	return AudioResult{Data: wav, Format: FormatWAV}, nil
}

// wrapPCMInWAV prepends a 44-byte RIFF/WAVE header to raw signed 16-bit
// little-endian PCM audio.
func wrapPCMInWAV(pcm []byte, channels, bitsPerSample, sampleRate int) []byte {
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign

	out := make([]byte, 44+len(pcm))
	copy(out[0:4], "RIFF")
	lePut32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	lePut32(out[16:20], 16)                    // fmt chunk size
	lePut16(out[20:22], 1)                     // PCM
	lePut16(out[22:24], uint16(channels))      // channels
	lePut32(out[24:28], uint32(sampleRate))    // sample rate
	lePut32(out[28:32], uint32(byteRate))      // byte rate
	lePut16(out[32:34], uint16(blockAlign))    // block align
	lePut16(out[34:36], uint16(bitsPerSample)) // bits per sample
	copy(out[36:40], "data")
	lePut32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

func lePut16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func lePut32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
