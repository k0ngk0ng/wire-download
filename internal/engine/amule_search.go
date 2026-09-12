package engine

// This file implements the small part of aMule's External Connections (EC)
// protocol needed for native searches.  amulecmd cannot be used here: its
// search result numbering is kept in the text client's process and the text
// output does not contain the MD4 hash required to enqueue a result.

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/k0ngk0ng/wire-download/internal/search"
)

const (
	// EC_CURRENT_PROTOCOL_VERSION from aMule 2.3.3's ECCodes.h.
	ecProtocolVersion uint64 = 0x0204

	// EC frames always carry the 0x20 protocol marker.  The native client
	// intentionally advertises neither optional capability: this keeps all
	// numbers in network byte order and makes zlib negotiation explicit.  A
	// frame carrying either optional flag is rejected because it was not
	// negotiated by this client.
	ecFrameFlags uint32 = 0x20
	ecFlagZlib   uint32 = 0x01
	ecFlagUTF8   uint32 = 0x02

	ecMaxFramePayload = 8 << 20
	ecMaxTagCount     = 4096
	ecMaxTagDepth     = 16
	ecMaxStringBytes  = 1 << 20

	ecSearchPollInterval = 500 * time.Millisecond
	// A completed global server search can report 100 before the first
	// server's result queue has been populated. Keep polling until the stage
	// deadline, reserving this interval to stop the search and drain the EC
	// response cleanly.
	ecSearchGlobalCleanupReserve = 1 * time.Second
	ecSearchCancelWindow         = 500 * time.Millisecond
	ecIODeadlineWindow           = 500 * time.Millisecond
)

const (
	ecOpAuthReq           byte = 0x02
	ecOpAuthFail          byte = 0x03
	ecOpAuthOK            byte = 0x04
	ecOpStrings           byte = 0x06
	ecOpMiscData          byte = 0x07
	ecOpSearchStart       byte = 0x26
	ecOpSearchStop        byte = 0x27
	ecOpSearchResults     byte = 0x28
	ecOpSearchProgress    byte = 0x29
	ecOpDownloadSearchRes byte = 0x2a
	ecOpAuthSalt          byte = 0x4f
	ecOpAuthPassword      byte = 0x50
	ecOpGetConnState      byte = 0x0b
)

const (
	ecTagString         uint16 = 0x0000
	ecTagPasswordHash   uint16 = 0x0001
	ecTagProtocol       uint16 = 0x0002
	ecTagClientName     uint16 = 0x0100
	ecTagClientVersion  uint16 = 0x0101
	ecTagPasswordSalt   uint16 = 0x000b
	ecTagConnState      uint16 = 0x0005
	ecTagSearchFile     uint16 = 0x0700
	ecTagSearchType     uint16 = 0x0701
	ecTagSearchName     uint16 = 0x0702
	ecTagSearchFileType uint16 = 0x0705
	ecTagSearchStatus   uint16 = 0x0708
	ecTagSearchParent   uint16 = 0x0709

	ecTagPartFileName       uint16 = 0x0301
	ecTagPartFileSizeFull   uint16 = 0x0303
	ecTagPartFileStatus     uint16 = 0x0308
	ecTagPartFileSourceCnt  uint16 = 0x030a
	ecTagPartFileSourceXfer uint16 = 0x030d
	ecTagPartFileHash       uint16 = 0x031e
)

const (
	ecSearchLocal  uint64 = 0
	ecSearchGlobal uint64 = 1
	ecSearchKad    uint64 = 2
)

const (
	ecConnStateED2KConnected uint64 = 0x01
	ecConnStateKadConnected  uint64 = 0x04
	ecConnStateKadRunning    uint64 = 0x10
)

type ecTag struct {
	name     uint16
	typ      byte
	data     []byte
	children []ecTag
}

type ecPacket struct {
	op   byte
	tags []ecTag
}

func ecEmptyTag(name uint16) ecTag {
	return ecTag{name: name}
}

func ecUintTag(name uint16, value uint64) ecTag {
	switch {
	case value <= 0xff:
		return ecTag{name: name, typ: 2, data: []byte{byte(value)}}
	case value <= 0xffff:
		data := make([]byte, 2)
		binary.BigEndian.PutUint16(data, uint16(value))
		return ecTag{name: name, typ: 3, data: data}
	case value <= 0xffffffff:
		data := make([]byte, 4)
		binary.BigEndian.PutUint32(data, uint32(value))
		return ecTag{name: name, typ: 4, data: data}
	default:
		data := make([]byte, 8)
		binary.BigEndian.PutUint64(data, value)
		return ecTag{name: name, typ: 5, data: data}
	}
}

func ecStringTag(name uint16, value string) (ecTag, error) {
	if !utf8.ValidString(value) {
		return ecTag{}, errors.New("EC string is not valid UTF-8")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return ecTag{}, errors.New("EC string contains NUL")
	}
	if len(value) >= ecMaxStringBytes {
		return ecTag{}, errors.New("EC string is too long")
	}
	data := make([]byte, len(value)+1)
	copy(data, value)
	return ecTag{name: name, typ: 6, data: data}, nil
}

