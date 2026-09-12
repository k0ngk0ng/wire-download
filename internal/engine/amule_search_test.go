package engine

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/search"
)

func writeTestECPacket(w io.Writer, packet ecPacket) error {
	payload, err := packet.marshalBinary()
	if err != nil {
		return err
	}
	frame := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(frame[:4], ecFrameFlags)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if n > 0 {
			frame = frame[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readTestECPacket(r io.Reader) (ecPacket, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return ecPacket{}, err
	}
	if got := binary.BigEndian.Uint32(header[:4]); got != ecFrameFlags {
		return ecPacket{}, fmt.Errorf("flags 0x%x", got)
	}
	length := binary.BigEndian.Uint32(header[4:])
	if length < 3 || length > ecMaxFramePayload {
		return ecPacket{}, fmt.Errorf("length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return ecPacket{}, err
	}
	return parseECPacket(body)
}

type testAMuleSearchServer struct {
	listener  net.Listener
	password  string
	salt      uint64
	hash      []byte
	connState uint64

	mu        sync.Mutex
	modes     []uint64
	stopCount int
	active    int
	maxActive int
	// delayGlobalResult makes the first global result poll empty even though
	// progress is already 100. This models aMule's initially empty server
	// queue and exercises the bounded settle window in the client.
	delayGlobalResult bool
	globalResultPolls int
	globalResultDelay int
	resultSent        bool
	starts            chan struct{}
	stopSeen          chan struct{}
	done              chan error
}

func newTestAMuleSearchServer(t *testing.T, password string) *testAMuleSearchServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testAMuleSearchServer{
		listener:          listener,
		password:          password,
		salt:              0x0102030405060708,
		hash:              mustTestHash(t, "0123456789abcdef0123456789abcdef"),
		connState:         ecConnStateED2KConnected | ecConnStateKadConnected | ecConnStateKadRunning,
		globalResultDelay: 1,
		starts:            make(chan struct{}, 8),
		stopSeen:          make(chan struct{}, 1),
		done:              make(chan error, 1),
	}
	go server.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case err := <-server.done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("EC test server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("EC test server did not stop")
		}
	})
	return server
}

func mustTestHash(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *testAMuleSearchServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *testAMuleSearchServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.done <- err
			return
		}
		go func() {
			if err := s.handle(conn); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				s.done <- err
			}
		}()
	}
}

