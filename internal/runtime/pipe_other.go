//go:build !windows

package runtime

import (
	"context"
	"errors"
	"net"
)

func dialPipe(context.Context, string) (net.Conn, error) {
	return nil, errors.New("named pipes (npipe://) exist only on Windows")
}
