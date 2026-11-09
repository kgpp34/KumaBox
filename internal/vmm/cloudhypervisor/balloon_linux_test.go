//go:build linux

package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
)

func TestResizeBalloonChecksLiveDeviceAndNeverResumesPausedGuest(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		device string
		target types.BalloonTarget
		code   errdefs.Code
	}{
		{name: "inflate", state: "Running", device: `{"size":268435456}`, target: types.BalloonTarget{Size: 536870913}},
		{name: "deflate", state: "Running", device: `{"size":268435456}`},
		{name: "disabled", state: "Running", device: "null", code: errdefs.CodeHostIncompatible},
		{name: "paused", state: "Paused", device: `{"size":268435456}`, code: errdefs.CodeStateConflict},
		{name: "too large", state: "Running", device: `{"size":268435456}`, target: types.BalloonTarget{Size: 1 << 30}, code: errdefs.CodeInvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan int64, 4)
			driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/vm.info":
					_, _ = fmt.Fprintf(w, `{"state":%q,"memory_actual_size":805306368,"config":{"memory":{"size":1073741824},"balloon":%s}}`, test.state, test.device)
				case "/api/v1/vm.resize":
					if r.Method != http.MethodPut {
						t.Errorf("resize method = %s", r.Method)
					}
					var payload struct {
						Size int64 `json:"desired_balloon"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					requests <- payload.Size
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected API request %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			status, err := driver.ResizeBalloon(t.Context(), plan.Process, test.target)
			if test.code != "" {
				if code, _ := errdefs.CodeOf(err); code != test.code || len(requests) != 0 {
					t.Fatalf("invalid resize = %+v, %v, writes=%d", status, err, len(requests))
				}
				return
			}
			want, _ := test.target.Resolve(1 << 30)
			if err != nil || len(requests) != 1 || <-requests != want || status.Target != want || status.Actual == nil || *status.Actual != 268435456 {
				t.Fatalf("resize = %+v, %v", status, err)
			}
		})
	}
}
