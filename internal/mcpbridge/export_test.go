package mcpbridge

import "github.com/modelcontextprotocol/go-sdk/mcp"

// CloseSessionForTest poisons the live session the way a proxy idle-close or a
// CM redeploy would, so a test can assert the next call reconnects instead of
// failing with "client is closing".
func (b *Bridge) CloseSessionForTest() {
	b.mu.Lock()
	defer b.mu.Unlock()

	_ = b.session.Close()
}

// SessionForTest returns the current live session pointer, letting a test assert
// whether a call swapped it (a reconnect) or left it in place.
func (b *Bridge) SessionForTest() *mcp.ClientSession {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.session
}
