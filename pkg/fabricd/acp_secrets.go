package fabricd

import (
	"bytes"
	"io"

	"github.com/aiomni/dune/internal/wire"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (a *acpController) redactCredential(data []byte) []byte {
	if secret := a.mcpSecret.Load(); secret != nil {
		return bytes.ReplaceAll(data, []byte(*secret), []byte("[redacted]"))
	}
	return data
}

// Stderr is a byte stream: an OS read can split a credential at any position.
// Retain only a possible token prefix between reads, rather than whole log lines.
func (a *acpController) readStderr(reader io.Reader) {
	buffer := make([]byte, wire.ChunkSize)
	var pending []byte
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			data := a.redactCredential(append(pending, buffer[:n]...))
			keep := 0
			if secret := a.mcpSecret.Load(); secret != nil {
				for size := min(len(data), len(*secret)-1); size > 0; size-- {
					if bytes.HasSuffix(data, []byte((*secret)[:size])) {
						keep = size
						break
					}
				}
			}
			cut := len(data) - keep
			if cut > 0 {
				a.r.emit(&pb.Message{Kind: "stderr", Data: append([]byte(nil), data[:cut]...)})
			}
			pending = append([]byte(nil), data[cut:]...)
		}
		if err != nil {
			if len(pending) > 0 {
				a.r.emit(&pb.Message{Kind: "stderr", Data: []byte("[redacted]")})
			}
			return
		}
	}
}
