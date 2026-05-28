package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const cacheName = "cache"

// hostListener is a Listener that binds plain loopback sockets via the
// standard library. The responder treats them as a netstack from its
// perspective — they're just net.PacketConn / net.Listener.
type hostListener struct{}

func (hostListener) ListenPacket(network, address string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(context.Background(), network, address)
}

func (hostListener) Listen(network, address string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), network, address)
}

// fakeResolver records the host-resolver query and returns canned
// addresses or a canned error.
type fakeResolver struct {
	addrs []netip.Addr
	err   error

	mu       sync.Mutex
	queries  []string
	networks []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	f.queries = append(f.queries, host)
	f.networks = append(f.networks, network)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs, nil
}

func newResponderOnLoopback(t *testing.T, table InternalTable, host HostResolver) (*Responder, *net.UDPAddr) {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("udp local addr: %T", udp.LocalAddr())
	}
	_ = udp.Close()

	r, err := New(Config{
		ListenAddr: addr.String(),
		Listener:   hostListener{},
		Table:      table,
		Host:       host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, addr
}

func buildQuery(t *testing.T, id uint16, name string, qtype dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: qtype, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func sendUDP(t *testing.T, target *net.UDPAddr, query []byte) []byte {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 1500)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp[:n]
}

// parseAnswer extracts header + all answer A/AAAA addresses + the
// rcode from a wire-format response.
func parseAnswer(t *testing.T, resp []byte) (dnsmessage.Header, []netip.Addr) {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("skip questions: %v", err)
	}
	var addrs []netip.Addr
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			t.Fatalf("answer header: %v", err)
		}
		//nolint:exhaustive // only A/AAAA are answer types this responder produces; everything else is skipped.
		switch rh.Type {
		case dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				t.Fatalf("A resource: %v", err)
			}
			addrs = append(addrs, netip.AddrFrom4(a.A))
		case dnsmessage.TypeAAAA:
			a, err := p.AAAAResource()
			if err != nil {
				t.Fatalf("AAAA resource: %v", err)
			}
			addrs = append(addrs, netip.AddrFrom16(a.AAAA))
		default:
			_ = p.SkipAnswer()
		}
	}
	return h, addrs
}

func TestInternalHitReturnsSynthA(t *testing.T) {
	cacheIP := netip.MustParseAddr("10.0.0.7")
	table := MapTable{cacheName: cacheIP}
	host := &fakeResolver{err: errors.New("should not be called")}

	_, addr := newResponderOnLoopback(t, table, host)

	resp := sendUDP(t, addr, buildQuery(t, 0x1234, "cache.", dnsmessage.TypeA))
	h, addrs := parseAnswer(t, resp)

	if h.ID != 0x1234 {
		t.Errorf("ID: got %x want 1234", h.ID)
	}
	if !h.Response {
		t.Error("Response bit not set")
	}
	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v want NoError", h.RCode)
	}
	if len(addrs) != 1 || addrs[0] != cacheIP {
		t.Errorf("addrs: got %v want [%v]", addrs, cacheIP)
	}

	host.mu.Lock()
	if len(host.queries) != 0 {
		t.Errorf("host resolver called for internal name: %v", host.queries)
	}
	host.mu.Unlock()
}

func TestInternalHitMismatchedFamilyReturnsEmpty(t *testing.T) {
	// Internal entry is A but client asks for AAAA — should return
	// NOERROR with empty answer (RFC 4074), not SERVFAIL.
	table := MapTable{cacheName: netip.MustParseAddr("10.0.0.7")}
	host := &fakeResolver{err: errors.New("should not be called")}

	_, addr := newResponderOnLoopback(t, table, host)
	resp := sendUDP(t, addr, buildQuery(t, 0x55aa, "cache.", dnsmessage.TypeAAAA))
	h, addrs := parseAnswer(t, resp)
	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v want NoError", h.RCode)
	}
	if len(addrs) != 0 {
		t.Errorf("addrs: got %v want []", addrs)
	}
}

