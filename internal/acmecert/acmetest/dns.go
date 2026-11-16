package acmetest

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

// DNSZone is the zone served by DNS. Domains used with the DNS-01 challenge
// must be inside it.
const DNSZone = "example.test."

const (
	dnsListenAttempts = 10
	dnsRecordTTL      = 60
)

// DNS is an in-process authoritative DNS server for DNSZone. It is also the
// DNS provider through which the code under test publishes challenge records,
// so a record appended to it is immediately visible to the CA.
type DNS struct {
	// Addr is the host:port the server listens on, over both UDP and TCP.
	Addr string

	mu sync.Mutex
	// +checklocks:mu
	txt map[string][]string
	// +checklocks:mu
	appended int
}

// StartDNS runs a DNS server that is stopped when the test ends.
func StartDNS(t *testing.T) *DNS {
	t.Helper()

	d := &DNS{txt: map[string][]string{}}

	pc, l := listenUDPAndTCP(t)
	d.Addr = l.Addr().String()

	for _, srv := range []*dns.Server{
		{PacketConn: pc, Handler: d},
		{Listener: l, Handler: d},
	} {
		go srv.ActivateAndServe() //nolint:errcheck

		t.Cleanup(func() { srv.Shutdown() }) //nolint:errcheck
	}

	return d
}

// listenUDPAndTCP listens on the same free port for both protocols: the ACME
// client queries over UDP and the CA over TCP.
func listenUDPAndTCP(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()

	var lc net.ListenConfig

	for range dnsListenAttempts {
		pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
		require.NoError(t, err)

		l, err := lc.Listen(t.Context(), "tcp", pc.LocalAddr().String())
		if err == nil {
			return pc, l
		}

		pc.Close() //nolint:errcheck
	}

	require.FailNow(t, "unable to find a port that is free for both UDP and TCP")

	return nil, nil
}

// Appended returns the number of records that were ever published.
func (d *DNS) Appended() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.appended
}

// Published returns the number of records that are currently published.
func (d *DNS) Published() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	n := 0
	for _, values := range d.txt {
		n += len(values)
	}

	return n
}

// AppendRecords implements libdns.RecordAppender.
func (d *DNS) AppendRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, rec := range recs {
		rr := rec.RR()
		name := strings.ToLower(libdns.AbsoluteName(rr.Name, zone))

		d.txt[name] = append(d.txt[name], rr.Data)
		d.appended++
	}

	return recs, nil
}

// DeleteRecords implements libdns.RecordDeleter.
func (d *DNS) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, rec := range recs {
		rr := rec.RR()
		name := strings.ToLower(libdns.AbsoluteName(rr.Name, zone))

		var remaining []string

		for _, v := range d.txt[name] {
			if v != rr.Data {
				remaining = append(remaining, v)
			}
		}

		d.txt[name] = remaining
	}

	return recs, nil
}

// ServeDNS implements dns.Handler.
func (d *DNS) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		name := strings.ToLower(q.Name)

		switch {
		case !dns.IsSubDomain(DNSZone, name):
			m.Rcode = dns.RcodeNameError

		case q.Qtype == dns.TypeSOA && name == DNSZone:
			m.Answer = append(m.Answer, &dns.SOA{
				Hdr:     dns.RR_Header{Name: DNSZone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: dnsRecordTTL},
				Ns:      "ns." + DNSZone,
				Mbox:    "hostmaster." + DNSZone,
				Serial:  1,
				Refresh: dnsRecordTTL,
				Retry:   dnsRecordTTL,
				Expire:  dnsRecordTTL,
				Minttl:  dnsRecordTTL,
			})

		case q.Qtype == dns.TypeTXT:
			for _, v := range d.txtValues(name) {
				m.Answer = append(m.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: dnsRecordTTL},
					Txt: []string{v},
				})
			}
		}
	}

	w.WriteMsg(m) //nolint:errcheck
}

func (d *DNS) txtValues(name string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.txt[name]...)
}
