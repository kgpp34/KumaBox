package types

import "testing"

func TestExecConfigValidationAndEnvironment(t *testing.T) {
	config := ExecConfig{Args: []string{"sh", "-c", "echo"}, Env: []string{"A=1", "A=2", "EMPTY="}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	environment := config.Environment()
	if environment["A"] != "2" || environment["EMPTY"] != "" {
		t.Fatalf("environment = %#v", environment)
	}
	for _, invalid := range []ExecConfig{
		{},
		{Args: []string{""}},
		{Args: []string{"echo", "bad\x00argument"}},
		{Args: []string{"env"}, Env: []string{"MISSING_VALUE"}},
		{Args: []string{"env"}, Env: []string{"=missing-key"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted invalid config %#v", invalid)
		}
	}
}