func (s *testAMuleSearchServer) handle(conn net.Conn) error {
	defer conn.Close()
	auth, err := readTestECPacket(conn)
	if err != nil {
		return err
	}
	if auth.op != ecOpAuthReq {
		return fmt.Errorf("first opcode 0x%x", auth.op)
	}
	protocol := auth.firstTag(ecTagProtocol)
	if protocol == nil {
		return errors.New("missing protocol tag")
	}
	if value, err := protocol.integer(); err != nil || value != ecProtocolVersion {
		return fmt.Errorf("protocol=%d err=%v", value, err)
	}
	if err := writeTestECPacket(conn, ecPacket{op: ecOpAuthSalt, tags: []ecTag{ecUintTag(ecTagPasswordSalt, s.salt)}}); err != nil {
		return err
	}
	password, err := readTestECPacket(conn)
	if err != nil {
		return err
	}
	if password.op != ecOpAuthPassword {
		return fmt.Errorf("password opcode 0x%x", password.op)
	}
	tag := password.firstTag(ecTagPasswordHash)
	if tag == nil {
		return errors.New("missing password tag")
	}
	got, err := tag.hashValue()
	if err != nil {
		return err
	}
	plainDigest := md5.Sum([]byte(s.password))
	want := ecPasswordChallenge(hex.EncodeToString(plainDigest[:]), s.salt)
	if string(got) != string(want) {
		return errors.New("wrong password challenge")
	}
	if err := writeTestECPacket(conn, ecPacket{op: ecOpAuthOK}); err != nil {
		return err
	}
	for {
		request, err := readTestECPacket(conn)
		if err != nil {
			return err
		}
		switch request.op {
		case ecOpSearchStart:
			searchTag := request.firstTag(ecTagSearchType)
			if searchTag == nil {
				return errors.New("missing search tag")
			}
			mode, err := searchTag.integer()
			if err != nil {
				return err
			}
			if searchTag.child(ecTagSearchName) == nil || searchTag.child(ecTagSearchFileType) == nil {
				return errors.New("search request did not include name and file type")
			}
			query, err := searchTag.child(ecTagSearchName).stringValue()
			if err != nil || query == "" {
				return fmt.Errorf("query=%q err=%v", query, err)
			}
			s.mu.Lock()
			s.modes = append(s.modes, mode)
			s.active++
			s.globalResultPolls = 0
			s.resultSent = false
			if s.active > s.maxActive {
				s.maxActive = s.active
			}
			s.mu.Unlock()
			s.starts <- struct{}{}
			if err := writeTestECPacket(conn, ecPacket{op: ecOpStrings}); err != nil {
				return err
			}
		case ecOpGetConnState:
			s.mu.Lock()
			state := s.connState
			s.mu.Unlock()
			if err := writeTestECPacket(conn, ecPacket{op: ecOpMiscData, tags: []ecTag{ecUintTag(ecTagConnState, state)}}); err != nil {
				return err
			}
		case ecOpSearchProgress:
			mode := s.currentMode()
			status := uint64(0)
			switch mode {
			case ecSearchLocal:
				status = 0
			case ecSearchGlobal:
				status = 100
			case ecSearchKad:
				status = 0xfffe
			}
			if err := writeTestECPacket(conn, ecPacket{op: ecOpSearchProgress, tags: []ecTag{ecUintTag(ecTagSearchStatus, status)}}); err != nil {
				return err
			}
		case ecOpSearchResults:
			mode := s.currentMode()
			s.mu.Lock()
			if mode == ecSearchGlobal {
				s.globalResultPolls++
			}
			sendResult := !s.resultSent && (!s.delayGlobalResult || mode != ecSearchGlobal || s.globalResultPolls > s.globalResultDelay)
			if sendResult {
				s.resultSent = true
			}
			s.mu.Unlock()
			response := ecPacket{op: ecOpSearchResults}
			if sendResult {
				response.tags = []ecTag{testSearchResultTag(s.hash, "fixture | file.iso", 12345, 7, 3)}
			}
			if err := writeTestECPacket(conn, response); err != nil {
				return err
			}
		case ecOpSearchStop:
			s.mu.Lock()
			s.stopCount++
			s.active = 0
			s.mu.Unlock()
			select {
			case s.stopSeen <- struct{}{}:
			default:
			}
			if err := writeTestECPacket(conn, ecPacket{op: ecOpMiscData}); err != nil {
				return err
			}
			// The native client reuses this authenticated session for the next
			// stage of an all-network search.
			continue
		default:
			return fmt.Errorf("unexpected opcode 0x%x", request.op)
		}
	}
}

func (s *testAMuleSearchServer) currentMode() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.modes) == 0 {
		return ecSearchLocal
	}
	return s.modes[len(s.modes)-1]
}

func testSearchResultTag(hash []byte, name string, size, sources, complete uint64) ecTag {
	nameTag, _ := ecStringTag(ecTagPartFileName, name)
	hashTag, _ := ecHashTag(ecTagPartFileHash, hash)
	root := ecUintTag(ecTagSearchFile, 1)
	root.children = []ecTag{
		ecUintTag(ecTagPartFileSourceCnt, sources),
		ecUintTag(ecTagPartFileSourceXfer, complete),
		ecUintTag(ecTagPartFileStatus, 0),
		nameTag,
		ecUintTag(ecTagPartFileSizeFull, size),
		hashTag,
	}
	return root
}