func ecHashTag(name uint16, value []byte) (ecTag, error) {
	if len(value) != 16 {
		return ecTag{}, errors.New("EC hash must contain 16 bytes")
	}
	return ecTag{name: name, typ: 9, data: append([]byte(nil), value...)}, nil
}

func (t ecTag) child(name uint16) *ecTag {
	for i := range t.children {
		if t.children[i].name == name {
			return &t.children[i]
		}
	}
	return nil
}

func (t ecTag) integer() (uint64, error) {
	switch t.typ {
	case 2:
		if len(t.data) != 1 {
			return 0, errors.New("EC uint8 tag has invalid length")
		}
		return uint64(t.data[0]), nil
	case 3:
		if len(t.data) != 2 {
			return 0, errors.New("EC uint16 tag has invalid length")
		}
		return uint64(binary.BigEndian.Uint16(t.data)), nil
	case 4:
		if len(t.data) != 4 {
			return 0, errors.New("EC uint32 tag has invalid length")
		}
		return uint64(binary.BigEndian.Uint32(t.data)), nil
	case 5:
		if len(t.data) != 8 {
			return 0, errors.New("EC uint64 tag has invalid length")
		}
		return binary.BigEndian.Uint64(t.data), nil
	case 0:
		if len(t.data) != 0 {
			return 0, errors.New("empty EC tag has data")
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("EC tag %x is not an integer", t.name)
	}
}

func (t ecTag) stringValue() (string, error) {
	if t.typ != 6 {
		return "", fmt.Errorf("EC tag %x is not a string", t.name)
	}
	if len(t.data) == 0 || len(t.data) > ecMaxStringBytes || t.data[len(t.data)-1] != 0 {
		return "", errors.New("EC string tag has invalid termination")
	}
	value := t.data[:len(t.data)-1]
	if !utf8.Valid(value) {
		return "", errors.New("EC string tag is not valid UTF-8")
	}
	return string(value), nil
}

func (t ecTag) hashValue() ([]byte, error) {
	if t.typ != 9 || len(t.data) != 16 {
		return nil, fmt.Errorf("EC tag %x is not a 16-byte hash", t.name)
	}
	return append([]byte(nil), t.data...), nil
}

func (t ecTag) marshalAppend(dst []byte, depth int) ([]byte, error) {
	if depth > ecMaxTagDepth {
		return nil, errors.New("EC tag nesting is too deep")
	}
	if t.name > 0x7fff {
		return nil, fmt.Errorf("EC tag name %x is out of range", t.name)
	}
	if len(t.children) > math.MaxUint16 {
		return nil, errors.New("too many EC child tags")
	}
	body := make([]byte, 0, len(t.data)+2)
	if len(t.children) > 0 {
		var count [2]byte
		binary.BigEndian.PutUint16(count[:], uint16(len(t.children)))
		body = append(body, count[:]...)
		for _, child := range t.children {
			var err error
			body, err = child.marshalAppend(body, depth+1)
			if err != nil {
				return nil, err
			}
		}
	}
	body = append(body, t.data...)
	// aMule's EC tag length excludes this tag's own child-count field. It
	// still includes each child tag's header and payload (and nested child
	// count fields through their respective GetTagLen values).
	wireLength := len(body)
	if len(t.children) > 0 {
		wireLength -= 2
	}
	if len(body) > ecMaxFramePayload || wireLength < 0 || uint64(wireLength) > math.MaxUint32 {
		return nil, errors.New("EC tag payload is too large")
	}
	header := make([]byte, 7)
	encodedName := t.name << 1
	if len(t.children) > 0 {
		encodedName |= 1
	}
	binary.BigEndian.PutUint16(header[:2], encodedName)
	header[2] = t.typ
	binary.BigEndian.PutUint32(header[3:], uint32(wireLength))
	dst = append(dst, header...)
	dst = append(dst, body...)
	return dst, nil
}

func (p ecPacket) marshalBinary() ([]byte, error) {
	if len(p.tags) > math.MaxUint16 {
		return nil, errors.New("too many EC packet tags")
	}
	body := make([]byte, 3, 256)
	body[0] = p.op
	binary.BigEndian.PutUint16(body[1:3], uint16(len(p.tags)))
	for _, tag := range p.tags {
		var err error
		body, err = tag.marshalAppend(body, 0)
		if err != nil {
			return nil, err
		}
		if len(body) > ecMaxFramePayload {
			return nil, errors.New("EC packet payload is too large")
		}
	}
	return body, nil
}

func parseECTag(data []byte, pos *int, end, depth int) (ecTag, error) {
	if depth > ecMaxTagDepth {
		return ecTag{}, errors.New("EC tag nesting is too deep")
	}
	if *pos < 0 || *pos > end || end > len(data) || end-*pos < 7 {
		return ecTag{}, errors.New("truncated EC tag header")
	}
	encodedName := binary.BigEndian.Uint16(data[*pos : *pos+2])
	*pos += 2
	typ := data[*pos]
	*pos++
	length := binary.BigEndian.Uint32(data[*pos : *pos+4])
	*pos += 4
	if length > ecMaxFramePayload {
		return ecTag{}, errors.New("EC tag length exceeds packet bounds")
	}
	hasChildren := encodedName&1 != 0
	physicalLength := uint64(length)
	if hasChildren {
		// The declared length excludes this tag's own uint16 child count.
		physicalLength += 2
	}
	if physicalLength > uint64(end-*pos) || physicalLength > ecMaxFramePayload {
		return ecTag{}, errors.New("EC tag length exceeds packet bounds")
	}
	bodyStart := *pos
	bodyEnd := bodyStart + int(physicalLength)
	tag := ecTag{name: encodedName >> 1, typ: typ}
	if hasChildren {
		if length < 2 {
			return ecTag{}, errors.New("EC child tag container is truncated")
		}
		count := int(binary.BigEndian.Uint16(data[bodyStart : bodyStart+2]))
		if count > ecMaxTagCount {
			return ecTag{}, errors.New("too many EC child tags")
		}
		childPos := bodyStart + 2
		tag.children = make([]ecTag, 0, count)
		for i := 0; i < count; i++ {
			child, err := parseECTag(data, &childPos, bodyEnd, depth+1)
			if err != nil {
				return ecTag{}, err
			}
			tag.children = append(tag.children, child)
		}
		if childPos > bodyEnd {
			return ecTag{}, errors.New("EC child tags exceed container length")
		}
		tag.data = append([]byte(nil), data[childPos:bodyEnd]...)
	} else {
		tag.data = append([]byte(nil), data[bodyStart:bodyEnd]...)
	}
	*pos = bodyEnd
	if err := validateECTagData(tag); err != nil {
		return ecTag{}, err
	}
	return tag, nil
}

func validateECTagData(tag ecTag) error {
	validLength := func(want int) error {
		if len(tag.data) != want {
			return fmt.Errorf("EC tag %x has data length %d, want %d", tag.name, len(tag.data), want)
		}
		return nil
	}
	switch tag.typ {
	case 0:
		return validLength(0)
	case 1:
		if len(tag.data) > ecMaxFramePayload {
			return errors.New("EC custom tag is too large")
		}
	case 2:
		return validLength(1)
	case 3:
		return validLength(2)
	case 4:
		return validLength(4)
	case 5:
		return validLength(8)
	case 6:
		if len(tag.data) == 0 || len(tag.data) > ecMaxStringBytes || tag.data[len(tag.data)-1] != 0 {
			return fmt.Errorf("EC string tag %x is not NUL terminated", tag.name)
		}
		if !utf8.Valid(tag.data[:len(tag.data)-1]) {
			return fmt.Errorf("EC string tag %x is not UTF-8", tag.name)
		}
	case 7:
		// aMule transmits doubles as NUL-terminated text. Validate the
		// framing here; callers do not currently consume double tags.
		if len(tag.data) == 0 || tag.data[len(tag.data)-1] != 0 {
			return fmt.Errorf("EC double tag %x is not NUL terminated", tag.name)
		}
	case 8:
		return validLength(6)
	case 9:
		return validLength(16)
	case 10:
		return validLength(16)
	default:
		return fmt.Errorf("unknown EC tag type %d", tag.typ)
	}
	return nil
}

func parseECPacket(body []byte) (ecPacket, error) {
	if len(body) < 3 {
		return ecPacket{}, errors.New("EC packet is shorter than opcode and tag count")
	}
	count := int(binary.BigEndian.Uint16(body[1:3]))
	if count > ecMaxTagCount {
		return ecPacket{}, errors.New("too many EC packet tags")
	}
	packet := ecPacket{op: body[0], tags: make([]ecTag, 0, count)}
	pos := 3
	for i := 0; i < count; i++ {
		tag, err := parseECTag(body, &pos, len(body), 0)
		if err != nil {
			return ecPacket{}, err
		}
		packet.tags = append(packet.tags, tag)
	}
	if pos != len(body) {
		return ecPacket{}, errors.New("trailing EC packet data")
	}
	return packet, nil
}

func (p ecPacket) firstTag(name uint16) *ecTag {
	for i := range p.tags {
		if p.tags[i].name == name {
			return &p.tags[i]
		}
	}
	return nil
}

type ecRemoteError struct {
	operation string
	message   string
}

func (e *ecRemoteError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("aMule rejected %s", e.operation)
	}
	return fmt.Sprintf("aMule rejected %s: %s", e.operation, e.message)
}

