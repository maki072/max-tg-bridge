package maxapi

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Binary transport (protocol ver=10) — what web.max.ru itself speaks over the
// same WebSocket endpoint. Confirmed from live DevTools captures: on the JSON
// (ver=11) transport the server replaces polls with a bare
// {"_type":"UNSUPPORTED"} attach, while over the binary one the very same
// message carries a full POLL attach (question, answers, results).
//
// Frame layout (big-endian):
//
//	ver u8 | cmd u8 | seq u16 | opcode u16 | flags u8 | payload length u24 | payload
//
// The payload is msgpack; a non-zero flags byte means it is LZ4
// block-compressed (seen live: 2 and 4). Large ids arrive wrapped in msgpack
// ext type 1, whose data is itself a msgpack-encoded integer.
//
// To keep the rest of the client untouched, binary frames are converted
// to/from the same Frame{Payload json.RawMessage} the JSON transport uses.

const binProtocolVersion = 10

// BinaryWSURL is where web.max.ru opens its binary socket (from DevTools:
// Request URL). It is NOT the JSON endpoint (ws-api.oneme.ru) — that one
// drops any binary frame with a 1006 close; this one answers binary INIT and
// PING (verified live with a throwaway deviceId).
const BinaryWSURL = "wss://api.oneme.ru/websocket"

const binHeaderLen = 10

// maxBinPayload bounds a decompressed payload (guards against LZ4 bombs).
const maxBinPayload = 64 << 20

// encodeBinFrame renders f (JSON payload) as a binary ver=10 frame with an
// uncompressed msgpack payload.
func encodeBinFrame(f Frame) ([]byte, error) { return encodeBinFrameVer(f, binProtocolVersion) }

// encodeBinFrameVer is encodeBinFrame with an explicit header version byte
// (10 on the web WebSocket, 11 on the desktop TCP transport). A frame with no
// payload at all (a bare PING) carries an empty body, as the desktop client
// sends it.
func encodeBinFrameVer(f Frame, ver byte) ([]byte, error) {
	var body []byte
	if len(f.Payload) > 0 {
		var v any
		dec := json.NewDecoder(strings.NewReader(string(f.Payload)))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("binary frame: payload is not JSON: %w", err)
		}
		var err error
		if body, err = msgpackEncode(nil, v); err != nil {
			return nil, err
		}
	}
	if len(body) > 0xFFFFFF {
		return nil, fmt.Errorf("binary frame: payload too large (%d bytes)", len(body))
	}
	out := make([]byte, binHeaderLen, binHeaderLen+len(body))
	out[0] = ver
	out[1] = byte(f.Cmd)
	binary.BigEndian.PutUint16(out[2:], uint16(f.Seq))
	binary.BigEndian.PutUint16(out[4:], uint16(f.Opcode))
	binary.BigEndian.PutUint32(out[6:], uint32(len(body))) // flags byte 0 = uncompressed
	return append(out, body...), nil
}