func TestAMuleSearchNativeECAllModes(t *testing.T) {
	const password = "native-secret"
	server := newTestAMuleSearchServer(t, password)
	a := NewAMule("amulecmd", "", password, server.port())
	var got []search.Result
	var progresses []int
	err := a.Search(context.Background(), search.Request{Query: "fixture", Type: "ed2k", ED2KMode: "all", TimeoutSeconds: 5}, func(results []search.Result, progress int) {
		got = append(got, results...)
		progresses = append(progresses, progress)
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results=%d, want one result from each stage: %#v", len(got), got)
	}
	if got[0].Link != "ed2k://|file|fixture%20%7C%20file.iso|12345|0123456789abcdef0123456789abcdef|/" {
		t.Fatalf("result link = %q", got[0].Link)
	}
	if got[0].Name != "fixture | file.iso" || got[0].Size != 12345 || got[0].Peers != 7 || got[0].Seeds != 3 {
		t.Fatalf("result = %#v", got[0])
	}
	if len(progresses) == 0 || progresses[len(progresses)-1] != 100 {
		t.Fatalf("progresses=%v, want final 100", progresses)
	}
	server.mu.Lock()
	modes := append([]uint64(nil), server.modes...)
	maxActive := server.maxActive
	server.mu.Unlock()
	if len(modes) != 2 || modes[0] != ecSearchGlobal || modes[1] != ecSearchKad {
		t.Fatalf("search modes=%v, want global then kad", modes)
	}
	if maxActive > 1 {
		t.Fatalf("search stages overlapped: max active=%d", maxActive)
	}
}

func TestAMuleSearchPreflightRejectsDisconnectedNetwork(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		state uint64
	}{
		{name: "server", mode: "server", state: ecConnStateKadConnected},
		{name: "kad", mode: "kad", state: ecConnStateED2KConnected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestAMuleSearchServer(t, "preflight-secret")
			server.mu.Lock()
			server.connState = tt.state
			server.mu.Unlock()
			a := NewAMule("amulecmd", "", "preflight-secret", server.port())
			err := a.Search(context.Background(), search.Request{
				Query: "disconnected", Type: "ed2k", ED2KMode: tt.mode, TimeoutSeconds: 5,
			}, nil)
			if err == nil || !strings.Contains(err.Error(), "not connected") {
				t.Fatalf("Search() error=%v, want a clear disconnected-network error", err)
			}
			server.mu.Lock()
			starts := len(server.modes)
			server.mu.Unlock()
			if starts != 0 {
				t.Fatalf("Search started despite disconnected network: %d starts", starts)
			}
		})
	}
}

func TestAMuleSearchGlobalCollectsDelayedInitialResult(t *testing.T) {
	server := newTestAMuleSearchServer(t, "delayed-secret")
	server.mu.Lock()
	server.delayGlobalResult = true
	server.globalResultDelay = 3
	server.mu.Unlock()
	passwordDigest := md5.Sum([]byte("delayed-secret"))
	passwordHash := hex.EncodeToString(passwordDigest[:])
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newAMuleECClient(ctx, "127.0.0.1", server.port(), passwordHash)
	if err != nil {
		t.Fatalf("newAMuleECClient() error=%v", err)
	}
	defer client.close()
	var got []search.Result
	_, err = client.runSearch(ctx, "delayed", amuleSearchMode{name: "global", typeID: ecSearchGlobal}, func(results []search.Result, _ int) {
		got = append(got, results...)
	})
	if err != nil {
		t.Fatalf("Search() error=%v", err)
	}
	if len(got) == 0 {
		t.Fatal("global search dropped result returned after initial status 100")
	}
	server.mu.Lock()
	stops := server.stopCount
	server.mu.Unlock()
	if stops != 1 {
		t.Fatalf("global search stop count=%d, want one cleanup stop", stops)
	}
}

