package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The ECS Aggregation reports its STP priority as the step (it ran at 4096
// when its inform was captured); the USW-XG16 reports the priority itself.
func TestSTPPriorityOnTheWire(t *testing.T) {
	for _, c := range []struct {
		file, model string
		priority    int
	}{
		{"inform-ecs-aggregation-uswf066.json", "USWF066", 4096},
		{"inform-usw-xg16-usxg.json", "USXG", 32768},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", c.file))
		if err != nil {
			t.Fatal(err)
		}
		var inf struct {
			STPPriority int `json:"stp_priority"`
		}
		if err := json.Unmarshal(b, &inf); err != nil {
			t.Fatal(err)
		}
		if got := stpPriorityWire(c.model, c.priority); got != inf.STPPriority {
			t.Errorf("%s at %d: sends %d, the real switch sends %d", c.model, c.priority, got, inf.STPPriority)
		}
	}
	if got := stpPriorityWire("USWF07D", 32768); got != 8 {
		t.Errorf("the ECS Core at 32768 sends %d, want 8", got)
	}
}