// decodeBinFrame parses a binary frame into a Frame with a JSON payload.
func decodeBinFrame(data []byte) (Frame, error) {
	if len(data) < binHeaderLen {
		return Frame{}, fmt.Errorf("binary frame: short header (%d bytes)", len(data))
	}
	f := Frame{
		Ver:    int(data[0]),
		Cmd:    int(data[1]),
		Seq:    int(binary.BigEndian.Uint16(data[2:])),
		Opcode: int(binary.BigEndian.Uint16(data[4:])),
	}
	flags := data[6]
	n := int(data[7])<<16 | int(data[8])<<8 | int(data[9])
	body := data[binHeaderLen:]
	if n > len(body) {
		return f, fmt.Errorf("binary frame: payload length %d exceeds frame (%d)", n, len(body))
	}
	body = body[:n]
	switch {
	case flags == 0:
	case flags == 0xFF: // Zstandard (desktop transport)
		var err error
		if body, err = zstdDecompress(body, maxBinPayload); err != nil {
			return f, fmt.Errorf("binary frame (opcode %d): zstd: %w", f.Opcode, err)
		}
	default: // LZ4 block
		var err error
		if body, err = lz4BlockDecompress(body, maxBinPayload); err != nil {
			return f, fmt.Errorf("binary frame (opcode %d): %w", f.Opcode, err)
		}
	}
	if len(body) == 0 {
		f.Payload = json.RawMessage("null")
		return f, nil
	}
	v, rest, err := msgpackDecode(body)
	if err != nil {
		return f, fmt.Errorf("binary frame (opcode %d): %w", f.Opcode, err)
	}
	// Some responses concatenate several top-level msgpack values; the answer is
	// the first object among them (falling back to the first value).
	if _, isMap := v.(map[string]any); !isMap {
		for len(rest) > 0 {
			var next any
			if next, rest, err = msgpackDecode(rest); err != nil {
				break
			}
			if _, ok := next.(map[string]any); ok {
				v = next
				break
			}
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return f, fmt.Errorf("binary frame (opcode %d): %w", f.Opcode, err)
	}
	f.Payload = raw
	return f, nil
}

// lz4BlockDecompress decodes a raw LZ4 block (no frame header).
func lz4BlockDecompress(src []byte, limit int) ([]byte, error) {
	dst := make([]byte, 0, len(src)*4)
	errCorrupt := errors.New("lz4: corrupt block")
	for i := 0; i < len(src); {
		tok := src[i]
		i++
		lit := int(tok >> 4)
		if lit == 15 {
			for {
				if i >= len(src) {
					return nil, errCorrupt
				}
				b := src[i]
				i++
				lit += int(b)
				if b != 255 {
					break
				}
			}
		}
		if i+lit > len(src) || len(dst)+lit > limit {
			return nil, errCorrupt
		}
		dst = append(dst, src[i:i+lit]...)
		i += lit
		if i == len(src) {
			break // the last sequence carries literals only
		}
		if i+2 > len(src) {
			return nil, errCorrupt
		}
		off := int(src[i]) | int(src[i+1])<<8
		i += 2
		if off == 0 || off > len(dst) {
			return nil, errCorrupt
		}
		ml := int(tok & 15)
		if ml == 15 {
			for {
				if i >= len(src) {
					return nil, errCorrupt
				}
				b := src[i]
				i++
				ml += int(b)
				if b != 255 {
					break
				}
			}
		}
		ml += 4
		if len(dst)+ml > limit {
			return nil, errCorrupt
		}
		start := len(dst) - off
		for k := 0; k < ml; k++ { // byte-by-byte: matches may overlap their own output
			dst = append(dst, dst[start+k])
		}
	}
	return dst, nil
}

var errMsgpackShort = errors.New("msgpack: unexpected end of data")

// msgpackDecode decodes one msgpack value into JSON-friendly Go values:
// map[string]any, []any, string, int64, uint64, float64, bool, nil, []byte.
func msgpackDecode(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, b, errMsgpackShort
	}
	c := b[0]
	b = b[1:]
	switch {
	case c <= 0x7f:
		return int64(c), b, nil
	case c >= 0xe0:
		return int64(int8(c)), b, nil
	case c >= 0x80 && c <= 0x8f:
		return msgpackMap(b, int(c&0x0f))
	case c >= 0x90 && c <= 0x9f:
		return msgpackArray(b, int(c&0x0f))
	case c >= 0xa0 && c <= 0xbf:
		return msgpackStr(b, int(c&0x1f))
	}
	switch c {
	case 0xc0:
		return nil, b, nil
	case 0xc2:
		return false, b, nil
	case 0xc3:
		return true, b, nil
	case 0xc4, 0xc5, 0xc6: // bin 8/16/32
		n, rest, err := msgpackLen(b, c-0xc4)
		if err != nil {
			return nil, b, err
		}
		if len(rest) < n {
			return nil, b, errMsgpackShort
		}
		return append([]byte(nil), rest[:n]...), rest[n:], nil
	case 0xc7, 0xc8, 0xc9: // ext 8/16/32
		n, rest, err := msgpackLen(b, c-0xc7)
		if err != nil {
			return nil, b, err
		}
		if len(rest) < n+1 {
			return nil, b, errMsgpackShort
		}
		return msgpackExt(int8(rest[0]), rest[1:1+n], rest[1+n:])
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8: // fixext 1/2/4/8/16
		n := 1 << (c - 0xd4)
		if len(b) < n+1 {
			return nil, b, errMsgpackShort
		}
		return msgpackExt(int8(b[0]), b[1:1+n], b[1+n:])
	case 0xca:
		if len(b) < 4 {
			return nil, b, errMsgpackShort
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), b[4:], nil
	case 0xcb:
		if len(b) < 8 {
			return nil, b, errMsgpackShort
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case 0xcc:
		if len(b) < 1 {
			return nil, b, errMsgpackShort
		}
		return int64(b[0]), b[1:], nil
	case 0xcd:
		if len(b) < 2 {
			return nil, b, errMsgpackShort
		}
		return int64(binary.BigEndian.Uint16(b)), b[2:], nil
	case 0xce:
		if len(b) < 4 {
			return nil, b, errMsgpackShort
		}
		return int64(binary.BigEndian.Uint32(b)), b[4:], nil
	case 0xcf:
		if len(b) < 8 {
			return nil, b, errMsgpackShort
		}
		u := binary.BigEndian.Uint64(b)
		if u <= math.MaxInt64 {
			return int64(u), b[8:], nil
		}
		return u, b[8:], nil
	case 0xd0:
		if len(b) < 1 {
			return nil, b, errMsgpackShort
		}
		return int64(int8(b[0])), b[1:], nil
	case 0xd1:
		if len(b) < 2 {
			return nil, b, errMsgpackShort
		}
		return int64(int16(binary.BigEndian.Uint16(b))), b[2:], nil
	case 0xd2:
		if len(b) < 4 {
			return nil, b, errMsgpackShort
		}
		return int64(int32(binary.BigEndian.Uint32(b))), b[4:], nil
	case 0xd3:
		if len(b) < 8 {
			return nil, b, errMsgpackShort
		}
		return int64(binary.BigEndian.Uint64(b)), b[8:], nil
	case 0xd9, 0xda, 0xdb: // str 8/16/32
		n, rest, err := msgpackLen(b, c-0xd9)
		if err != nil {
			return nil, b, err
		}
		return msgpackStr(rest, n)
	case 0xdc, 0xdd: // array 16/32
		n, rest, err := msgpackLen(b, c-0xdc+1)
		if err != nil {
			return nil, b, err
		}
		return msgpackArray(rest, n)
	case 0xde, 0xdf: // map 16/32
		n, rest, err := msgpackLen(b, c-0xde+1)
		if err != nil {
			return nil, b, err
		}
		return msgpackMap(rest, n)
	}
	return nil, b, fmt.Errorf("msgpack: unsupported type byte 0x%02x", c)
}

// msgpackLen reads a 1/2/4-byte big-endian length (width selector 0/1/2).
func msgpackLen(b []byte, width byte) (int, []byte, error) {
	switch width {
	case 0:
		if len(b) < 1 {
			return 0, b, errMsgpackShort
		}
		return int(b[0]), b[1:], nil
	case 1:
		if len(b) < 2 {
			return 0, b, errMsgpackShort
		}
		return int(binary.BigEndian.Uint16(b)), b[2:], nil
	default:
		if len(b) < 4 {
			return 0, b, errMsgpackShort
		}
		n := binary.BigEndian.Uint32(b)
		if n > maxBinPayload {
			return 0, b, fmt.Errorf("msgpack: length %d too large", n)
		}
		return int(n), b[4:], nil
	}
}

func msgpackStr(b []byte, n int) (any, []byte, error) {
	if len(b) < n {
		return nil, b, errMsgpackShort
	}
	return string(b[:n]), b[n:], nil
}

func msgpackArray(b []byte, n int) (any, []byte, error) {
	if n > len(b) { // every element takes at least one byte
		return nil, b, errMsgpackShort
	}
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		v, rest, err := msgpackDecode(b)
		if err != nil {
			return nil, b, err
		}
		out = append(out, v)
		b = rest
	}
	return out, b, nil
}