func TestAMuleSearchCancellationSendsStopAndReleasesGate(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	password := "cancel-secret"
	serverDone := make(chan error, 1)
	stopSeen := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		if _, err = readTestECPacket(conn); err != nil {
			serverDone <- err
			return
		}
		salt := uint64(9)
		if err = writeTestECPacket(conn, ecPacket{op: ecOpAuthSalt, tags: []ecTag{ecUintTag(ecTagPasswordSalt, salt)}}); err != nil {
			serverDone <- err
			return
		}
		if _, err = readTestECPacket(conn); err != nil {
			serverDone <- err
			return
		}
		if err = writeTestECPacket(conn, ecPacket{op: ecOpAuthOK}); err != nil {
			serverDone <- err
			return
		}
		stateRequest, stateErr := readTestECPacket(conn)
		if stateErr != nil {
			serverDone <- stateErr
			return
		}
		if stateRequest.op != ecOpGetConnState {
			serverDone <- fmt.Errorf("expected connection-state request, got opcode 0x%x", stateRequest.op)
			return
		}
		if err = writeTestECPacket(conn, ecPacket{op: ecOpMiscData, tags: []ecTag{ecUintTag(ecTagConnState, ecConnStateED2KConnected)}}); err != nil {
			serverDone <- err
			return
		}
		if _, err = readTestECPacket(conn); err != nil {
			serverDone <- err
			return
		}
		if err = writeTestECPacket(conn, ecPacket{op: ecOpStrings}); err != nil {
			serverDone <- err
			return
		}
		for {
			packet, readErr := readTestECPacket(conn)
			if readErr != nil {
				serverDone <- readErr
				return
			}
			if packet.op == ecOpSearchStop {
				stopSeen <- struct{}{}
				serverDone <- nil
				return
			}
			// Keep progress blocked so cancellation must interrupt a read.
		}
	}()

	a := NewAMule("amulecmd", "", password, listener.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- a.Search(ctx, search.Request{Query: "cancel", Type: "ed2k", ED2KMode: "server", TimeoutSeconds: 5}, nil)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Search cancellation error=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Search did not return after cancellation")
	}
	select {
	case <-stopSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("Search did not send EC search stop")
	}
	if err := <-serverDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}

	// A cancelled search must release the gate even if the caller immediately
	// starts another request. The second request is expected to fail at the
	// closed test listener, rather than wait for the old search.
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer secondCancel()
	start := time.Now()
	_ = a.Search(secondCtx, search.Request{Query: "again", Type: "ed2k", ED2KMode: "server", TimeoutSeconds: 5}, nil)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("search gate was not released promptly")
	}
}

func TestAMuleSearchRejectsUnnegotiatedFlagsAndBounds(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ec := &amuleECClient{conn: client}
	done := make(chan error, 1)
	go func() {
		frame := make([]byte, 8)
		binary.BigEndian.PutUint32(frame[:4], ecFrameFlags|ecFlagUTF8)
		binary.BigEndian.PutUint32(frame[4:], 3)
		_, err := server.Write(frame)
		done <- err
	}()
	if _, err := ec.readPacket(context.Background()); err == nil || !strings.Contains(err.Error(), "optional flags") {
		t.Fatalf("readPacket error=%v, want negotiated flags error", err)
	}
	<-done

	if _, err := parseECPacket([]byte{ecOpStrings, 0, 1}); err == nil {
		t.Fatal("truncated packet tag was accepted")
	}
	malformed := []byte{ecOpStrings, 0, 1, 0, 0, 6, 0, 0, 0, 0}
	if _, err := parseECPacket(malformed); err == nil {
		t.Fatal("out-of-bounds tag was accepted")
	}
}

func TestAMuleSearchGateSerializesWaiters(t *testing.T) {
	a := NewAMule("amulecmd", "", "secret")
	first, err := a.acquireSearchGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := a.acquireSearchGate(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second gate acquisition error=%v", err)
	}
	<-first
	third, err := a.acquireSearchGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-third
}

func TestReadAMuleECPasswordHashUsesRemoteConf(t *testing.T) {
	got, err := readAMuleECPasswordHash("", "plain")
	if err != nil {
		t.Fatal(err)
	}
	digest := md5.Sum([]byte("plain"))
	if got != hex.EncodeToString(digest[:]) {
		t.Fatalf("hash=%q", got)
	}
	if _, err := readAMuleECPasswordHash("", ""); !errors.Is(err, errAMuleNoPass) {
		t.Fatalf("empty password error=%v", err)
	}
}
