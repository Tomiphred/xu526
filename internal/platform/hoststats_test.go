package platform

import "testing"

func TestDarwinStatParsers(t *testing.T) {
	if l1, l5 := parseLoadavg("{ 1.92 2.10 2.31 }\n"); l1 != 1.92 || l5 != 2.10 {
		t.Errorf("loadavg %v %v", l1, l5)
	}
	if n := parseInt("17179869184\n"); n>>20 != 16384 {
		t.Errorf("memsize %d", n)
	}
	vm := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10000.
Pages active:                            400000.
Pages inactive:                          200000.
Pages speculative:                         5000.
Pages throttled:                              0.
Pages wired down:                        150000.
Pages purgeable:                           1000.
`
	if got, want := parseVMStat(vm), int64(10000+200000+5000+1000)*16384; got != want {
		t.Errorf("vm_stat %d, want %d", got, want)
	}
}

func TestNodeReportsPlatform(t *testing.T) {
	p, _ := newTestPlatform(t)
	n := p.Node()
	if n.OS == "" || n.Arch == "" {
		t.Fatalf("%+v", n)
	}
}