func TestExternalLookupReturnsRealA(t *testing.T) {
	resolvedIP := netip.MustParseAddr("93.184.216.34")
	host := &fakeResolver{addrs: []netip.Addr{resolvedIP}}
	_, addr := newResponderOnLoopback(t, MapTable{}, host)

	resp := sendUDP(t, addr, buildQuery(t, 0x0001, "example.com.", dnsmessage.TypeA))
	h, addrs := parseAnswer(t, resp)

	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v", h.RCode)
	}
	if len(addrs) != 1 || addrs[0] != resolvedIP {
		t.Errorf("addrs: got %v want [%v]", addrs, resolvedIP)
	}
	host.mu.Lock()
	if len(host.queries) != 1 || host.queries[0] != "example.com" || host.networks[0] != "ip4" {
		t.Errorf("host resolver invoked wrong: queries=%v networks=%v", host.queries, host.networks)
	}
	host.mu.Unlock()
}

func TestExternalLookupAAAA(t *testing.T) {
	resolvedIP := netip.MustParseAddr("2606:2800:220:1::248")
	host := &fakeResolver{addrs: []netip.Addr{resolvedIP}}
	_, addr := newResponderOnLoopback(t, MapTable{}, host)

	resp := sendUDP(t, addr, buildQuery(t, 0x0002, "example.com.", dnsmessage.TypeAAAA))
	h, addrs := parseAnswer(t, resp)
	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v", h.RCode)
	}
	if len(addrs) != 1 || addrs[0] != resolvedIP {
		t.Errorf("addrs: got %v want [%v]", addrs, resolvedIP)
	}
	host.mu.Lock()
	if host.networks[0] != "ip6" {
		t.Errorf("network: got %v want ip6", host.networks[0])
	}
	host.mu.Unlock()
}

func TestNXDOMAIN(t *testing.T) {
	host := &fakeResolver{err: &net.DNSError{Err: "no such host", IsNotFound: true}}
	_, addr := newResponderOnLoopback(t, MapTable{}, host)

	resp := sendUDP(t, addr, buildQuery(t, 0x0003, "missing.example.", dnsmessage.TypeA))
	h, addrs := parseAnswer(t, resp)
	if h.RCode != dnsmessage.RCodeNameError {
		t.Errorf("RCode: got %v want NXDOMAIN", h.RCode)
	}
	if len(addrs) != 0 {
		t.Errorf("addrs: got %v want []", addrs)
	}
}

func TestSERVFAIL(t *testing.T) {
	host := &fakeResolver{err: errors.New("upstream went away")}
	_, addr := newResponderOnLoopback(t, MapTable{}, host)

	resp := sendUDP(t, addr, buildQuery(t, 0x0004, "anywhere.example.", dnsmessage.TypeA))
	h, _ := parseAnswer(t, resp)
	if h.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("RCode: got %v want SERVFAIL", h.RCode)
	}
}

func TestMalformedPacketDoesNotCrash(t *testing.T) {
	// Send a packet that's too short to be a DNS header. The responder
	// should silently drop it. We can't easily observe "no response"
	// without timing out a read, so we send a malformed packet first,
	// then a valid one and verify the valid one still works.
	host := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("10.1.1.1")}}
	_, addr := newResponderOnLoopback(t, MapTable{}, host)

	// Garbage packet (too short)
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	// Then a valid query
	resp := sendUDP(t, addr, buildQuery(t, 0x0005, "any.example.", dnsmessage.TypeA))
	h, addrs := parseAnswer(t, resp)
	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v", h.RCode)
	}
	if len(addrs) != 1 {
		t.Errorf("addrs: got %v", addrs)
	}
}

func TestTCPPath(t *testing.T) {
	cacheIP := netip.MustParseAddr("10.0.0.42")
	host := &fakeResolver{err: errors.New("should not be called")}
	_, addr := newResponderOnLoopback(t, MapTable{cacheName: cacheIP}, host)

	conn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: addr.IP, Port: addr.Port})
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	query := buildQuery(t, 0xbeef, "cache.", dnsmessage.TypeA)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], safeUint16(t, len(query)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}

	var rlenBuf [2]byte
	if _, err := io.ReadFull(conn, rlenBuf[:]); err != nil {
		t.Fatalf("read len: %v", err)
	}
	rlen := binary.BigEndian.Uint16(rlenBuf[:])
	resp := make([]byte, rlen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("read body: %v", err)
	}

	h, addrs := parseAnswer(t, resp)
	if h.ID != 0xbeef {
		t.Errorf("ID: got %x want beef", h.ID)
	}
	if len(addrs) != 1 || addrs[0] != cacheIP {
		t.Errorf("addrs: got %v want [%v]", addrs, cacheIP)
	}
}

