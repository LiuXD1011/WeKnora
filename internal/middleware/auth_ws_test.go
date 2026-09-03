package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestBearerTokenFromWebSocketSubProtocol covers the browser WebSocket bridge
// in bearerToken: a browser cannot set the Authorization header on a
// WebSocket handshake, so the sandbox workbench terminal sends its JWT as a
// "bearer.<token>" sub-protocol and the middleware promotes it.
func TestBearerTokenFromWebSocketSubProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/api/v1/sessions/s-1/sandbox/terminal/ws", nil)
		return c
	}

	// Ordinary header auth is untouched by the bridge.
	c := newContext()
	c.Request.Header.Set("Authorization", "Bearer header-token")
	token, ok := bearerToken(c)
	require.True(t, ok)
	require.Equal(t, "header-token", token)
	require.Empty(t, c.Request.Header.Get("Sec-Websocket-Protocol"))

	// A single bearer sub-protocol is promoted and consumed.
	c = newContext()
	c.Request.Header.Set("Sec-WebSocket-Protocol", "bearer.ws-token")
	token, ok = bearerToken(c)
	require.True(t, ok)
	require.Equal(t, "ws-token", token)
	// The token moved into the regular header for the rest of the chain.
	require.Equal(t, "Bearer ws-token", c.Request.Header.Get("Authorization"))
	// ...and never echoes back on the response or lingers on the request.
	require.Empty(t, c.Request.Header.Get("Sec-Websocket-Protocol"))

	// Multiple comma-separated sub-protocols still find the token.
	c = newContext()
	c.Request.Header.Set("Sec-WebSocket-Protocol", "chat.example.org, bearer.multi-token")
	token, ok = bearerToken(c)
	require.True(t, ok)
	require.Equal(t, "multi-token", token)

	// A sub-protocol list without credentials is ignored.
	c = newContext()
	c.Request.Header.Set("Sec-WebSocket-Protocol", "chat.example.org")
	_, ok = bearerToken(c)
	require.False(t, ok)

	// No credentials at all stays that way.
	c = newContext()
	_, ok = bearerToken(c)
	require.False(t, ok)
}