func ecPacketError(packet ecPacket, operation string) error {
	message := ""
	if tag := packet.firstTag(ecTagString); tag != nil {
		if value, err := tag.stringValue(); err == nil {
			message = strings.TrimSpace(value)
		}
	}
	return &ecRemoteError{operation: operation, message: message}
}

type amuleECClient struct {
	conn   net.Conn
	active bool
	broken bool
}

func newAMuleECClient(ctx context.Context, host string, port int, passwordHash string) (*amuleECClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	if port <= 0 || port > 65535 {
		port = defaultAMulePort
	}
	if passwordHash == "" {
		return nil, errAMuleNoPass
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("connect to aMule EC: %w", err)
	}
	client := &amuleECClient{conn: conn}
	if err := client.authenticate(ctx, passwordHash); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (c *amuleECClient) close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.active = false
	err := c.conn.Close()
	c.conn = nil
	return err
}

func ecDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(ecIODeadlineWindow)
	if ctx != nil {
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
	}
	return deadline
}

func ecWriteAll(ctx context.Context, conn net.Conn, data []byte) error {
	if conn == nil {
		return errors.New("EC connection is closed")
	}
	for len(data) > 0 {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := conn.SetWriteDeadline(ecDeadline(ctx)); err != nil {
			return err
		}
		n, err := conn.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func ecReadFull(ctx context.Context, conn net.Conn, data []byte) error {
	if conn == nil {
		return errors.New("EC connection is closed")
	}
	for len(data) > 0 {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := conn.SetReadDeadline(ecDeadline(ctx)); err != nil {
			return err
		}
		n, err := conn.Read(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (c *amuleECClient) writePacket(ctx context.Context, packet ecPacket) error {
	payload, err := packet.marshalBinary()
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > ecMaxFramePayload {
		return errors.New("EC packet payload is out of bounds")
	}
	frame := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(frame[:4], ecFrameFlags)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	if err := ecWriteAll(ctx, c.conn, frame); err != nil {
		if ctx == nil || ctx.Err() == nil {
			c.broken = true
		}
		return err
	}
	return nil
}

func (c *amuleECClient) readPacket(ctx context.Context) (ecPacket, error) {
	var header [8]byte
	if err := ecReadFull(ctx, c.conn, header[:]); err != nil {
		if ctx == nil || ctx.Err() == nil {
			c.broken = true
		}
		return ecPacket{}, err
	}
	flags := binary.BigEndian.Uint32(header[:4])
	if flags != ecFrameFlags {
		c.broken = true
		return ecPacket{}, fmt.Errorf("unsupported aMule EC flags 0x%08x (optional flags were not negotiated)", flags)
	}
	length := binary.BigEndian.Uint32(header[4:])
	if length < 3 || length > ecMaxFramePayload {
		c.broken = true
		return ecPacket{}, fmt.Errorf("aMule EC frame length %d is out of bounds", length)
	}
	body := make([]byte, int(length))
	if err := ecReadFull(ctx, c.conn, body); err != nil {
		if ctx == nil || ctx.Err() == nil {
			c.broken = true
		}
		return ecPacket{}, err
	}
	packet, err := parseECPacket(body)
	if err != nil {
		c.broken = true
		return ecPacket{}, err
	}
	return packet, nil
}

func (c *amuleECClient) exchange(ctx context.Context, packet ecPacket) (ecPacket, error) {
	if c == nil || c.conn == nil {
		return ecPacket{}, errors.New("EC connection is closed")
	}
	if err := c.writePacket(ctx, packet); err != nil {
		return ecPacket{}, err
	}
	return c.readPacket(ctx)
}

// connectionState asks the core for its current network flags. The EC
// response is EC_OP_MISC_DATA containing an EC_TAG_CONNSTATE uint8 tag.
// Checking this before SEARCH_START avoids treating a disconnected core's
// empty result set as a successful search.
func (c *amuleECClient) connectionState(ctx context.Context) (uint64, error) {
	reply, err := c.exchange(ctx, ecPacket{op: ecOpGetConnState})
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("read connection state: %w", err)
	}
	if reply.op == 0x05 {
		return 0, ecPacketError(reply, "connection state")
	}
	if reply.op != ecOpMiscData {
		c.broken = true
		return 0, fmt.Errorf("connection state: expected opcode 0x%02x, got 0x%02x", ecOpMiscData, reply.op)
	}
	stateTag := reply.firstTag(ecTagConnState)
	if stateTag == nil {
		c.broken = true
		return 0, errors.New("connection state response has no state tag")
	}
	state, err := stateTag.integer()
	if err != nil {
		c.broken = true
		return 0, fmt.Errorf("invalid connection state: %w", err)
	}
	return state, nil
}

func (c *amuleECClient) ensureSearchNetwork(ctx context.Context, mode amuleSearchMode) error {
	state, err := c.connectionState(ctx)
	if err != nil {
		return err
	}
	required := ecConnStateED2KConnected
	if mode.typeID == ecSearchKad {
		required = ecConnStateKadConnected
	}
	if state&required == 0 {
		return fmt.Errorf("aMule %s network is not connected (connection state 0x%02x); connect or bootstrap it first", mode.name, state)
	}
	return nil
}

func (c *amuleECClient) authenticate(ctx context.Context, passwordHash string) error {
	clientName, err := ecStringTag(ecTagClientName, "wire-download")
	if err != nil {
		return err
	}
	clientVersion, err := ecStringTag(ecTagClientVersion, "native-search")
	if err != nil {
		return err
	}
	reply, err := c.exchange(ctx, ecPacket{op: ecOpAuthReq, tags: []ecTag{
		clientName,
		clientVersion,
		ecUintTag(ecTagProtocol, ecProtocolVersion),
	}})
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("aMule EC authentication request: %w", err)
	}
	if reply.op == ecOpAuthFail {
		return ecPacketError(reply, "EC authentication")
	}
	if reply.op != ecOpAuthSalt {
		return fmt.Errorf("aMule EC authentication: expected salt, got opcode 0x%02x", reply.op)
	}
	saltTag := reply.firstTag(ecTagPasswordSalt)
	if saltTag == nil {
		return errors.New("aMule EC authentication response has no password salt")
	}
	salt, err := saltTag.integer()
	if err != nil {
		return fmt.Errorf("invalid aMule EC password salt: %w", err)
	}
	challenge := ecPasswordChallenge(passwordHash, salt)
	passwordTag, err := ecHashTag(ecTagPasswordHash, challenge)
	if err != nil {
		return err
	}
	reply, err = c.exchange(ctx, ecPacket{op: ecOpAuthPassword, tags: []ecTag{passwordTag}})
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("aMule EC password authentication: %w", err)
	}
	if reply.op == ecOpAuthFail {
		return ecPacketError(reply, "EC password authentication")
	}
	if reply.op != ecOpAuthOK {
		return fmt.Errorf("aMule EC password authentication: expected success, got opcode 0x%02x", reply.op)
	}
	return nil
}

func ecPasswordChallenge(passwordHash string, salt uint64) []byte {
	saltText := fmt.Sprintf("%X", salt)
	saltDigest := md5.Sum([]byte(saltText))
	joined := strings.ToLower(passwordHash) + hex.EncodeToString(saltDigest[:])
	challenge := md5.Sum([]byte(joined))
	return challenge[:]
}

func readAMuleECPasswordHash(configDir, plain string) (string, error) {
	if strings.TrimSpace(configDir) != "" {
		path := filepath.Join(configDir, "remote.conf")
		contents, err := os.ReadFile(path)
		if err == nil {
			if hash, found, parseErr := parseAMuleECPassword(contents); found || parseErr != nil {
				if parseErr != nil {
					return "", parseErr
				}
				return hash, nil
			}
		} else if !os.IsNotExist(err) && strings.TrimSpace(plain) == "" {
			return "", fmt.Errorf("read aMule remote configuration: %w", err)
		}
	}
	if strings.TrimSpace(plain) == "" {
		return "", errAMuleNoPass
	}
	digest := md5.Sum([]byte(plain))
	return hex.EncodeToString(digest[:]), nil
}

func parseAMuleECPassword(contents []byte) (hash string, found bool, err error) {
	section := ""
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "ec" || !strings.EqualFold(strings.TrimSpace(key), "password") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) != 32 {
			return "", true, errors.New("aMule remote.conf contains an invalid EC password hash")
		}
		decoded, decodeErr := hex.DecodeString(value)
		if decodeErr != nil || len(decoded) != 16 {
			return "", true, errors.New("aMule remote.conf contains an invalid EC password hash")
		}
		return strings.ToLower(value), true, nil
	}
	return "", false, nil
}

