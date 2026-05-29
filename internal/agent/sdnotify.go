package agent

import (
	"net"
	"os"
)

// sdNotifyReady signals systemd that the service is ready, when the unit
// uses Type=notify. Reads NOTIFY_SOCKET from the env (set by systemd) and
// writes "READY=1\n" to it. No-op when the env var is unset (running
// outside systemd) so unit tests don't need to fake it.
//
// We inline the protocol rather than depend on github.com/coreos/go-systemd:
// it's a Unix datagram socket and four bytes of payload. Adding the
// transitive dependency surface isn't worth it.
func sdNotifyReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	addr := &net.UnixAddr{Name: sock, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		// systemd is responsible for cleaning up if we never report
		// ready; on this failure we just lose the up-signal. Quiet
		// failure is intentional — the agent should still serve
		// requests in test/dev environments where NOTIFY_SOCKET
		// happens to be set but unreachable.
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("READY=1\n"))
}
