package tts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiniMaxDefaults(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "test-key")

	p := NewMiniMaxProvider()
	if p.Name() != "minimax" {
		t.Errorf("expected name minimax, got %s", p.Name())
	}
	if p.baseURL != minimaxDefaultBaseURL {
		t.Errorf("expected default base URL %s, got %s", minimaxDefaultBaseURL, p.baseURL)
	}
	if p.model != minimaxDefaultModel {
		t.Errorf("expected default model %s, got %s", minimaxDefaultModel, p.model)
	}
	if p.DefaultVoice() != minimaxDefaultVoice {
		t.Errorf("expected default voice %s, got %s", minimaxDefaultVoice, p.DefaultVoice())
	}
	if p.sampleRate != minimaxDefaultSampleRate {
		t.Errorf("expected default sample rate %d, got %d", minimaxDefaultSampleRate, p.sampleRate)
	}
}

func TestMiniMaxEnvOverrides(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "k")
	t.Setenv("MINIMAX_BASE_URL", "https://api.minimax.cn")
	t.Setenv("MINIMAX_MODEL", "speech-02-turbo")
	t.Setenv("MINIMAX_VOICE", "presenter_male")
	t.Setenv("MINIMAX_SAMPLE_RATE", "24000")

	p := NewMiniMaxProvider()
	if p.baseURL != "https://api.minimax.cn" {
		t.Errorf("expected base URL override, got %s", p.baseURL)
	}
	if p.model != "speech-02-turbo" {
		t.Errorf("expected model override, got %s", p.model)
	}
	if p.DefaultVoice() != "presenter_male" {
		t.Errorf("expected voice override, got %s", p.DefaultVoice())
	}
	if p.sampleRate != 24000 {
		t.Errorf("expected sample rate 24000, got %d", p.sampleRate)
	}
}

func TestMiniMaxIsValidVoice(t *testing.T) {
	p := NewMiniMaxProvider()
	if !p.IsValidVoice("female-shaonv") {
		t.Error("system voice should be valid")
	}
	if !p.IsValidVoice("my-cloned-voice-123") {
		t.Error("cloned voice ids should be passed through to the API")
	}
	if p.IsValidVoice("") {
		t.Error("empty voice should be invalid")
	}
}

func TestMiniMaxSynthesize_Success(t *testing.T) {
	// 100 bytes of PCM (50 samples), hex-encoded in data.audio
	pcm := make([]byte, 100)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	hexAudio := make([]byte, 0, len(pcm)*2)
	const hexDigits = "0123456789abcdef"
	for _, b := range pcm {
		hexAudio = append(hexAudio, hexDigits[b>>4], hexDigits[b&0x0F])
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/t2a_v2") {
			t.Errorf("expected /v1/t2a_v2 path, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer auth, got %s", r.Header.Get("Authorization"))
		}

		var req minimaxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Model != "speech-02-hd" {
			t.Errorf("expected model speech-02-hd, got %s", req.Model)
		}
		if req.Text != "你好" {
			t.Errorf("expected text 你好, got %s", req.Text)
		}
		if req.VoiceSetting.VoiceID != "female-shaonv" {
			t.Errorf("expected voice female-shaonv, got %s", req.VoiceSetting.VoiceID)
		}
		if req.AudioSetting.Format != "pcm" {
			t.Errorf("expected pcm format, got %s", req.AudioSetting.Format)
		}
		if req.Stream {
			t.Error("expected stream=false")
		}

		resp := minimaxResponse{}
		resp.BaseResp.StatusCode = 0
		resp.BaseResp.StatusMsg = "success"
		resp.Data.Audio = string(hexAudio)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := &MiniMaxProvider{
		apiKey:     "test-key",
		baseURL:    server.URL,
		model:      "speech-02-hd",
		voice:      "female-shaonv",
		sampleRate: 32000,
		httpClient: server.Client(),
	}

	result, err := p.SynthesizeAudio("你好", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Format != FormatWAV {
		t.Errorf("expected wav format, got %s", result.Format)
	}

	data := result.Data
	if len(data) != 144 { // 44-byte header + 100 PCM bytes
		t.Fatalf("expected 144 bytes, got %d", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Error("expected RIFF/WAVE header")
	}
	if data[44] != 0 || data[45] != 1 { // first PCM bytes 0x00, 0x01
		t.Errorf("PCM payload mismatch: %x %x", data[44], data[45])
	}
	// Sample rate field at offset 24 must be little-endian 32000 = 0x00007D00
	if data[24] != 0x00 || data[25] != 0x7D || data[26] != 0x00 || data[27] != 0x00 {
		t.Error("expected sample rate 32000 in WAV header")
	}
}

func TestMiniMaxSynthesize_MissingKey(t *testing.T) {
	p := &MiniMaxProvider{httpClient: &http.Client{}}
	if _, err := p.SynthesizeAudio("hi", ""); err == nil {
		t.Fatal("expected error for missing MINIMAX_API_KEY")
	}
}

func TestMiniMaxSynthesize_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":2049,"status_msg":"invalid api key"},"trace_id":"t1"}`))
	}))
	defer server.Close()

	p := &MiniMaxProvider{
		apiKey:     "bad",
		baseURL:    server.URL,
		voice:      "female-shaonv",
		sampleRate: 32000,
		httpClient: server.Client(),
	}

	_, err := p.SynthesizeAudio("hi", "")
	if err == nil {
		t.Fatal("expected API error")
	}
	if !strings.Contains(err.Error(), "2049") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error should surface status code and message, got: %v", err)
	}
}