func TestTCPMultipleQueriesPerConn(t *testing.T) {
	cacheIP := netip.MustParseAddr("10.0.0.99")
	host := &fakeResolver{err: errors.New("should not be called")}
	_, addr := newResponderOnLoopback(t, MapTable{cacheName: cacheIP}, host)

	conn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: addr.IP, Port: addr.Port})
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	for i := range 3 {
		id := uint16(0x100 + i)
		query := buildQuery(t, id, "cache.", dnsmessage.TypeA)
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], safeUint16(t, len(query)))
		if _, err := conn.Write(lenBuf[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(query); err != nil {
			t.Fatal(err)
		}

		var rlenBuf [2]byte
		if _, err := io.ReadFull(conn, rlenBuf[:]); err != nil {
			t.Fatalf("read len iter %d: %v", i, err)
		}
		rlen := binary.BigEndian.Uint16(rlenBuf[:])
		resp := make([]byte, rlen)
		if _, err := io.ReadFull(conn, resp); err != nil {
			t.Fatalf("read body iter %d: %v", i, err)
		}
		h, addrs := parseAnswer(t, resp)
		if h.ID != id {
			t.Errorf("iter %d ID: got %x want %x", i, h.ID, id)
		}
		if len(addrs) != 1 || addrs[0] != cacheIP {
			t.Errorf("iter %d addrs: got %v", i, addrs)
		}
	}
}

// safeUint16 narrows an int to uint16 inside a test, failing loudly if
// the value would overflow. Keeps gosec G115 silent at call sites.
func safeUint16(t *testing.T, n int) uint16 {
	t.Helper()
	if n < 0 || n > 0xFFFF {
		t.Fatalf("value %d does not fit in uint16", n)
	}
	return uint16(n) //nolint:gosec // bounds-checked above
}

func TestConcurrentQueries(t *testing.T) {
	cacheIP := netip.MustParseAddr("10.5.5.5")
	host := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	_, addr := newResponderOnLoopback(t, MapTable{cacheName: cacheIP}, host)

	const N = 50
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range N {
		wg.Go(func() {
			id := uint16(i + 1) //nolint:gosec // i ∈ [0,N)
			name := "cache."
			want := cacheIP
			if i%2 == 0 {
				name = "external.example."
				want = netip.MustParseAddr("8.8.8.8")
			}
			resp := sendUDP(t, addr, buildQuery(t, id, name, dnsmessage.TypeA))
			h, addrs := parseAnswer(t, resp)
			if h.ID != id {
				return
			}
			if len(addrs) != 1 || addrs[0] != want {
				return
			}
			ok.Add(1)
		})
	}
	wg.Wait()
	if ok.Load() != N {
		t.Errorf("ok: got %d want %d", ok.Load(), N)
	}
}

func TestCanonicalize(t *testing.T) {
	cases := map[string]string{
		"cache.":        "cache",
		"Cache.":        "cache",
		"FOO.bar.":      "foo.bar",
		"already.lower": "already.lower",
		"":              "",
	}
	for in, want := range cases {
		if got := canonicalize(in); got != want {
			t.Errorf("canonicalize(%q): got %q want %q", in, got, want)
		}
	}
}

func TestMapTableLookup(t *testing.T) {
	ip := netip.MustParseAddr("10.0.0.1")
	m := MapTable{cacheName: ip}
	if got, ok := m.Lookup("cache"); !ok || got != ip {
		t.Errorf("lower: got %v ok=%v", got, ok)
	}
	if got, ok := m.Lookup("CACHE"); !ok || got != ip {
		t.Errorf("upper: got %v ok=%v", got, ok)
	}
	if _, ok := m.Lookup("missing"); ok {
		t.Error("missing should not be found")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Table: MapTable{}}); err == nil {
		t.Error("expected error when Listener is nil")
	}
	if _, err := New(Config{Listener: hostListener{}}); err == nil {
		t.Error("expected error when Table is nil")
	}
}

func TestCloseIdempotent(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("udp local addr: %T", udp.LocalAddr())
	}
	_ = udp.Close()

	r, err := New(Config{ListenAddr: addr.String(), Listener: hostListener{}, Table: MapTable{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartFailsOnBadAddr(t *testing.T) {
	r, err := New(Config{
		ListenAddr: "not:a:valid:address",
		Listener:   hostListener{},
		Table:      MapTable{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Start(context.Background()); err == nil {
		_ = r.Close()
		t.Fatal("expected Start to fail on bad address")
	}
}
