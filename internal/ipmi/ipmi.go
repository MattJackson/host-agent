// Package ipmi wraps the three Dell-specific ipmitool raw commands the
// fan controller uses. Centralizing them ensures the bytes-on-wire
// match the bash original exactly:
//
//	manual on  : ipmitool raw 0x30 0x30 0x01 0x00
//	manual off : ipmitool raw 0x30 0x30 0x01 0x01
//	set fan    : ipmitool raw 0x30 0x30 0x02 0xff <hex-pct>
//
// "0x30 0x30" is the Dell-OEM netfn/cmd pair. Refusing to issue these
// on non-Dell BMCs is the vendor guard's whole job — wrong vendor =
// undefined-behavior writes to the BMC.
package ipmi

import (
	"context"
	"fmt"
	"strings"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

// Client issues Dell raw fan commands via a Runner.
type Client struct {
	Runner runner.Runner

	// FanIndices selects the fan-addressing dialect for SetFan.
	//
	//   nil  → BROADCAST: one write to selector 0xff ("all fans"). This is
	//          the 12th-gen+ convention (R620/R720/R730/…). Their iDRAC7+
	//          accepts 0xff and fans move together.
	//   set  → PER-INDEX: one write per fan index. 11th-gen iDRAC6
	//          (R410/R510/R610/R710) REJECTS the 0xff selector with
	//          completion code 0xCC ("Invalid data field") and requires each
	//          fan addressed individually (0x00, 0x01, …). Per-index also
	//          works on 12G, so it is the universal fallback.
	//
	// Populated by ProbeFanAddressing (or from the FAN_INDICES config key).
	FanIndices []int
}

// New returns a Client wired to r in broadcast (0xff) mode.
func New(r runner.Runner) *Client { return &Client{Runner: r} }

// MaxFanIndexProbe bounds the per-index auto-probe. Real chassis top out
// well under this (R410 = 8 fans, indices 0x00–0x07); the extra probes
// just return 0xCC and are discarded.
const MaxFanIndexProbe = 16

// Vendor reads `ipmitool mc info` and extracts the Manufacturer Name
// field. Bash:
//
//	ipmitool mc info | awk -F': ' '/Manufacturer Name/{print $2; exit}'
//
// Returns the raw string (e.g. "Dell Inc.") or "" if absent.
func (c *Client) Vendor(ctx context.Context) (string, error) {
	out, err := c.Runner.Run(ctx, "ipmitool", "mc", "info")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "Manufacturer Name") {
			continue
		}
		// "Manufacturer Name        : Dell Inc."
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		return strings.TrimSpace(line[idx+1:]), nil
	}
	return "", nil
}

// EngageManual switches the BMC to manual fan control. Must be called
// before any SetFan; otherwise the BMC overrides our SetFan with its
// own thermal policy within ~30 seconds.
//
// Callers should treat this as idempotent and re-issue it every control
// cycle. iDRAC's "third-party PCIe cooling response" silently flips the
// BMC back to auto when a non-Dell PCIe card is present, and subsequent
// SetFan calls become no-ops the controller cannot detect. Re-engaging
// each cycle is the only way to keep manual control sticky on hosts
// with third-party GPUs / HBAs.
func (c *Client) EngageManual(ctx context.Context) error {
	_, err := c.Runner.Run(ctx, "ipmitool", "raw", "0x30", "0x30", "0x01", "0x00")
	return err
}

// HandbackAuto returns fan control to the iDRAC's automatic policy.
// Call on shutdown so the box doesn't run with a stale manual setpoint
// after the container exits.
func (c *Client) HandbackAuto(ctx context.Context) error {
	_, err := c.Runner.Run(ctx, "ipmitool", "raw", "0x30", "0x30", "0x01", "0x01")
	return err
}

// SetFan commands all chassis fans to pct percent (0-100). pct is
// formatted as `0xNN` hex to match the bash original's
// `printf "0x%02x" "$pct"`. Values above 100 are passed through — the
// BMC clamps or rejects them, and the byte is still valid (0xff). A
// NEGATIVE pct is floored to 0 first: `fmt.Sprintf("0x%02x", -5)` yields
// the invalid byte "0x-5", which the BMC would reject — failing OPEN
// (fans drop) rather than safe. The controller's clamp() normally keeps
// pct in range; this is defense in depth.
func (c *Client) SetFan(ctx context.Context, pct int) error {
	if pct < 0 {
		pct = 0
	}
	// Broadcast dialect (12G+): one write to 0xff.
	if len(c.FanIndices) == 0 {
		return c.setSelector(ctx, "0xff", pct)
	}
	// Per-index dialect (11G iDRAC6): address each fan. Any single failure
	// is returned so the caller (controller) surfaces it; the remaining
	// fans have already been written this cycle and the next cycle retries
	// the whole set.
	for _, idx := range c.FanIndices {
		if err := c.setSelector(ctx, fmt.Sprintf("0x%02x", idx), pct); err != nil {
			return fmt.Errorf("fan idx 0x%02x: %w", idx, err)
		}
	}
	return nil
}

// setSelector issues one `0x30 0x30 0x02 <selector> <pct>` write. selector
// is either 0xff (all fans) or a single fan index.
func (c *Client) setSelector(ctx context.Context, selector string, pct int) error {
	_, err := c.Runner.Run(ctx, "ipmitool", "raw", "0x30", "0x30", "0x02", selector, fmt.Sprintf("0x%02x", pct))
	return err
}

// ProbeFanAddressing determines which fan-addressing dialect this BMC
// accepts, by actually issuing set-fan writes at probePct (a safe,
// mid-range value the caller is about to command anyway).
//
// Strategy:
//  1. Try the 0xff broadcast selector. If it succeeds → broadcast mode
//     (returns broadcast=true, nil indices). This is the 12G path and
//     leaves behaviour on those hosts byte-for-byte unchanged.
//  2. Otherwise probe indices 0..MaxFanIndexProbe-1 and collect every one
//     the BMC accepts → per-index mode (returns the discovered indices).
//  3. If nothing works → ok=false, and the caller drops to monitor-only.
//
// It writes to the BMC, so call it only after EngageManual and only when
// the caller intends to take fan control.
func (c *Client) ProbeFanAddressing(ctx context.Context, probePct int) (indices []int, broadcast bool, ok bool) {
	if err := c.setSelector(ctx, "0xff", probePct); err == nil {
		return nil, true, true
	}
	for i := 0; i < MaxFanIndexProbe; i++ {
		if err := c.setSelector(ctx, fmt.Sprintf("0x%02x", i), probePct); err == nil {
			indices = append(indices, i)
		}
	}
	return indices, false, len(indices) > 0
}

// VerifyIndices filters a caller-supplied index list (from the FAN_INDICES
// config key) down to those the BMC actually accepts at probePct, so a
// profile that names one index too many degrades gracefully instead of
// failing every SetFan. Returns the accepted subset.
func (c *Client) VerifyIndices(ctx context.Context, candidates []int, probePct int) []int {
	var ok []int
	for _, idx := range candidates {
		if err := c.setSelector(ctx, fmt.Sprintf("0x%02x", idx), probePct); err == nil {
			ok = append(ok, idx)
		}
	}
	return ok
}
