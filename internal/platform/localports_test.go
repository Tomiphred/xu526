package platform

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestParseListeners(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 222 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0BB8 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1000        0 333 1 0000000000000000 100 0 0 10 0
`
	got := parseProcNet([]byte(tcp))
	if len(got) != 2 || got[0].ip != "127.0.0.1" || got[0].port != 3000 || got[0].inode != "111" || got[1].ip != "0.0.0.0" || got[1].port != 8080 {
		t.Errorf("proc: %+v", got)
	}
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 444 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 555 1 0000000000000000 100 0 0 10 0
`
	g6 := parseProcNet([]byte(tcp6))
	if len(g6) != 2 || g6[0].ip != "::1" || g6[0].port != 5432 || g6[1].ip != "127.0.0.1" || g6[1].port != 80 {
		t.Errorf("proc6: %+v", g6)
	}
	ns := "\r\nActive Connections\r\n\r\n  Proto  Local Address          Foreign Address        State           PID\r\n" +
		"  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1100\r\n" +
		"  TCP    127.0.0.1:5173         0.0.0.0:0              LISTENING       8812\r\n" +
		"  TCP    [::]:3000              [::]:0                 LISTENING       4420\r\n" +
		"  TCP    127.0.0.1:5173         127.0.0.1:50122        ESTABLISHED     8812\r\n"
	w := parseNetstat([]byte(ns))
	if len(w) != 3 || w[1].port != 5173 || w[1].pid != 8812 || w[2].ip != "::" {
		t.Errorf("netstat: %+v", w)
	}
	ls := parseLsof([]byte("p501\ncnode\nn127.0.0.1:3000\nn[::1]:3000\np777\ncPython\nn*:8000\n"))
	if len(ls) != 3 || ls[0].process != "node" || ls[1].ip != "::1" || ls[2].ip != "0.0.0.0" || ls[2].pid != 777 {
		t.Errorf("lsof: %+v", ls)
	}
}

func TestProbeHTTP(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><head><title> My  Vite &amp; App </title></head></html>")
	}))
	defer ln.Close()
	lp := &LocalPort{Upstream: ln.Addr().String()}
	probeHTTP(context.Background(), lp)
	if !lp.HTTP || lp.Title != "My Vite & App" {
		t.Errorf("probe %+v", lp)
	}
	raw, _ := net.Listen("tcp", "127.0.0.1:0") // accepts, never answers HTTP
	defer raw.Close()
	go func() {
		for {
			c, err := raw.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("SSH-2.0-OpenSSH\r\n"))
			c.Close()
		}
	}()
	lp2 := &LocalPort{Upstream: raw.Addr().String()}
	probeHTTP(context.Background(), lp2)
	if lp2.HTTP {
		t.Error("a non-HTTP port was taken for HTTP")
	}
	if !strings.Contains(localUpstream("0.0.0.0", 80), "127.0.0.1:80") || localUpstream("::", 5) != "[::1]:5" {
		t.Error("localUpstream")
	}
}

func TestShareLocalProgram(t *testing.T) {
	p, srv := newTestPlatform(t)
	dev, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(dev, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "host="+r.Host) }))
	defer dev.Close()
	port := dev.Addr().(*net.TCPAddr).Port

	if _, err := p.ShareLocal(context.Background(), "devapp", dev.Addr().String(), true); err != nil {
		t.Fatal(err)
	}
	body := func(host string) string {
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Host = host
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 200)
		n, _ := resp.Body.Read(b)
		return string(b[:n])
	}
	// Dev servers that check Host see their own address.
	if got := body("devapp.test"); got != fmt.Sprintf("host=localhost:%d", port) {
		t.Errorf("rewritten: %q", got)
	}
	st, _ := p.Status("devapp")
	if st.Kind != "tunnel" || st.Runtime != "external" || st.Running != 1 {
		t.Errorf("status %+v", st)
	}
	if _, err := p.ShareLocal(context.Background(), "devapp2", dev.Addr().String(), false); err != nil {
		t.Fatal(err)
	}
	if got := body("devapp2.test"); got != "host=devapp2.test" {
		t.Errorf("not rewritten: %q", got)
	}
	if _, err := p.ShareLocal(context.Background(), "nothing", "127.0.0.1:1", false); err == nil {
		t.Error("shared a port nothing listens on")
	}
	// Deleting it stops forwarding but leaves the program alone.
	if err := p.Delete("devapp"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", dev.Addr().String()); err != nil {
		t.Error("the program was stopped")
	} else {
		c.Close()
	}
}

func TestNetworkSecurity(t *testing.T) {
	for ip, want := range map[string]string{"127.0.0.1": "this computer", "::1": "this computer", "192.168.1.20": "local network",
		"10.0.0.5": "local network", "172.20.1.1": "local network", "100.101.1.2": "local network", "8.8.8.8": "internet", "2001:4860::1": "internet"} {
		if got := classifyIP(ip); got != want {
			t.Errorf("%s: %s, want %s", ip, got, want)
		}
	}
	db := NetPort{Port: 5432, Exposure: "network"}
	assessPort(&db)
	if db.Risk != "high" || len(db.Warnings) != 1 || !strings.Contains(db.Warnings[0], "PostgreSQL") {
		t.Errorf("exposed db: %+v", db)
	}
	local := NetPort{Port: 5432, Exposure: "local"}
	assessPort(&local)
	if local.Risk != "none" {
		t.Errorf("local db: %+v", local)
	}
	dev := NetPort{Port: 3000, Exposure: "local", Internet: 2}
	assessPort(&dev)
	if dev.Risk != "high" {
		t.Errorf("internet connections to an unshared port: %+v", dev)
	}
	shared := NetPort{Port: 3000, Exposure: "local", SharedAs: "site"}
	assessPort(&shared)
	if shared.Risk != "low" {
		t.Errorf("shared: %+v", shared)
	}
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1000        0 333 1 0000000000000000 100 0 0 10 0
`
	e := parseProcTCP([]byte(tcp))
	if len(e) != 1 || e[0].state != "01" || e[0].localPort != 3000 || e[0].remotePort != 54000 || e[0].remoteIP != "127.0.0.1" {
		t.Errorf("established: %+v", e)
	}
	ns := "  TCP    192.168.1.5:3000       203.0.113.9:51515      ESTABLISHED     4420\r\n  TCP    0.0.0.0:3000   0.0.0.0:0  LISTENING  4420\r\n"
	if c := parseNetstatConns([]byte(ns)); len(c) != 1 || c[0].localPort != 3000 || c[0].remote != "203.0.113.9:51515" {
		t.Errorf("netstat conns: %+v", c)
	}
	if c := parseLsofConns([]byte("p1\ncnode\nn127.0.0.1:3000->127.0.0.1:52344\n")); len(c) != 1 || c[0].localPort != 3000 {
		t.Errorf("lsof conns: %+v", c)
	}
}