func msgpackMap(b []byte, n int) (any, []byte, error) {
	if 2*n > len(b) {
		return nil, b, errMsgpackShort
	}
	out := make(map[string]any, n)
	for i := 0; i < n; i++ {
		k, rest, err := msgpackDecode(b)
		if err != nil {
			return nil, b, err
		}
		v, rest, err := msgpackDecode(rest)
		if err != nil {
			return nil, b, err
		}
		key, ok := k.(string)
		if !ok {
			key = fmt.Sprint(k)
		}
		out[key] = v
		b = rest
	}
	return out, b, nil
}

// msgpackExt unwraps ext type 1 (a nested msgpack value — MAX uses it for
// 64-bit ids); any other ext type is passed through as its raw bytes.
func msgpackExt(typ int8, data, rest []byte) (any, []byte, error) {
	if typ == 1 {
		v, tail, err := msgpackDecode(data)
		if err != nil {
			return nil, rest, err
		}
		if len(tail) != 0 {
			return nil, rest, fmt.Errorf("msgpack: %d trailing bytes in ext 1", len(tail))
		}
		return v, rest, nil
	}
	return append([]byte(nil), data...), rest, nil
}

// msgpackEncode appends v (as produced by a json.Decoder with UseNumber) to
// dst. Map keys are sorted so encoding is deterministic.
func msgpackEncode(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, 0xc0), nil
	case bool:
		if x {
			return append(dst, 0xc3), nil
		}
		return append(dst, 0xc2), nil
	case string:
		return msgpackAppendStr(dst, x), nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return msgpackAppendInt(dst, i), nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("msgpack: bad number %q", x)
		}
		dst = append(dst, 0xcb)
		return binary.BigEndian.AppendUint64(dst, math.Float64bits(f)), nil
	case []any:
		n := len(x)
		switch {
		case n <= 15:
			dst = append(dst, 0x90|byte(n))
		case n <= math.MaxUint16:
			dst = binary.BigEndian.AppendUint16(append(dst, 0xdc), uint16(n))
		default:
			dst = binary.BigEndian.AppendUint32(append(dst, 0xdd), uint32(n))
		}
		for _, e := range x {
			var err error
			if dst, err = msgpackEncode(dst, e); err != nil {
				return nil, err
			}
		}
		return dst, nil
	case map[string]any:
		n := len(x)
		switch {
		case n <= 15:
			dst = append(dst, 0x80|byte(n))
		case n <= math.MaxUint16:
			dst = binary.BigEndian.AppendUint16(append(dst, 0xde), uint16(n))
		default:
			dst = binary.BigEndian.AppendUint32(append(dst, 0xdf), uint32(n))
		}
		keys := make([]string, 0, n)
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dst = msgpackAppendStr(dst, k)
			var err error
			if dst, err = msgpackEncode(dst, x[k]); err != nil {
				return nil, err
			}
		}
		return dst, nil
	}
	return nil, fmt.Errorf("msgpack: unsupported value type %T", v)
}