type amuleSearchMode struct {
	name   string
	typeID uint64
}

func requestedAMuleSearchModes(mode string) ([]amuleSearchMode, error) {
	switch mode {
	case "server":
		return []amuleSearchMode{{name: "server", typeID: ecSearchLocal}}, nil
	case "global":
		return []amuleSearchMode{{name: "global", typeID: ecSearchGlobal}}, nil
	case "kad":
		return []amuleSearchMode{{name: "kad", typeID: ecSearchKad}}, nil
	case "all":
		return []amuleSearchMode{{name: "global", typeID: ecSearchGlobal}, {name: "kad", typeID: ecSearchKad}}, nil
	default:
		return nil, fmt.Errorf("unsupported eD2k search mode %q", mode)
	}
}

func (a *AMule) acquireSearchGate(ctx context.Context) (chan struct{}, error) {
	if a == nil {
		return nil, errAMuleClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errAMuleClosed
	}
	if a.searchGate == nil {
		a.searchGate = make(chan struct{}, 1)
	}
	gate := a.searchGate
	a.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return gate, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *AMule) searchConfig() (host string, port int, configDir, password string, err error) {
	if a == nil {
		return "", 0, "", "", errAMuleClosed
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return "", 0, "", "", errAMuleClosed
	}
	host, port, configDir, password = a.host, a.port, a.configDir, a.password
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 || port > 65535 {
		port = defaultAMulePort
	}
	return host, port, configDir, password, nil
}

