package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type quotaBatchWebsocketExecutor struct {
	websocketDirectCaptureExecutor
	reset int64
}

func (e *quotaBatchWebsocketExecutor) HttpRequest(_ context.Context, a *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	reset := e.reset
	if a.ID == "quota-ws-b" {
		reset += 3600
	}
	body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%d}}}`, reset)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestQuotaBatchWebsocketReplaysAtBoundaryAndSelectsNextAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exec := &quotaBatchWebsocketExecutor{reset: time.Now().Add(4 * time.Hour).Unix()}
	manager := coreauth.NewManager(nil, coreauth.NewQuotaBatchSelector(""), nil)
	manager.RegisterExecutor(exec)
	model := "gpt-quota-batch-websocket"
	for _, id := range []string{"quota-ws-a", "quota-ws-b"} {
		_, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexResponseSteering: true}, manager)
	handler := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		return conn
	}
	conn := dial()
	full := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))
	if err := conn.WriteMessage(websocket.TextMessage, full); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	// A passive quota event crosses the 15-point batch boundary without waiting
	// for the next scheduled full probe.
	a, _ := manager.GetByID("quota-ws-a")
	a.Quota = coreauth.QuotaState{ObservedAt: time.Now().Add(time.Second), Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "15", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": fmt.Sprint(exec.reset),
	}}
	if _, err := manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	continuation := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"previous_response_id":"resp-1","input":[]}`, model))
	if err := conn.WriteMessage(websocket.TextMessage, continuation); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Text != wsHTTPReplayRequiredCloseReason {
		t.Fatalf("expected replay close, got %v", err)
	}
	exec.mu.Lock()
	count := len(exec.authIDs)
	exec.mu.Unlock()
	if count != 1 {
		t.Fatal("continuation was incorrectly sent upstream across batch boundary")
	}
	replay := dial()
	if err := replay.WriteMessage(websocket.TextMessage, full); err != nil {
		t.Fatal(err)
	}
	if _, _, err := replay.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.authIDs) != 2 || exec.authIDs[0] != "quota-ws-a" || exec.authIDs[1] != "quota-ws-b" {
		t.Fatalf("selected accounts: %v", exec.authIDs)
	}
}
