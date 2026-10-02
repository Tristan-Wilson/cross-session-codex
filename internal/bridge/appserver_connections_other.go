//go:build !linux

package bridge

import "context"

func appServerConnections(ctx context.Context, pid int, socket string) (int, error) {
	return lsofAppServerConnections(ctx, pid, socket)
}
