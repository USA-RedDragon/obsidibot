package cmd_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// A minimal Source RCON server, enough for startup discovery to succeed.
//
// It exists because the server GUID is no longer configurable: obsidibot reads
// it from the game with ServerInfo, so an end-to-end test of startup has to
// have a game to read it from. Answering the real protocol is also the only way
// to prove the RCON client is wired up at all.
//
// Wire format, all little-endian: size int32 (everything after it), id int32,
// type int32, NUL-terminated body, then one more NUL.
const (
	typeResponseValue int32 = 0
	typeExecCommand   int32 = 2
	typeAuthResponse  int32 = 2
	typeAuth          int32 = 3
)

const fakeServerGUID = "63a86971-0cb9-4569-a43a-4b05317f2d73"

type fakeRCON struct {
	listener net.Listener
	mu       sync.Mutex
	commands []string
}

// startFakeRCON listens on a free port and serves until the test ends.
func startFakeRCON(t *testing.T) *fakeRCON {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRCON{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed with the test
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeRCON) port() int { return f.listener.Addr().(*net.TCPAddr).Port } //nolint:forcetypeassert // always TCP

func (f *fakeRCON) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		id, typ, body, err := readPacket(conn)
		if err != nil {
			return
		}

		switch typ {
		case typeAuth:
			// Accept any password: this fake is not testing authentication.
			_ = writePacket(conn, id, typeAuthResponse, "")
		case typeExecCommand:
			if body == "" {
				// The client queues an empty command as a sentinel and treats
				// its answer as the end of the response.
				_ = writePacket(conn, id, typeResponseValue, "")
				continue
			}
			f.mu.Lock()
			f.commands = append(f.commands, body)
			f.mu.Unlock()
			_ = writePacket(conn, id, typeResponseValue, f.respond(body))
		}
	}
}

func (f *fakeRCON) respond(command string) string {
	fields := strings.Fields(command)
	switch {
	case strings.EqualFold(fields[0], "ServerInfo"):
		return "(ServerInfo): Server Name: Obsidian Wilds / UUID: " + fakeServerGUID +
			" / TimeOfDay: 1224 / Weather: ClearSky"
	case strings.EqualFold(fields[0], "Unban"):
		// The real server's wording. The ban scheduler only closes a record
		// once the game has confirmed the lift, so a generic "ok" here would
		// leave it retrying forever.
		return "(" + command + "): Unbanned player with Id '" + fields[1] + "'."
	}
	return "(" + command + "): ok"
}

// unbanned returns the identifiers this fake was asked to unban.
func (f *fakeRCON) unbanned() []string {
	var out []string
	for _, command := range f.issued() {
		fields := strings.Fields(command)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Unban") {
			out = append(out, fields[1])
		}
	}
	return out
}

func (f *fakeRCON) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func readPacket(r io.Reader) (id, typ int32, body string, err error) {
	var size int32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < 10 || size > 64<<10 {
		return 0, 0, "", errors.New("bad packet size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(buf[0:4]))  //nolint:gosec // protocol field
	typ = int32(binary.LittleEndian.Uint32(buf[4:8])) //nolint:gosec // protocol field
	return id, typ, string(buf[8 : len(buf)-2]), nil
}

func writePacket(w io.Writer, id, typ int32, body string) error {
	// The sign bit is part of the wire format here, not a magnitude: the
	// protocol's int32 fields are written as-is, and this fake only ever emits
	// small positive ids and bodies it constructed itself.
	//nolint:gosec // reinterpreting the bit pattern is the protocol
	size := uint32(len(body) + 10)
	buf := make([]byte, 0, size+4)
	buf = binary.LittleEndian.AppendUint32(buf, size)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))  //nolint:gosec // protocol field
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ)) //nolint:gosec // protocol field
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}
