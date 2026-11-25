package memtest

import (
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "16g", want: 16 << 30},
		{in: " 16G ", want: 16 << 30},
		{in: "512m", want: 512 << 20},
		{in: "64k", want: 64 << 10},
		{in: "1000", want: 1000},
		{in: "", bad: true},
		{in: "g", bad: true},
		{in: "0g", bad: true},
		{in: "-1g", bad: true},
		{in: "1.5g", bad: true},
		{in: "16gb", bad: true},
		{in: "99999999999g", bad: true}, // overflows int64
	} {
		got, err := ParseBytes(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseBytes(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestPeakFrom(t *testing.T) {
	for _, tc := range []struct {
		name, logs string
		want       int64
	}{
		{"present", "analyzing\nMEMTEST_PEAK=2147483648\n", 2 << 30},
		{"crlf and spaces", "x\r\n  MEMTEST_PEAK=1024\r\n", 1024},
		{"last wins", "MEMTEST_PEAK=1\nMEMTEST_PEAK=2\n", 2},
		{"absent (cgroup killed)", "analyzing\nKilled\n", 0},
		{"empty value (memory.peak unreadable)", "MEMTEST_PEAK=\n", 0},
		{"garbage", "MEMTEST_PEAK=max\n", 0},
		{"not at line start", "echo MEMTEST_PEAK=5\n", 0},
		{"no trailing newline", "MEMTEST_PEAK=7", 7},
		{"empty", "", 0},
	} {
		if got := PeakFrom(tc.logs); got != tc.want {
			t.Errorf("%s: PeakFrom = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestTail(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc"},
		{"a\nb\nc", 5, "a\nb\nc"},
		{"a\nb\nc\n", 3, "a\nb\nc"},
		{"", 3, ""},
	} {
		if got := Tail(tc.s, tc.n); got != tc.want {
			t.Errorf("Tail(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestAppName(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{in: "nats-server", want: "nats-server"},
		{in: "gitea_1.22", want: "gitea_1.22"},
		{in: "my app", want: "my-app"},
		{in: "naïve", want: "na--ve"}, // each UTF-8 byte of ï becomes '-'
		{in: "a\tb", want: "a-b"},
		{in: ".", bad: true},
		{in: "..", bad: true},
		{in: "", bad: true},
		{in: "!!", bad: true},
	} {
		got, err := AppName(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("AppName(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("AppName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestAppNames(t *testing.T) {
	got, err := AppNames([]string{"nats-server", "gitea"})
	if err != nil || strings.Join(got, ",") != "nats-server,gitea" {
		t.Fatalf("AppNames = %v, %v", got, err)
	}
	// Two directories with one base name (or names that sanitise alike) share a keyspace.
	if _, err := AppNames([]string{"app", "app"}); err == nil || !strings.Contains(err.Error(), `both get app name "app"`) {
		t.Errorf("duplicate base names: err = %v", err)
	}
	if _, err := AppNames([]string{"my app", "my-app"}); err == nil {
		t.Error("names that sanitise alike: want an error")
	}
}

func TestDecide(t *testing.T) {
	const limit = 16 << 30
	peakLog := "MEMTEST_PEAK=3435973837\n" // 3.2 GiB
	for _, tc := range []struct {
		name     string
		exit     int
		oom      bool
		logs     string
		failed   bool
		wantLine string
	}{
		{"clean exit", 0, false, peakLog, false, "app: exit 0, OOMKilled=false, peak 3.2 GiB of 16.0 GiB"},
		{"cango OOM-killed, sh survives", 137, false, "MEMTEST_PEAK=17179869184\n", true,
			"app: exit 137, OOMKilled=false, peak 16.0 GiB of 16.0 GiB"},
		{"whole cgroup killed", 137, true, "analyzing\n", true, "app: exit 137, OOMKilled=true, peak unavailable of 16.0 GiB"},
		{"OOMKilled with exit 0", 0, true, peakLog, true, "app: exit 0, OOMKilled=true, peak 3.2 GiB of 16.0 GiB"},
		{"cango error", 1, false, peakLog, true, "app: exit 1, OOMKilled=false, peak 3.2 GiB of 16.0 GiB"},
	} {
		v := Decide("app", tc.exit, tc.oom, tc.logs, limit)
		if v.Failed != tc.failed || v.Line != tc.wantLine {
			t.Errorf("%s: Decide = %+v, want Failed=%v Line=%q", tc.name, v, tc.failed, tc.wantLine)
		}
	}
}