// Search runs one or more native aMule searches and emits eD2k links as
// results arrive.  A single aMule EC connection is used for all requested
// modes so the timeout supplied by the caller is shared across the stages.
// The daemon's search manager owns result IDs and later calls Store.Add with
// the emitted link; EC_DOWNLOAD_SEARCH_RESULT is intentionally unnecessary.
func (a *AMule) Search(ctx context.Context, req search.Request, emit search.Emit) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if req.Type != "all" && req.Type != "ed2k" {
		return errors.New("aMule search supports only all or ed2k result types")
	}
	modes, err := requestedAMuleSearchModes(req.ED2KMode)
	if err != nil {
		return err
	}
	searchCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	gate, err := a.acquireSearchGate(searchCtx)
	if err != nil {
		return err
	}
	defer func() { <-gate }()
	host, port, configDir, plainPassword, err := a.searchConfig()
	if err != nil {
		return err
	}
	passwordHash, err := readAMuleECPasswordHash(configDir, plainPassword)
	if err != nil {
		return err
	}
	client, err := newAMuleECClient(searchCtx, host, port, passwordHash)
	if err != nil {
		return err
	}
	defer func() {
		// If a caller cancels while a search is in flight, issue the EC stop
		// request before closing the connection. The request is best effort and
		// uses a fresh short context because searchCtx is already cancelled.
		if client != nil && client.active && searchCtx.Err() != nil {
			client.cancelActiveSearch()
		}
		if client != nil {
			_ = client.close()
		}
	}()

	var failures []error
	for index, mode := range modes {
		stageBase, stageWidth := 0, 100
		if len(modes) > 1 {
			stageBase = index * 50
			stageWidth = 50
		}
		stageCtx := searchCtx
		stageCancel := func() {}
		if len(modes) > 1 {
			// Divide the remaining deadline among the stages still to run. A
			// global search can otherwise consume the entire request timeout and
			// make ED2KMode=all silently skip Kad.
			if deadline, ok := searchCtx.Deadline(); ok {
				remaining := time.Until(deadline)
				stagesLeft := len(modes) - index
				budget := remaining / time.Duration(stagesLeft)
				if budget <= 0 {
					return contextDeadlineError(searchCtx)
				}
				stageCtx, stageCancel = context.WithTimeout(searchCtx, budget)
			}
		}
		var stageErr error
		if err := client.ensureSearchNetwork(stageCtx, mode); err != nil {
			stageErr = err
		} else {
			_, stageErr = client.runSearch(stageCtx, req.Query, mode, func(results []search.Result, progress int) {
				if emit == nil {
					return
				}
				if len(modes) > 1 {
					progress = stageBase + progress*stageWidth/100
				}
				emit(results, max(0, min(100, progress)))
			})
		}
		stageCancel()
		if stageErr == nil {
			continue
		}
		failures = append(failures, fmt.Errorf("%s search: %w", mode.name, stageErr))
		if searchCtx.Err() != nil {
			break
		}
		if client.broken && index+1 < len(modes) {
			// A malformed response or transport failure can leave an
			// outstanding request on the old connection. Discard it and
			// authenticate a fresh session before trying the next network.
			_ = client.close()
			nextClient, reconnectErr := newAMuleECClient(searchCtx, host, port, passwordHash)
			if reconnectErr != nil {
				failures = append(failures, fmt.Errorf("reconnect before %s search: %w", modes[index+1].name, reconnectErr))
				break
			}
			client = nextClient
			continue
		}
		if errors.Is(stageErr, context.DeadlineExceeded) && index+1 < len(modes) {
			// A timed-out run has sent SEARCH_STOP, whose response is not
			// useful to the next stage. Close that EC session and authenticate a
			// fresh one before continuing with the remaining stage.
			_ = client.close()
			nextClient, reconnectErr := newAMuleECClient(searchCtx, host, port, passwordHash)
			if reconnectErr != nil {
				failures = append(failures, fmt.Errorf("reconnect before %s search: %w", modes[index+1].name, reconnectErr))
				break
			}
			client = nextClient
		}
		// A remote EC_OP_FAILED response has been fully consumed and the
		// connection remains usable. In all mode, continue with the other
		// network so the manager can retain partial results and report the
		// failed stage.
	}
	if len(failures) > 0 {
		return fmt.Errorf("aMule search incomplete: %w", errors.Join(failures...))
	}
	return nil
}

