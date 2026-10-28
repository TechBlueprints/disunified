package sonic

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Locate (docs/drivers/sonic.md §7.1, §7.5): SONiC's CLI has no LED
// command, but the platform API does -- the DX010's
// Chassis.set_status_led('green'|'off') writes
// /sys/devices/platform/leds_dx010/leds/dx010:green:stat. There is no blink
// primitive, so while the controller says locate the driver toggles the
// system status LED between off and green once a second, and leaves it
// green when locate ends. A platform whose API cannot set the LED (the VS
// raises FileNotFoundError for its chassis metadata) is logged once per run
// and only reported as locating. Hardware-untested: pmon's system-health
// daemon also sets the status LED and may fight the blink.

// ledCommand sets the chassis status LED through the platform API. The
// API returns False rather than raising when the write fails, so the exit
// status carries that too.
func ledCommand(color string) string {
	return fmt.Sprintf(`sudo python3 -c 'import sys; from sonic_platform.platform import Platform; sys.exit(0 if Platform().get_chassis().set_status_led("%s") else 1)'`, color)
}

const ledTimeout = 10 * time.Second

// setLED runs one LED write through the runner.
func (c *Collector) setLED(ctx context.Context, color string) error {
	cctx, cancel := context.WithTimeout(ctx, ledTimeout)
	defer cancel()
	_, err := c.run(cctx, ledCommand(color), "")
	return err
}

// SetLocate implements devicemodel.Locator. On: the first write (LED off)
// runs here, so a platform without LED control is found at once; then a
// goroutine toggles until SetLocate(false), a new SetLocate(true) or Close.
// Off: the blink stops and the LED is set green once.
func (c *Collector) SetLocate(ctx context.Context, on bool) error {
	c.stopBlink()
	c.mu.Lock()
	noLED := c.noLED
	c.mu.Unlock()
	if noLED {
		return nil
	}
	first := "green"
	if on {
		first = "off"
	}
	if err := c.setLED(ctx, first); err != nil {
		if ctx.Err() != nil {
			return err
		}
		c.mu.Lock()
		c.noLED = true
		c.mu.Unlock()
		c.Log.Printf("sonic %s: locate %s: this platform has no controllable status LED (%s); the switch is only reported as locating from now on", c.hostName(), onOff(on), lastLine(err.Error()))
		return nil
	}
	if !on {
		c.Log.Printf("sonic %s: locate off: status LED back to green", c.hostName())
		return nil
	}
	interval := c.BlinkInterval
	if interval <= 0 {
		interval = time.Second
	}
	bctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		lit, failed := false, false
		for {
			select {
			case <-bctx.Done():
				return
			case <-t.C:
			}
			color := "green"
			if lit {
				color = "off"
			}
			if err := c.setLED(bctx, color); err != nil {
				if bctx.Err() != nil {
					return
				}
				if !failed {
					c.Log.Printf("sonic %s: locate: status LED %s: %s (still trying)", c.hostName(), color, lastLine(err.Error()))
				}
				failed = true
				continue
			}
			lit = !lit
		}
	}()
	c.mu.Lock()
	c.blinkStop = func() { cancel(); wg.Wait() }
	c.mu.Unlock()
	c.Log.Printf("sonic %s: locate on: blinking the status LED every %s", c.hostName(), interval)
	return nil
}

// stopBlink ends a running blink and waits for its goroutine.
func (c *Collector) stopBlink() {
	c.mu.Lock()
	stop := c.blinkStop
	c.blinkStop = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (c *Collector) hostName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		// A Python traceback ends with the exception; keep that line.
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}
