package chrome

import (
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewSessionForTest wraps a client session that talks to a server in this process.
func NewSessionForTest(cs *mcp.ClientSession, opts Options) *Session { return newSession(cs, opts) }

// Isolate and KillTree expose the process helpers.
func Isolate(cmd *exec.Cmd)  { isolate(cmd) }
func KillTree(cmd *exec.Cmd) { killTree(cmd) }

// TailBuffer exposes the buffer that keeps the end of the server's output.
type TailBuffer = tailBuffer

// NewTailBuffer makes one that keeps max bytes.
func NewTailBuffer(max int) *TailBuffer { return &tailBuffer{max: max} }

// OutputOf exposes what an error says of the output the server left behind.
func OutputOf(t *TailBuffer) string { return outputOf(t) }
