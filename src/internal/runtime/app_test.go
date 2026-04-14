package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"purple-lightswitch/internal/bootstrap"
)

func TestAppServesIndexAndWebsocketHello(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New(Config{ListenHost: "127.0.0.1", Port: 0}, bootstrap.RuntimeAssets{}, nil)
	address, err := app.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = app.Shutdown(shutdownCtx)
	}()

	baseURL := address
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}

	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !strings.Contains(string(body), "Purple Lightswitch") {
		t.Fatalf("unexpected HTML body: %s", string(body))
	}

	wsURL := "ws://" + strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial websocket failed: %v", err)
	}
	defer conn.Close()

	var payload helloMessage
	if err := conn.ReadJSON(&payload); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if payload.Type != "hello" || payload.ClientID == "" {
		t.Fatalf("unexpected hello payload: %+v", payload)
	}
}

func TestBasicAuthBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New(Config{ListenHost: "127.0.0.1", Port: 0, Password: "secret"}, bootstrap.RuntimeAssets{}, nil)
	address, err := app.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = app.Shutdown(shutdownCtx)
	}()

	baseURL := address
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}

	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	parsed.User = url.UserPassword("user", "secret")
	resp, err = http.Get(parsed.String() + "/")
	if err != nil {
		t.Fatalf("authenticated GET / failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with auth, got %d", resp.StatusCode)
	}
}

func TestShutdownReturnsWithLiveWebsocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New(Config{ListenHost: "127.0.0.1", Port: 0}, bootstrap.RuntimeAssets{}, nil)
	address, err := app.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	wsURL := "ws://" + strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial websocket failed: %v", err)
	}
	defer conn.Close()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := app.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestExtractRenderProgress(t *testing.T) {
	tests := []struct {
		line    string
		current int
		total   int
		ok      bool
	}{
		{line: "sampling 1/36", current: 1, total: 36, ok: true},
		{line: "15/36", current: 15, total: 36, ok: true},
		{line: "step 48/36", current: 36, total: 36, ok: true},
		{line: "renderer warming up", ok: false},
		{line: "bad 2/0", ok: false},
	}

	for _, test := range tests {
		current, total, ok := extractRenderProgress(test.line)
		if ok != test.ok || current != test.current || total != test.total {
			t.Fatalf("extractRenderProgress(%q) = (%d, %d, %v), want (%d, %d, %v)",
				test.line, current, total, ok, test.current, test.total, test.ok)
		}
	}
}

func TestAdvanceRenderProgress(t *testing.T) {
	state, changed := advanceRenderProgress(renderProgressState{}, "ggml_extend.hpp:1862 - vae compute buffer size: 1361.52 MB(VRAM)")
	if !changed || state.Phase != phaseEncode || state.Percent != 0 {
		t.Fatalf("unexpected encode start state: %+v changed=%v", state, changed)
	}

	state, changed = advanceRenderProgress(state, "|========>                                         | 9/18 - 3.24s/it")
	if !changed || state.Phase != phaseEncode || state.Percent != 15 {
		t.Fatalf("unexpected encode progress state: %+v changed=%v", state, changed)
	}

	state, changed = advanceRenderProgress(state, "ggml_extend.hpp:1862 - z_image compute buffer size: 598.06 MB(VRAM)")
	if !changed || state.Phase != phaseBuffer || state.Percent != 30 {
		t.Fatalf("unexpected buffer start state: %+v changed=%v", state, changed)
	}

	state, changed = advanceRenderProgress(state, "|=========================>                        | 3/6 - 11.64s/it")
	if !changed || state.Phase != phaseBuffer || state.Percent != 40 {
		t.Fatalf("unexpected buffer progress state: %+v changed=%v", state, changed)
	}

	state, changed = advanceRenderProgress(state, "stable-diffusion.cpp:3180 - sampling completed, taking 70.00s")
	if !changed || state.Phase != phaseGenerate || state.Percent != 50 {
		t.Fatalf("unexpected generate start state: %+v changed=%v", state, changed)
	}

	state, changed = advanceRenderProgress(state, "|==========================>                       | 19/36 - 4.31s/it")
	if !changed || state.Phase != phaseGenerate || state.Percent != 76 {
		t.Fatalf("unexpected generate progress state: %+v changed=%v", state, changed)
	}
}
