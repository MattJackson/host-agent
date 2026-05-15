package ipmi

import (
	"context"
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
