package sonic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Port recovery (docs/drivers/sonic.md §11.28): the UI's Re-enable on a
// port BPDU guard or loop protection took down arrives as the controller's
// port-recovery command. disunified-l2d keeps those holds (APPL_DB
// admin_status down), so the driver only asks it: one STATE_DB row per
// interface of the port, DUI_PORT_RECOVERY|<interface>, which l2d reads
// and deletes on its next pass (2 s) and answers by releasing a hold on
// that interface or on the PortChannel it is a member of. A port nothing
// holds is logged by l2d and left alone.

// recoverCommand is the STATE_DB write for one port's interfaces.
func recoverCommand(ifs []string, at time.Time) string {
	cmds := make([]string, 0, len(ifs))
	for _, ifc := range ifs {
		cmds = append(cmds, fmt.Sprintf("sudo sonic-db-cli STATE_DB hset %s at %d >/dev/null",
			sshrun.ShellQuote("DUI_PORT_RECOVERY|"+ifc), at.Unix()))
	}
	return strings.Join(cmds, " && ")
}

// RecoverPort implements devicemodel.PortRecoverer.
func (c *Collector) RecoverPort(ctx context.Context, idx int) error {
	if !c.L2 {
		return fmt.Errorf("disunified-l2d is off on this switch (options.l2d): no port is held")
	}
	ifs := c.ifnamesOf(idx)
	if len(ifs) == 0 {
		return fmt.Errorf("no port %d", idx)
	}
	if _, err := c.run(ctx, recoverCommand(ifs, time.Now()), ""); err != nil {
		return err
	}
	c.Log.Printf("sonic %s: port %d (%s): re-enable passed to disunified-l2d", c.hostName(), idx, strings.Join(ifs, ","))
	return nil
}
