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
}

// New returns a Client wired to r.
func New(r runner.Runner) *Client { return &Client{Runner: r} }

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
// `printf "0x%02x" "$pct"`. Values outside 0-100 are passed through —
// the BMC will clamp or reject them. The controller's clamp() is
// responsible for keeping pct in range.
func (c *Client) SetFan(ctx context.Context, pct int) error {
	hex := fmt.Sprintf("0x%02x", pct)
	_, err := c.Runner.Run(ctx, "ipmitool", "raw", "0x30", "0x30", "0x02", "0xff", hex)
	return err
}