func contextDeadlineError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return context.DeadlineExceeded
}

func (c *amuleECClient) runSearch(ctx context.Context, query string, mode amuleSearchMode, emit func([]search.Result, int)) ([]search.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	nameTag, err := ecStringTag(ecTagSearchName, query)
	if err != nil {
		return nil, err
	}
	fileTypeTag, err := ecStringTag(ecTagSearchFileType, "")
	if err != nil {
		return nil, err
	}
	searchTag := ecUintTag(ecTagSearchType, mode.typeID)
	searchTag.children = []ecTag{nameTag, fileTypeTag}
	reply, err := c.exchange(ctx, ecPacket{op: ecOpSearchStart, tags: []ecTag{searchTag}})
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("start request: %w", err)
	}
	if reply.op == ecOpAuthFail || reply.op == 0x05 {
		return nil, ecPacketError(reply, "search start")
	}
	if reply.op != ecOpStrings {
		c.broken = true
		return nil, fmt.Errorf("search start: expected opcode 0x%02x, got 0x%02x", ecOpStrings, reply.op)
	}
	c.active = true
	finished := false
	defer func() {
		if finished || !c.active {
			return
		}
		if ctx.Err() != nil {
			// The caller's context is already expired, so SEARCH_STOP cannot
			// share it. Send a best-effort stop with a fresh short context and
			// close this session; its unread reply must not reach a later stage.
			c.cancelActiveSearch()
			_ = c.close()
			return
		}
		// Any protocol or result decoding error leaves the core's current
		// search context in an unknown state. Drain the stop reply when the
		// transport is still usable; a failed cleanup marks the session broken
		// so an all-mode search will re-authenticate before its next stage.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), ecSearchCancelWindow)
		cleanupErr := c.stopActiveSearchAndDrain(cleanupCtx)
		cancel()
		if cleanupErr != nil {
			c.broken = true
		}
	}()

	byKey := make(map[string]search.Result)
	ordered := make([]search.Result, 0)
	var globalStopAt time.Time
	globalStarted := time.Now()
	globalExchangeCtx := ctx
	var cancelGlobalExchange context.CancelFunc
	if mode.typeID == ecSearchGlobal {
		if deadline, ok := ctx.Deadline(); ok {
			globalStopAt = deadline.Add(-ecSearchGlobalCleanupReserve)
			if !globalStopAt.After(time.Now()) {
				globalStopAt = time.Now()
			}
			globalExchangeCtx, cancelGlobalExchange = context.WithDeadline(ctx, globalStopAt)
			defer cancelGlobalExchange()
		}
	}
	finishGlobal := func() ([]search.Result, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), ecSearchGlobalCleanupReserve)
		stopErr := c.stopActiveSearchAndDrain(cleanupCtx)
		cancel()
		if stopErr != nil {
			c.broken = true
			if ctx.Err() != nil {
				return ordered, ctx.Err()
			}
			return ordered, fmt.Errorf("stop global search before deadline: %w", stopErr)
		}
		finished = true
		if ctx.Err() != nil {
			return ordered, ctx.Err()
		}
		return ordered, nil
	}
	for {
		if ctx.Err() != nil {
			return ordered, ctx.Err()
		}
		if mode.typeID == ecSearchGlobal && !globalStopAt.IsZero() && !time.Now().Before(globalStopAt) {
			return finishGlobal()
		}
		progressReply, err := c.exchange(globalExchangeCtx, ecPacket{op: ecOpSearchProgress})
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return ordered, ctx.Err()
			}
			if mode.typeID == ecSearchGlobal && !globalStopAt.IsZero() && !time.Now().Before(globalStopAt) {
				return finishGlobal()
			}
			return ordered, fmt.Errorf("read progress: %w", err)
		}
		if progressReply.op == 0x05 {
			c.active = false
			finished = true
			return ordered, ecPacketError(progressReply, "search progress")
		}
		if progressReply.op != ecOpSearchProgress {
			c.broken = true
			return ordered, fmt.Errorf("search progress: expected opcode 0x%02x, got 0x%02x", ecOpSearchProgress, progressReply.op)
		}
		statusTag := progressReply.firstTag(ecTagSearchStatus)
		if statusTag == nil {
			c.broken = true
			return ordered, errors.New("search progress response has no status tag")
		}
		status, err := statusTag.integer()
		if err != nil {
			c.broken = true
			return ordered, fmt.Errorf("invalid search progress status: %w", err)
		}

		resultsReply, err := c.exchange(globalExchangeCtx, ecPacket{op: ecOpSearchResults})
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return ordered, ctx.Err()
			}
			if mode.typeID == ecSearchGlobal && !globalStopAt.IsZero() && !time.Now().Before(globalStopAt) {
				return finishGlobal()
			}
			return ordered, fmt.Errorf("read results: %w", err)
		}
		if resultsReply.op == 0x05 {
			c.active = false
			finished = true
			return ordered, ecPacketError(resultsReply, "search results")
		}
		if resultsReply.op != ecOpSearchResults {
			c.broken = true
			return ordered, fmt.Errorf("search results: expected opcode 0x%02x, got 0x%02x", ecOpSearchResults, resultsReply.op)
		}
		parsed, err := parseECSearchResults(resultsReply)
		if err != nil {
			c.broken = true
			return ordered, err
		}
		newResults := make([]search.Result, 0, len(parsed))
		for _, result := range parsed {
			key := result.Link
			if old, ok := byKey[key]; ok {
				old.Seeds = max(old.Seeds, result.Seeds)
				old.Peers = max(old.Peers, result.Peers)
				byKey[key] = old
				// Emit updated counts as well. The manager's stable result ID
				// merges the update without increasing its per-source count.
				newResults = append(newResults, old)
				continue
			}
			byKey[key] = result
			ordered = append(ordered, result)
			newResults = append(newResults, result)
			if len(ordered) >= 1000 {
				break
			}
		}
		progress := ecSearchProgress(mode.typeID, status)
		if mode.typeID == ecSearchGlobal && !globalStopAt.IsZero() {
			// Global EC percentages may start at 100 before any server reply.
			// Display elapsed collection-window progress instead.
			window := globalStopAt.Sub(globalStarted)
			progress = 99
			if window > 0 {
				progress = max(0, min(99, int(time.Since(globalStarted)*100/window)))
			}
		}
		if emit != nil {
			emit(newResults, progress)
			if len(newResults) == 0 {
				emit(nil, progress)
			}
		}
		if len(ordered) >= 1000 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), ecSearchCancelWindow)
			stopErr := c.stopActiveSearchAndDrain(cleanupCtx)
			cancel()
			if stopErr != nil {
				c.broken = true
				if ctx.Err() != nil {
					return ordered, ctx.Err()
				}
				return ordered, fmt.Errorf("stop search after result limit: %w", stopErr)
			}
			finished = true
			if ctx.Err() != nil {
				return ordered, ctx.Err()
			}
			return ordered, nil
		}
		if mode.typeID == ecSearchGlobal {
			if !globalStopAt.IsZero() && !time.Now().Before(globalStopAt) {
				return finishGlobal()
			}
		} else if ecSearchDone(mode.typeID, status) {
			c.active = false
			finished = true
			return ordered, nil
		}
		wait := ecSearchPollInterval
		if mode.typeID == ecSearchGlobal && !globalStopAt.IsZero() {
			if remaining := time.Until(globalStopAt); remaining < wait {
				wait = remaining
			}
			if wait <= 0 {
				continue
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ordered, ctx.Err()
		case <-timer.C:
		}
	}
}

