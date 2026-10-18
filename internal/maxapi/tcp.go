package maxapi

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Raw TCP transport — how the MAX desktop client talks, and what the Telemax
// bridge (github.com/Trollobot/Telemax, AGPL-3.0) uses to log in and run with
// no browser at all: plain TLS to api2.oneme.ru:443, the same 10-byte-header +
// msgpack frames as the binary WebSocket, but with protocol byte 11, the
// DESKTOP device type, and no web-edge TLS fingerprinting. Only the wire facts
// (host, framing, INIT payload) come from Telemax; the code is our own.

const (
	// TCPAddr is MAX's desktop-client endpoint.
	TCPAddr = "api2.oneme.ru:443"
	// tcpProtocolVersion is the header version byte the desktop client sends.
	tcpProtocolVersion = 11
	// tcpReadIdle bounds silence on the socket: we ping every 15s and MAX
	// answers, so 90s without a single frame means a half-open connection.
	tcpReadIdle = 90 * time.Second
)

// MAX's certificate (*.oneme.ru) chains through Russia's state CA, which no
// standard trust store carries. These two public certificates (Минцифры
// "Russian Trusted Root CA" + "Sub CA", see certs/README.md) are trusted ONLY
// for the MAX TCP connection — never installed system-wide, never used for
// Telegram or anything else.
//
//go:embed certs/russian_trusted_root_ca.crt
var russianRootCA []byte

//go:embed certs/russian_trusted_sub_ca.crt
var russianSubCA []byte

// maxTCPTLSConfig verifies against the system roots plus the Russian chain.
func maxTCPTLSConfig(serverName string) *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(russianRootCA)
	pool.AppendCertsFromPEM(russianSubCA)
	return &tls.Config{ServerName: serverName, RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// dialMaxTCP opens the verified TLS connection to MAX's desktop endpoint.
func dialMaxTCP(ctx context.Context) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(TCPAddr)
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
		Config:    maxTCPTLSConfig(host),
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return d.DialContext(ctx, "tcp", TCPAddr)
}

// readTCPRaw reads exactly one frame's bytes off the stream: the 10-byte
// header, then as many payload bytes as its 24-bit length says. An error here
// means the stream is broken (or desynced); decoding the bytes is separate.
func readTCPRaw(r *bufio.Reader) ([]byte, error) {
	var hdr [binHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != tcpProtocolVersion && hdr[0] != binProtocolVersion {
		return nil, fmt.Errorf("tcp frame: unexpected protocol byte 0x%02x (stream out of sync)", hdr[0])
	}
	n := int(hdr[7])<<16 | int(hdr[8])<<8 | int(hdr[9])
	buf := make([]byte, binHeaderLen+n)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(r, buf[binHeaderLen:]); err != nil {
		return nil, err
	}
	return buf, nil
}

// tcpUserAgent is what INIT presents over TCP: the MAX desktop client.
func tcpUserAgent() map[string]any {
	return map[string]any{
		"deviceType":  "DESKTOP",
		"appVersion":  "26.24.0",
		"osVersion":   "Windows 11",
		"locale":      "ru",
		"screen":      "2.0x",
		"timezone":    "Europe/Moscow",
		"buildNumber": 75261,
	}
}

var (
	zstdOnce sync.Once
	zstdDec  *zstd.Decoder
)

// zstdDecompress inflates a Zstandard frame payload (flag byte 0xFF), refusing
// to grow past limit bytes.
func zstdDecompress(src []byte, limit int) ([]byte, error) {
	zstdOnce.Do(func() {
		zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(uint64(maxBinPayload)))
	})
	if zstdDec == nil {
		return nil, fmt.Errorf("zstd decoder unavailable")
	}
	out, err := zstdDec.DecodeAll(src, nil)
	if err == nil && len(out) > limit {
		return nil, fmt.Errorf("decompressed payload too large (%d bytes)", len(out))
	}
	return out, err
}