func TestMiniMaxSynthesize_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1001,"status_msg":"unauthorized"}}`))
	}))
	defer server.Close()

	p := &MiniMaxProvider{
		apiKey:     "bad",
		baseURL:    server.URL,
		voice:      "female-shaonv",
		sampleRate: 32000,
		httpClient: server.Client(),
	}

	if _, err := p.SynthesizeAudio("hi", ""); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestWrapPCMInWAV(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	wav := wrapPCMInWAV(pcm, 1, 16, 24000)

	if len(wav) != 48 {
		t.Fatalf("expected 48 bytes, got %d", len(wav))
	}
	// 24000 decimal = 0x5DC0, little-endian bytes C0 5D.
	if wav[24] != 0xC0 || wav[25] != 0x5D {
		t.Errorf("expected LE sample rate 24000, got %x %x", wav[24], wav[25])
	}
	if wav[34] != 16 {
		t.Errorf("expected 16 bits per sample, got %d", wav[34])
	}
	if string(wav[44:]) != string(pcm) {
		t.Error("PCM payload should follow header unchanged")
	}
}

func TestNewProviderSelection(t *testing.T) {
	t.Run("explicit minimax", func(t *testing.T) {
		t.Setenv("TTS_PROVIDER", "minimax")
		p, err := NewProvider()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name() != "minimax" {
			t.Errorf("expected minimax, got %s", p.Name())
		}
	})

	t.Run("explicit openai", func(t *testing.T) {
		t.Setenv("TTS_PROVIDER", "openai")
		p, err := NewProvider()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name() != "openai" {
			t.Errorf("expected openai, got %s", p.Name())
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		t.Setenv("TTS_PROVIDER", "azure")
		if _, err := NewProvider(); err == nil {
			t.Error("expected error for unknown provider")
		}
	})

	t.Run("auto-detect minimax when only its key is set", func(t *testing.T) {
		t.Setenv("TTS_PROVIDER", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("MINIMAX_API_KEY", "k")
		p, err := NewProvider()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name() != "minimax" {
			t.Errorf("expected minimax, got %s", p.Name())
		}
	})

	t.Run("defaults to openai when no keys", func(t *testing.T) {
		t.Setenv("TTS_PROVIDER", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("MINIMAX_API_KEY", "")
		p, err := NewProvider()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Name() != "openai" {
			t.Errorf("expected openai default, got %s", p.Name())
		}
	})
}