func ecSearchProgress(mode, status uint64) int {
	switch mode {
	case ecSearchGlobal:
		if status >= 100 {
			return 100
		}
		return int(status)
	case ecSearchLocal:
		// aMule reports 0xffff while a local search is active and 0 after
		// completion; no finer-grained local progress is available.
		if status == 0xffff {
			return 1
		}
		return 100
	case ecSearchKad:
		if status == 0xfffe {
			return 100
		}
		return 1
	default:
		return 0
	}
}

func ecSearchDone(mode, status uint64) bool {
	switch mode {
	case ecSearchLocal:
		return status != 0xffff
	case ecSearchGlobal:
		return status >= 100
	case ecSearchKad:
		return status == 0xfffe
	default:
		return false
	}
}

func parseECSearchResults(packet ecPacket) ([]search.Result, error) {
	results := make([]search.Result, 0, len(packet.tags))
	for _, tag := range packet.tags {
		if tag.name != ecTagSearchFile {
			continue
		}
		nameTag := tag.child(ecTagPartFileName)
		sizeTag := tag.child(ecTagPartFileSizeFull)
		hashTag := tag.child(ecTagPartFileHash)
		if nameTag == nil || sizeTag == nil || hashTag == nil {
			return nil, errors.New("aMule search result is missing name, size, or hash")
		}
		name, err := nameTag.stringValue()
		if err != nil {
			return nil, fmt.Errorf("invalid aMule search result name: %w", err)
		}
		size, err := sizeTag.integer()
		if err != nil || size == 0 || size > math.MaxInt64 {
			if err == nil {
				err = errors.New("size must be between 1 and MaxInt64")
			}
			return nil, fmt.Errorf("invalid aMule search result size: %w", err)
		}
		hash, err := hashTag.hashValue()
		if err != nil {
			return nil, fmt.Errorf("invalid aMule search result hash: %w", err)
		}
		name = cleanAMuleSearchName(name)
		if name == "" {
			name = hex.EncodeToString(hash)
		}
		hashText := hex.EncodeToString(hash)
		link := fmt.Sprintf("ed2k://|file|%s|%d|%s|/", url.PathEscape(name), size, hashText)
		result := search.Result{Name: name, Kind: "ed2k", Link: link, Size: int64(size)}
		if sourceTag := tag.child(ecTagPartFileSourceCnt); sourceTag != nil {
			value, err := sourceTag.integer()
			if err != nil || value > math.MaxInt64 {
				return nil, fmt.Errorf("invalid aMule search source count: %w", err)
			}
			result.Peers = int64(value)
		}
		if completeTag := tag.child(ecTagPartFileSourceXfer); completeTag != nil {
			value, err := completeTag.integer()
			if err != nil || value > math.MaxInt64 {
				return nil, fmt.Errorf("invalid aMule complete source count: %w", err)
			}
			result.Seeds = int64(value)
		}
		results = append(results, result)
	}
	return results, nil
}