func msgpackAppendStr(dst []byte, s string) []byte {
	n := len(s)
	switch {
	case n <= 31:
		dst = append(dst, 0xa0|byte(n))
	case n <= math.MaxUint8:
		dst = append(dst, 0xd9, byte(n))
	case n <= math.MaxUint16:
		dst = binary.BigEndian.AppendUint16(append(dst, 0xda), uint16(n))
	default:
		dst = binary.BigEndian.AppendUint32(append(dst, 0xdb), uint32(n))
	}
	return append(dst, s...)
}

func msgpackAppendInt(dst []byte, i int64) []byte {
	switch {
	case i >= 0 && i <= 0x7f:
		return append(dst, byte(i))
	case i < 0 && i >= -32:
		return append(dst, byte(int8(i)))
	case i >= math.MinInt8 && i <= math.MaxInt8:
		return append(dst, 0xd0, byte(int8(i)))
	case i >= math.MinInt16 && i <= math.MaxInt16:
		return binary.BigEndian.AppendUint16(append(dst, 0xd1), uint16(int16(i)))
	case i >= math.MinInt32 && i <= math.MaxInt32:
		return binary.BigEndian.AppendUint32(append(dst, 0xd2), uint32(int32(i)))
	}
	return binary.BigEndian.AppendUint64(append(dst, 0xd3), uint64(i))
}
