package detect

import "testing"

// The table holds the boards Apple sold with one Configuration: 29 of the
// 64 in the device list (research/09-configurations.md).
func TestTheTableHoldsTheBoardsWithOneConfiguration(t *testing.T) {
	if len(oneConfiguration) != 29 {
		t.Fatalf("the table holds %d boards, want 29", len(oneConfiguration))
	}
	for board, want := range map[string][2]int{
		"j274": {8, 8}, "j457": {8, 7}, "j316s": {10, 16}, "j416s": {12, 19}, "j514m": {14, 30}, "j516c": {16, 40},
		"j773g": {10, 10}, "j700": {6, 5}, "j716s": {18, 20}, "j873g": {12, 12},
	} {
		cpu, gpu, ok := OneConfiguration(board)
		if !ok || cpu != want[0] || gpu != want[1] {
			t.Errorf("%s: %d CPU, %d GPU, %v; want %v", board, cpu, gpu, ok, want)
		}
	}
	for _, board := range []string{"j314s", "j313", "j414c", "j416c", "j375d", "j773s", "", "j999"} {
		if _, _, ok := OneConfiguration(board); ok {
			t.Errorf("%s was sold with more than one Configuration, or is unknown, and is in the table", board)
		}
	}
}

func TestBoardIsTheFirstCompatibleOfTheDeviceTree(t *testing.T) {
	for _, c := range []struct {
		compatible []string
		want       string
	}{
		{[]string{"apple,j314s", "apple,t6000", "apple,arm-platform"}, "j314s"},
		{[]string{"apple,j416c", "apple,t6021"}, "j416c"},
		{nil, ""},
		{[]string{"qemu,virt"}, ""},
	} {
		if got := Board(c.compatible); got != c.want {
			t.Errorf("Board(%v) = %q, want %q", c.compatible, got, c.want)
		}
	}
}

func TestCoreCountsAreReadFromAKernelLog(t *testing.T) {
	const m2max = `Sep 27 00:34:30 omarchy-m2-max kernel: Booting Linux on physical CPU 0x0000000000 [0x612f0240]
Sep 27 00:34:30 omarchy-m2-max kernel: smp: Bringing up secondary CPUs ...
Sep 27 00:34:30 omarchy-m2-max kernel: smp: Brought up 1 node, 12 CPUs
Sep 27 00:34:31 omarchy-m2-max kernel: asahi 406400000.gpu:   Number of cores: 40
Sep 27 00:34:31 omarchy-m2-max kernel: asahi 406400000.gpu:   Active cores: 30
`
	if cpu, gpu := CoreCounts(m2max); cpu != 12 || gpu != 30 {
		t.Fatalf("an M2 Max's log: %d CPU, %d GPU", cpu, gpu)
	}
	const monotonic = "[    0.412345] smp: Brought up 1 node, 10 CPUs\n[    3.100000] asahi 406400000.gpu:   Active cores: 16\n"
	if cpu, gpu := CoreCounts(monotonic); cpu != 10 || gpu != 16 {
		t.Fatalf("an M1 Pro's log: %d CPU, %d GPU", cpu, gpu)
	}
	const m3air = `[    0.268877] archlinux kernel: smp: Brought up 1 node, 8 CPUs
[    2.719048] archlinux kernel: aurora 292400000.gpu: Probing...
[    2.719265] archlinux kernel: aurora 292400000.gpu: G15G: T8122 revision 2.0 identity admitted (10 active / 10 slots)
`
	if cpu, gpu := CoreCounts(m3air); cpu != 8 || gpu != 10 {
		t.Fatalf("an M3 Air's log: %d CPU, %d GPU", cpu, gpu)
	}
	if cpu, gpu := CoreCounts("[    0.4] smp: Brought up 1 node, 8 CPUs\n"); cpu != 8 || gpu != 0 {
		t.Fatalf("a log with no GPU line: %d CPU, %d GPU", cpu, gpu)
	}
	if cpu, gpu := CoreCounts("[    0.000000] Linux version 7.1\n"); cpu != 0 || gpu != 0 {
		t.Fatalf("a log with neither line: %d CPU, %d GPU", cpu, gpu)
	}
	if cpu, _ := CoreCounts("smp: Brought up 1 node, 1 CPU\n"); cpu != 1 {
		t.Fatalf("one CPU: %d", cpu)
	}
}