func cleanAMuleSearchName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
}

func (c *amuleECClient) cancelActiveSearch() {
	if c == nil || c.conn == nil || !c.active {
		return
	}
	c.active = false
	ctx, cancel := context.WithTimeout(context.Background(), ecSearchCancelWindow)
	defer cancel()
	_ = c.writePacket(ctx, ecPacket{op: ecOpSearchStop})
}

// stopActiveSearchAndDrain stops a search and consumes the EC reply. It is
// used when a provider is abandoning a search but intends to reuse the same
// connection; cancellation uses cancelActiveSearch instead and closes the
// socket immediately.
func (c *amuleECClient) stopActiveSearchAndDrain(ctx context.Context) error {
	if c == nil || c.conn == nil || !c.active {
		return nil
	}
	if err := c.writePacket(ctx, ecPacket{op: ecOpSearchStop}); err != nil {
		c.active = false
		return err
	}
	reply, err := c.readPacket(ctx)
	c.active = false
	if err != nil {
		return err
	}
	if reply.op == 0x05 {
		return ecPacketError(reply, "search stop")
	}
	if reply.op != ecOpMiscData {
		return fmt.Errorf("search stop: expected opcode 0x%02x, got 0x%02x", ecOpMiscData, reply.op)
	}
	return nil
}
