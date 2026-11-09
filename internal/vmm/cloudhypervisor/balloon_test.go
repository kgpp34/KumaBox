package cloudhypervisor

import (
	"encoding/json"
	"testing"
)

func TestBalloonStatusAndCloneTarget(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     string
		actual     int64
		wantActual *int64
		wantReset  bool
		wantError  bool
	}{
		{name: "disabled", config: `{"memory":{"size":1073741824}}`},
		{name: "disabled null", config: `{"balloon":null}`},
		{name: "default", config: `{"memory":{"size":1073741824},"balloon":{"size":268435456}}`, actual: 805306368, wantActual: int64Pointer(268435456)},
		{name: "inflated", config: `{"memory":{"size":1073741824},"balloon":{"size":536870912}}`, actual: 805306368, wantActual: int64Pointer(268435456), wantReset: true},
		{name: "deflated", config: `{"memory":{"size":1073741824},"balloon":{"size":0}}`, wantReset: true},
		{name: "negative", config: `{"memory":{"size":1073741824},"balloon":{"size":-1}}`, wantError: true},
		{name: "exhausted", config: `{"memory":{"size":1073741824},"balloon":{"size":1073741824}}`, wantError: true},
		{name: "missing memory", config: `{"balloon":{"size":0}}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var info vmInfo
			if err := json.Unmarshal([]byte(test.config), &info.Config); err != nil {
				t.Fatal(err)
			}
			info.MemoryActualSize = test.actual
			status, err := balloonStatus(info)
			if (err != nil) != test.wantError {
				t.Fatalf("status = %+v, %v", status, err)
			}
			if !test.wantError && info.Config.Balloon == nil && status != nil {
				t.Fatalf("disabled balloon has status: %+v", status)
			}
			if !test.wantError && info.Config.Balloon != nil {
				if status.Target != info.Config.Balloon.Size || status.Default != 268435456 ||
					(status.Actual == nil) != (test.wantActual == nil) ||
					status.Actual != nil && *status.Actual != *test.wantActual {
					t.Fatalf("status = %+v", status)
				}
			}
			var config map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.config), &config); err != nil {
				t.Fatal(err)
			}
			target, err := cloneBalloonTarget(config)
			if (err != nil) != test.wantError || (target != nil) != test.wantReset {
				t.Fatalf("clone reset = %v, %v", target, err)
			}
			if target != nil && *target != 268435456 {
				t.Fatalf("clone inherited captured target: %d", *target)
			}
		})
	}
}

func int64Pointer(value int64) *int64 { return &value }
