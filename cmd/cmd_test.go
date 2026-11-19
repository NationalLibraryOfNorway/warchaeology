// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestRootCmd(t *testing.T) {
	NewWarcCommand()
}

func TestLoadExplicitConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeTestConfig(t, filepath.Join(dir, "config.yaml"), "working directory")
	envFile := filepath.Join(dir, "env.yaml")
	flagFile := filepath.Join(dir, "flag.yaml")
	writeTestConfig(t, envFile, "environment")
	writeTestConfig(t, flagFile, "flag")
	t.Setenv("WARC_CONFIG", envFile)

	t.Run("environment overrides discovery", func(t *testing.T) {
		assertConfigSource(t, "", "environment")
	})
	t.Run("flag overrides environment", func(t *testing.T) {
		assertConfigSource(t, flagFile, "flag")
	})
}

func TestLoadImplicitConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("WARC_CONFIG", "")

	systemRoot := t.TempDir()
	var userRootEnv, userDir string
	userRoot := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		userRootEnv, userDir = "APPDATA", userRoot
		t.Setenv("PROGRAMDATA", systemRoot)
	case "darwin", "ios":
		userRootEnv, userDir = "HOME", filepath.Join(userRoot, "Library", "Application Support")
	case "plan9":
		userRootEnv, userDir = "home", filepath.Join(userRoot, "lib")
		t.Setenv("XDG_CONFIG_DIRS", systemRoot)
	default:
		userRootEnv, userDir = "XDG_CONFIG_HOME", userRoot
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_DIRS", systemRoot)
	}
	t.Setenv(userRootEnv, userRoot)
	writeTestConfig(t, filepath.Join(userDir, "warc", "config.yaml"), "user")
	writeTestConfig(t, filepath.Join(systemRoot, "warc", "config.yaml"), "system")
	cwdConfig := filepath.Join(dir, "config.yaml")
	writeTestConfig(t, cwdConfig, "working directory")

	t.Run("working directory first", func(t *testing.T) {
		assertConfigSource(t, "", "working directory")
	})
	if err := os.Remove(cwdConfig); err != nil {
		t.Fatal(err)
	}
	t.Run("then user directory", func(t *testing.T) {
		assertConfigSource(t, "", "user")
	})
	t.Run("unavailable user directory", func(t *testing.T) {
		t.Setenv(userRootEnv, "")
		if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
			writeTestConfig(t, cwdConfig, "working directory")
			assertConfigSource(t, "", "working directory")
			return
		}
		assertConfigSource(t, "", "system")
	})
}

func TestSystemConfigRoots(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "ios":
		if got := systemConfigRoots(); !slices.Equal(got, []string{"/Library/Application Support"}) {
			t.Errorf("system config roots = %q, want /Library/Application Support", got)
		}
	case "windows":
		root := t.TempDir()
		t.Setenv("PROGRAMDATA", root)
		if got := systemConfigRoots(); !slices.Equal(got, []string{root}) {
			t.Errorf("system config roots = %q, want %q", got, root)
		}
		t.Setenv("PROGRAMDATA", "")
		if got := systemConfigRoots(); len(got) != 0 {
			t.Errorf("system config roots = %q, want none", got)
		}
	default:
		first, second := t.TempDir(), t.TempDir()
		raw := strings.Join([]string{first, "", "relative", second}, string(os.PathListSeparator))
		t.Setenv("XDG_CONFIG_DIRS", raw)
		if got := systemConfigRoots(); !slices.Equal(got, []string{first, second}) {
			t.Errorf("system config roots = %q, want %q", got, []string{first, second})
		}
		t.Setenv("XDG_CONFIG_DIRS", "")
		if got := systemConfigRoots(); !slices.Equal(got, []string{"/etc/xdg"}) {
			t.Errorf("system config roots = %q, want /etc/xdg", got)
		}
	}
}

func writeTestConfig(t *testing.T, path, source string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test-source: "+source+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertConfigSource(t *testing.T, explicitPath, want string) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	flags := pflag.NewFlagSet("warc", pflag.ContinueOnError)
	flags.String("config", "", "")
	if explicitPath != "" {
		if err := flags.Set("config", explicitPath); err != nil {
			t.Fatal(err)
		}
	}
	if err := loadConfig(flags); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("test-source"); got != want {
		t.Errorf("config source = %q, want %q", got, want)
	}
}
