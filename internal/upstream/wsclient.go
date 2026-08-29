// wsclient.go 纯标准库 RFC6455 WebSocket 客户端（最小实现）：
//   - 握手（Sec-WebSocket-Key / Accept）
//   - 客户端帧加掩码
//   - 文本/二进制/分片帧（控制帧自动应答 ping/pong）
//
// 仅依赖 crypto/tls、crypto/sha1、crypto/rand + base64，无第三方 ws 库。
package upstream

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ws 帧操作码。
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn 一条已建立连接的 WebSocket。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex // 发送锁
}

// wsDialOptions 建立连接的选项。
type wsDialOptions struct {
	Headers map[string]string // 额外握手头（例如 Authorization）
	Timeout time.Duration
}

// dialWS 建立到 ws:// 或 wss:// 的 WebSocket 连接。
func dialWS(wsURL string, opt wsDialOptions) (*wsConn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("bad ws url: %w", err)
	}
	if u.Scheme != "wss" && u.Scheme != "ws" {
		return nil, fmt.Errorf("unsupported ws scheme %q", u.Scheme)
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	host := u.Hostname()
	addr := net.JoinHostPort(host, port)

	var nc net.Conn
	dialer := &net.Dialer{Timeout: timeout}
	raw, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "wss" {
		tc := tls.Client(raw, &tls.Config{ServerName: host})
		if err := tc.Handshake(); err != nil {
			raw.Close()
			return nil, err
		}
		nc = tc
	} else {
		nc = raw
	}

	key, err := wsRandomKey()
	if err != nil {
		nc.Close()
		return nil, err
	}

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	fmt.Fprintf(&req, "Upgrade: websocket\r\n")
	fmt.Fprintf(&req, "Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	fmt.Fprintf(&req, "Sec-WebSocket-Version: 13\r\n")
	for k, v := range opt.Headers {
		fmt.Fprintf(&req, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(&req, "\r\n")

	_ = nc.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(nc, req.String()); err != nil {
		nc.Close()
		return nil, err
	}

	br := bufio.NewReader(nc)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ws handshake read: %w", err)
	}
	if !strings.Contains(statusLine, " 101 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 2048))
		nc.Close()
		return nil, fmt.Errorf("ws handshake failed: %s %s",
			strings.TrimSpace(statusLine), strings.TrimSpace(string(rest)))
	}
	accept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("read ws header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			accept = strings.TrimSpace(line[strings.Index(line, ":")+1:])
		}
	}
	if expect := wsAcceptFor(key); accept != expect {
		nc.Close()
		return nil, fmt.Errorf("ws accept mismatch: got %q want %q", accept, expect)
	}
	_ = nc.SetDeadline(time.Time{})
	return &wsConn{conn: nc, br: br}, nil
}

// wsAcceptFor 计算 Sec-WebSocket-Accept。
func wsAcceptFor(key string) string {
	h := sha1.New()
	h.Write([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsRandomKey 生成 16 字节随机 key 的 base64。
func wsRandomKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// WriteText 发送一条带掩码的文本帧（自动分片）。
func (c *wsConn) WriteText(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrames(wsOpText, payload)
}

// writeFrames 发送一（或多）条掩码文本帧。
func (c *wsConn) writeFrames(opcode byte, payload []byte) error {
	const maxPayload = 128 * 1024
	if len(payload) <= maxPayload {
		return c.writeFrameOnce(opcode, payload)
	}
	if err := c.writeFrameOnce(opcode, payload[:maxPayload]); err != nil {
		return err
	}
	for off := maxPayload; off < len(payload); off += maxPayload {
		end := off + maxPayload
		if end > len(payload) {
			end = len(payload)
		}
		if err := c.writeFrameOnce(wsOpContinuation, payload[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// writeFrameOnce 写一条未分片掩码帧。
func (c *wsConn) writeFrameOnce(opcode byte, payload []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|opcode) // FIN=1
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		sz := make([]byte, 8)
		binary.BigEndian.PutUint64(sz, uint64(n))
		hdr = append(hdr, sz...)
	}
	hdr = append(hdr, mask...)
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	_, err := c.conn.Write(masked)
	return err
}

// NextMessage 读取下一条完整文本消息（应答 ping/pong、跳过空、检测 close）。
func (c *wsConn) NextMessage() (opcode byte, payload []byte, err error) {
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			_ = c.writeFrameOnce(wsOpPong, data)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			return 0, nil, io.EOF
		case wsOpText, wsOpBinary:
			if fin {
				return op, data, nil
			}
			// 聚合分片
			all := append([]byte{}, data...)
			for {
				fin2, op2, d2, err := c.readFrame()
				if err != nil {
					return 0, nil, err
				}
				if op2 == wsOpPing {
					_ = c.writeFrameOnce(wsOpPong, d2)
					continue
				}
				if op2 == wsOpPong {
					continue
				}
				all = append(all, d2...)
				if fin2 {
					return op, all, nil
				}
			}
		default:
			continue
		}
	}
}

// readFrame 读一条原始帧。
func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var b [2]byte
	if _, err := io.ReadFull(c.br, b[:]); err != nil {
		return false, 0, nil, err
	}
	fin = b[0]&0x80 != 0
	op = b[0] & 0x0F
	length := uint64(b[1] & 0x7F)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 64*1024*1024 {
		return false, 0, nil, errors.New("ws frame too large")
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// Close 关闭连接。
func (c *wsConn) Close() error { return c.conn.Close() }

// wsWriteJSON 发送一条 JSON 文本帧。
func (c *wsConn) wsWriteJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteText(raw)
}

// nopNetConn 一个什么都不做的 net.Conn，用于 RAML override 场景的 noop WS。
type nopNetConn struct{}

func (nopNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopNetConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopNetConn) Close() error                     { return nil }
func (nopNetConn) LocalAddr() net.Addr              { return nil }
func (nopNetConn) RemoteAddr() net.Addr             { return nil }
func (nopNetConn) SetDeadline(time.Time) error      { return nil }
func (nopNetConn) SetReadDeadline(time.Time) error  { return nil }
func (nopNetConn) SetWriteDeadline(time.Time) error { return nil }
