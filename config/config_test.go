package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadPrecedence(t *testing.T) {
	temp := t.TempDir()
	fromEnv := filepath.Join(temp, "from-env")
	fromFlag := filepath.Join(temp, "from-flag")

	t.Run("flag wins over environment and default", func(t *testing.T) {
		t.Setenv(EnvRoot, fromEnv)
		cfg, err := Load(fromFlag)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Root != fromFlag {
			t.Errorf("Root = %q, want the flag value %q", cfg.Root, fromFlag)
		}
	})

	t.Run("environment wins over default", func(t *testing.T) {
		t.Setenv(EnvRoot, fromEnv)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Root != fromEnv {
			t.Errorf("Root = %q, want the environment value %q", cfg.Root, fromEnv)
		}
	})

	t.Run("default is used when nothing is set", func(t *testing.T) {
		t.Setenv(EnvRoot, "")
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Root != DefaultRoot {
			t.Errorf("Root = %q, want the default %q", cfg.Root, DefaultRoot)
		}
	})
}

func TestLoadRejectsRelativeRoots(t *testing.T) {
	t.Run("from the flag", func(t *testing.T) {
		t.Setenv(EnvRoot, "")
		if _, err := Load("kb"); !errors.Is(err, ErrRootNotAbsolute) {
			t.Fatalf("err = %v, want ErrRootNotAbsolute", err)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		t.Setenv(EnvRoot, "relative/root")
		if _, err := Load(""); !errors.Is(err, ErrRootNotAbsolute) {
			t.Fatalf("err = %v, want ErrRootNotAbsolute", err)
		}
	})
}

func TestLoadCleansThePath(t *testing.T) {
	t.Setenv(EnvRoot, "")
	cfg, err := Load("/var/lib/kumabox/")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Root != "/var/lib/kumabox" {
		t.Errorf("Root = %q, want the cleaned path", cfg.Root)
	}
}
