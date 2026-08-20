package ipmi

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

func TestVendor_Dell(t *testing.T) {
	r := runner.NewFakeRunner()
	r.Set("ipmitool", []string{"mc", "info"}, runner.FakeResponse{
		Output: `Device ID                 : 32
Manufacturer Name         : Dell Inc.
Product Name              : iDRAC8
`,
	})
	c := New(r)
	v, err := c.Vendor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "Dell Inc." {
		t.Errorf("got %q want %q", v, "Dell Inc.")
	}
}

func TestVendor_Missing(t *testing.T) {
	r := runner.NewFakeRunner()
	c := New(r)
	v, err := c.Vendor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Errorf("expected empty vendor, got %q", v)
	}
}

func TestSetFan_HexFormat(t *testing.T) {
	r := runner.NewFakeRunner()
	c := New(r)
	cases := map[int][]string{
		0:   {"raw", "0x30", "0x30", "0x02", "0xff", "0x00"},
		15:  {"raw", "0x30", "0x30", "0x02", "0xff", "0x0f"},
		100: {"raw", "0x30", "0x30", "0x02", "0xff", "0x64"},
		255: {"raw", "0x30", "0x30", "0x02", "0xff", "0xff"},
	}
	for pct, want := range cases {
		r.Calls = nil
		if err := c.SetFan(context.Background(), pct); err != nil {
			t.Fatalf("SetFan(%d): %v", pct, err)
		}
		if len(r.Calls) != 1 || r.Calls[0].Name != "ipmitool" {
			t.Fatalf("expected single ipmitool call, got %+v", r.Calls)
		}
		if !reflect.DeepEqual(r.Calls[0].Args, want) {
			t.Errorf("SetFan(%d) args: got %v want %v", pct, r.Calls[0].Args, want)
		}
	}
}

func TestSetFan_NegativeFlooredToZero(t *testing.T) {
	// A negative pct would format as the invalid byte "0x-5"; SetFan must
	// floor it to 0 so the BMC gets a valid 0x00 (L1).
	r := runner.NewFakeRunner()
	c := New(r)
	if err := c.SetFan(context.Background(), -5); err != nil {
		t.Fatalf("SetFan(-5): %v", err)
	}
	want := []string{"raw", "0x30", "0x30", "0x02", "0xff", "0x00"}
	if len(r.Calls) != 1 || !reflect.DeepEqual(r.Calls[0].Args, want) {
		t.Errorf("SetFan(-5) args: got %+v want %v", r.Calls, want)
	}
}

func TestEngageManualAndHandback(t *testing.T) {
	r := runner.NewFakeRunner()
	c := New(r)
	_ = c.EngageManual(context.Background())
	_ = c.HandbackAuto(context.Background())
	if len(r.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(r.Calls))
	}
	if !reflect.DeepEqual(r.Calls[0].Args, []string{"raw", "0x30", "0x30", "0x01", "0x00"}) {
		t.Errorf("EngageManual: %v", r.Calls[0].Args)
	}
	if !reflect.DeepEqual(r.Calls[1].Args, []string{"raw", "0x30", "0x30", "0x01", "0x01"}) {
		t.Errorf("HandbackAuto: %v", r.Calls[1].Args)
	}
}

func TestSetFan_PerIndex_IssuesOneWritePerFan(t *testing.T) {
	r := runner.NewFakeRunner()
	c := New(r)
	c.FanIndices = []int{0, 1, 7}
	if err := c.SetFan(context.Background(), 20); err != nil {
		t.Fatalf("SetFan: %v", err)
	}
	if len(r.Calls) != 3 {
		t.Fatalf("per-index SetFan should issue 3 writes, got %d: %+v", len(r.Calls), r.Calls)
	}
	wantSel := []string{"0x00", "0x01", "0x07"}
	for i, call := range r.Calls {
		want := []string{"raw", "0x30", "0x30", "0x02", wantSel[i], "0x14"}
		if !reflect.DeepEqual(call.Args, want) {
			t.Errorf("call %d args: got %v want %v", i, call.Args, want)
		}
	}
}

func TestSetFan_PerIndex_PropagatesFailure(t *testing.T) {
	r := runner.NewFakeRunner()
	// Fan index 0x01 is rejected by the BMC.
	r.SetPrefix("ipmitool", []string{"raw", "0x30", "0x30", "0x02", "0x01"},
		runner.FakeResponse{Err: errors.New("0xcc"), ExitCode: 1})
	c := New(r)
	c.FanIndices = []int{0, 1, 2}
	if err := c.SetFan(context.Background(), 20); err == nil {
		t.Fatal("SetFan should surface a per-index write failure")
	}
}

func TestProbeFanAddressing_BroadcastWhenOxffWorks(t *testing.T) {
	r := runner.NewFakeRunner() // everything succeeds by default
	c := New(r)
	idxs, broadcast, ok := c.ProbeFanAddressing(context.Background(), 50)
	if !ok || !broadcast || idxs != nil {
		t.Fatalf("expected broadcast mode, got idxs=%v broadcast=%v ok=%v", idxs, broadcast, ok)
	}
	// Only the single 0xff probe should have run.
	if len(r.Calls) != 1 || r.Calls[0].Args[4] != "0xff" {
		t.Fatalf("broadcast probe should be one 0xff write, got %+v", r.Calls)
	}
}

func TestProbeFanAddressing_PerIndexWhenOxffRejected(t *testing.T) {
	r := runner.NewFakeRunner()
	// 11G: 0xff rejected, per-index accepted (default success).
	r.SetPrefix("ipmitool", []string{"raw", "0x30", "0x30", "0x02", "0xff"},
		runner.FakeResponse{Err: errors.New("0xcc"), ExitCode: 1})
	c := New(r)
	idxs, broadcast, ok := c.ProbeFanAddressing(context.Background(), 50)
	if !ok || broadcast {
		t.Fatalf("expected per-index mode, got broadcast=%v ok=%v", broadcast, ok)
	}
	if len(idxs) != MaxFanIndexProbe || idxs[0] != 0 {
		t.Fatalf("expected indices 0..%d, got %v", MaxFanIndexProbe-1, idxs)
	}
}

func TestVerifyIndices_DropsRejected(t *testing.T) {
	r := runner.NewFakeRunner()
	r.SetPrefix("ipmitool", []string{"raw", "0x30", "0x30", "0x02", "0x08"},
		runner.FakeResponse{Err: errors.New("0xcc"), ExitCode: 1})
	c := New(r)
	got := c.VerifyIndices(context.Background(), []int{0, 1, 8}, 50)
	if !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("VerifyIndices should drop 8, got %v", got)
	}
}
