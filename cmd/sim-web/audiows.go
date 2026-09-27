package main

// Minimal WebSocket server for streaming raw PCM audio from a single
// router port to a single browser. RFC 6455, server side only, binary
// frames only, no permessage-deflate, no fragmentation, no
// control-frame handling beyond ping → pong → close.
//
// We hand-roll this rather than pulling in gorilla/websocket because
// audio is the only WebSocket use case and the protocol is small
// enough that a single file is clearer than a vendored dependency.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
)

// wsGUID is the magic appended to Sec-WebSocket-Key for the handshake.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// handleAudio: GET /api/audio?port=<node>.<port>&side=tx|rx
// Upgrades to WebSocket and streams raw 48 kHz int16 LE PCM as one
// binary frame per audio block (960 bytes).
func (a *app) handleAudio(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	side := r.URL.Query().Get("side")
	if port == "" || (side != "tx" && side != "rx") {
		http.Error(w, "want ?port=<n>.<p>&side=tx|rx", http.StatusBadRequest)
		return
	}

	conn, br, err := wsUpgrade(w, r)
	if err != nil {
		a.logger.Debug("audio ws upgrade", "err", err)
		return
	}
	defer conn.Close()

	a.logger.Info("audio stream opened", "port", port, "side", side, "remote", conn.RemoteAddr())
	defer a.logger.Info("audio stream closed", "port", port, "side", side)

	key := port + "|" + side
	ch, cancel := a.audioTap.Subscribe(key)
	defer cancel()

	// One goroutine reads from the client (so we notice close / pings).
	// Anything we see is either a ping (we must pong) or a close. The
	// client never sends data on this socket.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		_ = wsReadDrain(conn, br)
	}()

	// Periodic ping to detect dead clients quickly.
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-clientGone:
			return
		case <-r.Context().Done():
			return
		case <-ping.C:
			if err := wsWritePing(conn); err != nil {
				return
			}
		case blk, ok := <-ch:
			if !ok {
				return
			}
			if err := wsWriteBinary(conn, blk); err != nil {
				return
			}
		}
	}
}

// wsUpgrade performs the RFC 6455 server handshake and returns the
// hijacked connection plus the bufio.Reader holding any leftover bytes
// (usually empty for a fresh upgrade).
func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, nil, errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, nil, errors.New("missing key")
	}
	accept := wsAcceptToken(key)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacker unsupported", http.StatusInternalServerError)
		return nil, nil, errors.New("no hijacker")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw.Reader, nil
}

func wsAcceptToken(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsWriteBinary writes a single FIN binary frame, unmasked (server →
// client). Payload sizes up to ~16 MB are supported.
func wsWriteBinary(conn net.Conn, payload []byte) error {
	return wsWriteFrame(conn, 0x2, payload)
}

func wsWritePing(conn net.Conn) error {
	return wsWriteFrame(conn, 0x9, nil)
}

func wsWriteFrame(conn net.Conn, opcode byte, payload []byte) error {
	hdr := make([]byte, 0, 10)
	// FIN=1, RSV=000, opcode in low nibble.
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		hdr = append(hdr, ext[:]...)
	default:
		hdr = append(hdr, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	if n > 0 {
		if _, err := conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// wsReadDrain consumes everything the client sends. The only client
// frames we expect are pings (we respond with pong) and a close. Any
// data frame is silently dropped on the floor. Returns when the
// client closes or the connection errors out.
func wsReadDrain(conn net.Conn, br *bufio.Reader) error {
	r := br
	for {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		op, payload, err := wsReadFrame(r)
		if err != nil {
			return err
		}
		switch op {
		case 0x8: // close
			_ = wsWriteFrame(conn, 0x8, nil)
			return io.EOF
		case 0x9: // ping
			_ = wsWriteFrame(conn, 0xA, payload)
		default:
			// data frame or pong — ignore
		}
	}
}

// wsReadFrame reads one frame and returns its opcode + unmasked
// payload. Server-side, every client frame MUST be masked (RFC 6455
// 5.1). Fragmentation (FIN=0) is not supported — we treat any frame
// as one whole message.
func wsReadFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0F
	masked := h[1]&0x80 != 0
	pl := int(h[1] & 0x7F)
	switch pl {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		pl = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		pl = int(binary.BigEndian.Uint64(ext[:]))
	}
	if pl > 64*1024 {
		return 0, nil, fmt.Errorf("client frame too large: %d", pl)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, pl)
	if pl > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return op, payload, nil
}

// Silence "unused" lint on audio import if the file ever ends up
// being built standalone.
var _ = audio.SampleRate
